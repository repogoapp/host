// Package plugins runs the plugins an agent writes for a project
// (`<project>/.repogo/plugins/<name>/app.ts`) through one plugins server, and keeps
// the devices' subscriptions to their queries.
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
	"github.com/repogoapp/host/internal/mcp"
)

// Folder is where a project keeps its plugins, from its checkout's top.
const Folder = ".repogo/plugins"

var (
	// ErrOff is a host without the plugins SDK: a release, or a dev host started
	// without REPOGO_PLUGINS_SDK.
	ErrOff = errkind.New(errkind.Unavailable, "plugins: project plugins are a development feature on this host")
	// ErrNoPlugins is a project without a .repogo/plugins folder.
	ErrNoPlugins = errkind.New(errkind.NotFound, "plugins: this project has no "+Folder+" folder")
	// ErrNotYours is a subscription another device made, or one that ended.
	ErrNotYours = errkind.New(errkind.NotFound, "plugins: no such subscription")
)

// The longest a crashed server waits before it is started again.
const maxRestartWait = 30 * time.Second

type Config struct {
	// SDK is the plugins SDK's folder (its src/run.ts); empty leaves plugins off.
	SDK string
	// Socket is where the plugins server listens; DataDir holds every project's
	// databases, one folder per project.
	Socket  string
	DataDir string
	// Emit sends a subscription's new answer to the device that made it.
	Emit func(device.ID, emit.Event) error
	Log  *slog.Logger
}

type Service struct {
	cfg Config
	// ctx is the host's: the plugins server runs under it, not under a request.
	ctx context.Context

	// startMu makes starting the server one caller's job.
	startMu sync.Mutex

	mu       sync.Mutex
	server   *server
	restarts int
	subs     map[string]*subscription
	nextSub  int
	// prints is each checkout's plugins as last loaded, to reload on a change.
	prints map[string]string
}

// subscription is one device's subscription. The server knows it by the same
// id, which the host picks, so a push never arrives before its record.
type subscription struct {
	device   device.ID
	target   Target
	name     string
	input    json.RawMessage
	revision int
}

func New(ctx context.Context, cfg Config) *Service {
	return &Service{
		cfg: cfg, ctx: ctx,
		subs: map[string]*subscription{}, prints: map[string]string{},
	}
}

// On reports whether this host runs project plugins.
func (s *Service) On() bool { return s.cfg.SDK != "" }

// Has reports whether a folder's project has plugins this host would run.
func (s *Service) Has(path string) bool {
	_, err := s.target(path)
	return err == nil
}

// Target is what the plugins server needs for a folder: its project's data key
// and the plugins folder of the checkout it is in.
type Target struct {
	Project string `json:"project"`
	Dir     string `json:"dir"`
}

func (s *Service) target(path string) (Target, error) {
	if !s.On() {
		return Target{}, ErrOff
	}
	if !filepath.IsAbs(path) {
		return Target{}, errkind.ErrInvalid.Errorf("plugins: project must be an absolute path")
	}
	dir := filepath.Join(checkout(path), filepath.FromSlash(Folder))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return Target{}, ErrNoPlugins
	}
	return Target{Project: projectKey(path), Dir: dir}, nil
}

// List is the manifest of a folder's plugins: each plugin, its methods and their input schemas.
func (s *Service) List(ctx context.Context, path string) (Answer, error) {
	t, err := s.target(path)
	if err != nil {
		return Answer{}, err
	}
	return s.ask(ctx, t, "list", t)
}

// Call runs one method, a query or a mutation.
func (s *Service) Call(ctx context.Context, path, name string, input json.RawMessage) (Answer, error) {
	t, err := s.target(path)
	if err != nil {
		return Answer{}, err
	}
	return s.ask(ctx, t, "call", callParams{Target: t, Name: name, Input: orEmpty(input)})
}

// Subscribe runs a query for a device and keeps it: each time a write changes
// its answer, the device gets plugins.data. A plugin's error on the first run
// subscribes nothing.
func (s *Service) Subscribe(ctx context.Context, d device.ID, path, name string, input json.RawMessage) (Subscribed, error) {
	t, err := s.target(path)
	if err != nil {
		return Subscribed{}, err
	}
	srv, err := s.ready(ctx, t)
	if err != nil {
		return Subscribed{}, err
	}
	input = orEmpty(input)
	s.mu.Lock()
	s.nextSub++
	id := fmt.Sprintf("a%d", s.nextSub)
	s.subs[id] = &subscription{device: d, target: t, name: name, input: input, revision: 1}
	s.mu.Unlock()

	result, pluginErr, err := srv.request(ctx, "subscribe", subscribeParams{callParams{Target: t, Name: name, Input: input}, id})
	if err != nil || pluginErr != nil {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
	if err != nil {
		return Subscribed{}, err
	}
	if pluginErr != nil {
		return Subscribed{Error: pluginErr.toError()}, nil
	}
	var first struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(result, &first); err != nil {
		return Subscribed{}, fmt.Errorf("plugins: unreadable subscribe reply: %w", err)
	}
	return Subscribed{Subscription: id, Revision: 1, Result: first.Result}, nil
}

// Unsubscribe ends one of a device's subscriptions.
func (s *Service) Unsubscribe(ctx context.Context, d device.ID, id string) error {
	s.mu.Lock()
	sub, ok := s.subs[id]
	if !ok || sub.device != d {
		s.mu.Unlock()
		return ErrNotYours
	}
	delete(s.subs, id)
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		_, _, _ = srv.request(ctx, "unsubscribe", map[string]string{"subscription": id})
	}
	return nil
}

// Leave ends every subscription a device holds: it disconnected, and
// subscribes again when it comes back.
func (s *Service) Leave(d device.ID) {
	s.mu.Lock()
	var ended []string
	for id, sub := range s.subs {
		if sub.device == d {
			ended = append(ended, id)
			delete(s.subs, id)
		}
	}
	srv := s.server
	s.mu.Unlock()
	if srv == nil {
		return
	}
	for _, id := range ended {
		_, _, _ = srv.request(s.ctx, "unsubscribe", map[string]string{"subscription": id})
	}
}

// FilesChanged reloads a project's plugins when their code moved under a watched
// folder, so subscriptions show what the edited code answers.
func (s *Service) FilesChanged(path string, files []string) {
	for _, f := range files {
		if strings.HasPrefix(f, Folder+"/") && !strings.Contains(f, "/node_modules/") {
			if t, err := s.target(path); err == nil {
				s.reload(s.ctx, t)
			}
			return
		}
	}
}

type callParams struct {
	Target
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type subscribeParams struct {
	callParams
	Subscription string `json:"subscription"`
}

func (s *Service) ask(ctx context.Context, t Target, method string, params any) (Answer, error) {
	srv, err := s.ready(ctx, t)
	if err != nil {
		return Answer{}, err
	}
	result, pluginErr, err := srv.request(ctx, method, params)
	if err != nil {
		return Answer{}, err
	}
	if pluginErr != nil {
		return Answer{Error: pluginErr.toError()}, nil
	}
	return Answer{Result: result}, nil
}

// ready is the running server, started if need be, with t's plugins current: an
// agent that edits a plugin and calls it in the next breath gets the new code.
func (s *Service) ready(ctx context.Context, t Target) (*server, error) {
	srv, err := s.running(ctx)
	if err != nil {
		return nil, err
	}
	current := fingerprint(t.Dir)
	s.mu.Lock()
	last, seen := s.prints[t.Dir]
	s.prints[t.Dir] = current
	s.mu.Unlock()
	if seen && last != current {
		if _, _, err := srv.request(ctx, "reload", map[string]string{"project": t.Project}); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

func (s *Service) reload(ctx context.Context, t Target) {
	s.mu.Lock()
	srv := s.server
	s.prints[t.Dir] = fingerprint(t.Dir)
	s.mu.Unlock()
	if srv == nil {
		return
	}
	if _, _, err := srv.request(ctx, "reload", map[string]string{"project": t.Project}); err != nil {
		s.cfg.Log.Warn("plugins: reload failed", "project", t.Project, "err", err)
	}
}

func (s *Service) running(ctx context.Context) (*server, error) {
	s.mu.Lock()
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		return srv, nil
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	srv = s.server
	s.mu.Unlock()
	if srv != nil {
		return srv, nil
	}
	bun, err := bunPath()
	if err != nil {
		return nil, errkind.ErrUnavailable.Errorf("plugins: Bun is not installed on this environment")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	srv, err = startServer(s.ctx, bun, s.cfg.SDK, s.cfg.Socket, s.cfg.DataDir, s.cfg.Log, s.pushed, s.lost)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.server = srv
	// Code is loaded afresh by the new server.
	clear(s.prints)
	s.mu.Unlock()
	s.cfg.Log.Info("plugins: server started", "socket", s.cfg.Socket)
	return srv, nil
}

// pushed passes a subscription's new answer to its device.
func (s *Service) pushed(p push) {
	s.mu.Lock()
	sub, ok := s.subs[p.Subscription]
	if !ok {
		s.mu.Unlock()
		return
	}
	sub.revision++
	data := Data{Subscription: p.Subscription, Revision: sub.revision, Result: p.Result, Error: p.Error.toError()}
	d := sub.device
	s.mu.Unlock()
	if err := s.cfg.Emit(d, data); err != nil {
		s.cfg.Log.Debug("plugins: data push failed", "device", d, "err", err)
	}
}

// lost is the server's connection ending. Subscriptions are taken up again on
// a new server, after a wait that grows with each crash in a row.
func (s *Service) lost(srv *server) {
	s.mu.Lock()
	if s.server != srv {
		s.mu.Unlock()
		return
	}
	s.server = nil
	s.restarts++
	wait := min(time.Duration(s.restarts)*time.Second, maxRestartWait)
	held := len(s.subs) > 0
	s.mu.Unlock()
	s.cfg.Log.Warn("plugins: server stopped", "subscriptions", held)
	if !held || s.ctx.Err() != nil {
		return
	}
	go func() {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
		s.resubscribe()
	}()
}

// resubscribe takes every subscription up on a new server, under the same
// ids, and sends each device its answer as a push.
func (s *Service) resubscribe() {
	srv, err := s.running(s.ctx)
	if err != nil {
		s.cfg.Log.Warn("plugins: restart failed", "err", err)
		return
	}
	s.mu.Lock()
	s.restarts = 0
	subs := map[string]subscription{}
	for id, sub := range s.subs {
		subs[id] = *sub
	}
	s.mu.Unlock()
	for id, sub := range subs {
		params := subscribeParams{callParams{Target: sub.target, Name: sub.name, Input: sub.input}, id}
		result, pluginErr, err := srv.request(s.ctx, "subscribe", params)
		if err != nil {
			return
		}
		data := Data{Subscription: id, Error: pluginErr.toError()}
		if pluginErr == nil {
			var first struct {
				Result json.RawMessage `json:"result"`
			}
			_ = json.Unmarshal(result, &first)
			data.Result = first.Result
		}
		s.mu.Lock()
		live, ok := s.subs[id]
		if ok {
			live.revision++
			data.Revision = live.revision
		}
		s.mu.Unlock()
		if ok {
			_ = s.cfg.Emit(sub.device, data)
		}
	}
}

func orEmpty(input json.RawMessage) json.RawMessage {
	if len(input) == 0 || string(input) == "null" {
		return json.RawMessage("{}")
	}
	return input
}

// checkout is the git checkout a folder is in, a linked worktree included, so
// an agent can change a plugin on a branch without touching main's.
func checkout(path string) string {
	if root := git.Root(path); root != "" {
		return root
	}
	return filepath.Clean(path)
}

// projectKey names a project's data folder: every worktree of one project
// shares it, as MCP switches do.
func projectKey(path string) string {
	root := mcp.Root(path)
	sum := sha256.Sum256([]byte(root))
	return filepath.Base(root) + "-" + hex.EncodeToString(sum[:4])
}

// fingerprint is the plugins' source as of now: each file's path, size and time.
func fingerprint(dir string) string {
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() && e.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if info, err := e.Info(); err == nil && !e.IsDir() {
			fmt.Fprintf(h, "%s %d %d\n", path, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// bunPath finds Bun on PATH, then where its installer puts it: a host started
// at login may not have the shell's PATH.
func bunPath() (string, error) {
	if path, err := exec.LookPath("bun"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".bun", "bin", "bun")
	if _, err := os.Stat(path); err != nil {
		return "", errors.New("bun not found")
	}
	return path, nil
}
