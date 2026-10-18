// Package plugins runs the plugins an agent writes, for a project
// (`<project>/.repogo/plugins/<name>/app.ts`) or for every project (the
// environment's own folder), through one plugins server, and keeps the devices'
// subscriptions to their queries.
package plugins

import (
	"bytes"
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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/git"
)

// Folder is where a project keeps its plugins, from its checkout's top.
const Folder = ".repogo/plugins"

var (
	// ErrOff is a host without a plugins runtime: a dev build started without
	// REPOGO_PLUGINS_SDK.
	ErrOff = errkind.New(errkind.Unavailable, "plugins: this host was built without the plugins runtime")
	// ErrNoPlugins is a project without a .repogo/plugins folder, on an
	// environment without its own.
	ErrNoPlugins = errkind.New(errkind.NotFound, "plugins: this project has no "+Folder+" folder, and the environment has none")
	// ErrNotYours is a subscription another device made, or one that ended.
	ErrNotYours = errkind.New(errkind.NotFound, "plugins: no such subscription")
)

// The longest a crashed server waits before it is started again.
const maxRestartWait = 30 * time.Second

type Config struct {
	// Runtime is the plugins server's entry: the SDK's src/run.ts on a dev host,
	// the unpacked bundle (Unpack) on a release; empty leaves plugins off.
	Runtime string
	// Socket is where the plugins server listens; DataDir holds the plugins'
	// data, one folder per namespace a plugin names.
	Socket  string
	DataDir string
	// Global is the environment's own plugins folder, served to every project
	// under the project's own; empty leaves it off.
	Global string
	// Emit sends a subscription's new answer to the device that made it.
	Emit func(device.ID, emit.Event) error
	// Notify pushes a plugin's alert to the phones; nil drops it.
	Notify func(Notice)
	// Host runs a host method for a plugin's ctx.host; nil refuses them.
	Host func(ctx context.Context, plugin, method string, params json.RawMessage) (json.RawMessage, error)
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
	// prints is each checkout's plugins as last loaded, to reload on a change.
	prints map[string]string
}

// subscription is one device's subscription, by the id the device picked. The
// server knows it as serverID(device, id), so devices can't collide, and the
// device can end it even when its subscribe reply never reached it.
type subscription struct {
	device   device.ID
	id       string
	target   Target
	name     string
	input    json.RawMessage
	revision int
}

func serverID(d device.ID, id string) string { return string(d) + "/" + id }

// subscriptionID is a device's own name for a subscription: short, and plain.
var subscriptionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func New(ctx context.Context, cfg Config) *Service {
	return &Service{
		cfg: cfg, ctx: ctx,
		subs: map[string]*subscription{}, prints: map[string]string{},
	}
}

// On reports whether this host runs project plugins.
func (s *Service) On() bool { return s.cfg.Runtime != "" }

// Has reports whether a folder's project has plugins this host would run.
func (s *Service) Has(path string) bool {
	_, err := s.targets(path)
	return err == nil
}

// Target is what the plugins server needs: the plugins folder to run, and for
// the environment's own folder the caller's project, which is otherwise the
// folder's checkout.
type Target struct {
	Dir  string `json:"dir"`
	Root string `json:"root,omitempty"`
}

// targets is where a folder's plugins come from, the project's own first,
// then the environment's; each only when its folder exists.
func (s *Service) targets(path string) ([]Target, error) {
	if !s.On() {
		return nil, ErrOff
	}
	if !filepath.IsAbs(path) {
		return nil, errkind.ErrInvalid.Errorf("plugins: project must be an absolute path")
	}
	root := checkout(path)
	var out []Target
	if dir := filepath.Join(root, filepath.FromSlash(Folder)); isDir(dir) {
		out = append(out, Target{Dir: dir})
	}
	if isDir(s.cfg.Global) {
		out = append(out, Target{Dir: s.cfg.Global, Root: root})
	}
	if len(out) == 0 {
		return nil, ErrNoPlugins
	}
	return out, nil
}

// targetOf is the folder holding a method's plugin: the project's shadows the
// environment's. Neither having it goes to the first, whose answer is NOT_FOUND.
func (s *Service) targetOf(path, method string) (Target, error) {
	targets, err := s.targets(path)
	if err != nil {
		return Target{}, err
	}
	plugin, _, _ := strings.Cut(method, ".")
	for _, t := range targets {
		if _, err := os.Stat(filepath.Join(t.Dir, plugin, "app.ts")); err == nil {
			return t, nil
		}
	}
	return targets[0], nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return path != "" && err == nil && info.IsDir()
}

// List is the manifest of a folder's plugins (each one's `dir`, methods and input
// schemas; a project plugin hides an environment one of its name) and the pages
// that show their queries.
func (s *Service) List(ctx context.Context, path string) (Answer, error) {
	targets, err := s.targets(path)
	if err != nil {
		return Answer{}, err
	}
	plugins := []json.RawMessage{}
	seen := map[string]bool{}
	for _, t := range targets {
		answer, err := s.ask(ctx, t, "list", t)
		if err != nil || answer.Error != nil {
			return answer, err
		}
		var manifest struct {
			Plugins []map[string]any `json:"plugins"`
		}
		if err := json.Unmarshal(answer.Result, &manifest); err != nil {
			return Answer{}, fmt.Errorf("plugins: manifest of %s: %w", t.Dir, err)
		}
		for _, plugin := range manifest.Plugins {
			name, _ := plugin["name"].(string)
			if seen[name] {
				continue
			}
			seen[name] = true
			plugin["dir"] = filepath.Join(t.Dir, name)
			entry, err := json.Marshal(plugin)
			if err != nil {
				return Answer{}, err
			}
			plugins = append(plugins, entry)
		}
	}
	// Unescaped, so a page's target reads as the agent writes it: `?a=1&b=2`.
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]any{"plugins": plugins, "pages": pages(checkout(path), s.cfg.Global)}); err != nil {
		return Answer{}, err
	}
	return Answer{Result: bytes.TrimSpace(result.Bytes())}, nil
}

// Caller is who asked for a call, the plugin's ctx.caller.
type Caller string

const (
	CallerCard  Caller = "card"
	CallerAgent Caller = "agent"
)

// Call runs one method, a query or a mutation. A method with a confirm
// answers CONFIRM instead of running until confirmed says the user agreed.
func (s *Service) Call(ctx context.Context, path, name string, input json.RawMessage, confirmed bool, caller Caller) (Answer, error) {
	t, err := s.targetOf(path, name)
	if err != nil {
		return Answer{}, err
	}
	return s.ask(ctx, t, "call", callParams{Target: t, Name: name, Input: orEmpty(input), Confirmed: confirmed, Caller: caller})
}

// Subscribe keeps a device's query or stream under the device's id, replacing
// one already there; a query's changed answer, or a stream's batch of items,
// sends plugins.data. A plugin's error on the first run subscribes nothing.
func (s *Service) Subscribe(ctx context.Context, d device.ID, path, name, id string, input json.RawMessage) (Subscribed, error) {
	if !subscriptionID.MatchString(id) {
		return Subscribed{}, errkind.ErrInvalid.Errorf("plugins: a subscription id is 1 to 64 letters, digits, - or _")
	}
	t, err := s.targetOf(path, name)
	if err != nil {
		return Subscribed{}, err
	}
	srv, err := s.ready(ctx, t)
	if err != nil {
		return Subscribed{}, err
	}
	input = orEmpty(input)
	key := serverID(d, id)
	record := &subscription{device: d, id: id, target: t, name: name, input: input, revision: 1}
	s.mu.Lock()
	s.subs[key] = record
	s.mu.Unlock()

	result, pluginErr, err := srv.request(ctx, "subscribe", subscribeParams{callParams{Target: t, Name: name, Input: input}, key})
	if err != nil || pluginErr != nil {
		s.mu.Lock()
		if s.subs[key] == record {
			delete(s.subs, key)
		}
		s.mu.Unlock()
	}
	if err != nil {
		// The request may have reached the server: end it there too.
		_, _, _ = srv.request(s.ctx, "unsubscribe", map[string]string{"subscription": key})
		return Subscribed{}, err
	}
	if pluginErr != nil {
		return Subscribed{Error: pluginErr.toError()}, nil
	}
	var first struct {
		Result json.RawMessage `json:"result"`
		Stream bool            `json:"stream"`
	}
	if err := json.Unmarshal(result, &first); err != nil {
		return Subscribed{}, fmt.Errorf("plugins: unreadable subscribe reply: %w", err)
	}
	return Subscribed{Subscription: id, Revision: 1, Result: first.Result, Stream: first.Stream}, nil
}

// Unsubscribe ends one of a device's subscriptions.
func (s *Service) Unsubscribe(ctx context.Context, d device.ID, id string) error {
	key := serverID(d, id)
	s.mu.Lock()
	if _, ok := s.subs[key]; !ok {
		s.mu.Unlock()
		return ErrNotYours
	}
	delete(s.subs, key)
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		_, _, _ = srv.request(ctx, "unsubscribe", map[string]string{"subscription": key})
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

// FilesChanged reloads plugins whose code moved under a watched folder, a
// project's or the environment's own, so subscriptions show what the edited
// code answers.
func (s *Service) FilesChanged(path string, files []string) {
	for _, f := range files {
		if strings.Contains("/"+f, "/node_modules/") {
			continue
		}
		if path == s.cfg.Global && isDir(path) {
			s.reload(s.ctx, Target{Dir: s.cfg.Global})
			return
		}
		if strings.HasPrefix(f, Folder+"/") {
			if dir := filepath.Join(checkout(path), filepath.FromSlash(Folder)); isDir(dir) {
				s.reload(s.ctx, Target{Dir: dir})
			}
			return
		}
	}
}

type callParams struct {
	Target
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Confirmed bool            `json:"confirmed,omitempty"`
	Caller    Caller          `json:"caller,omitempty"`
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
		if _, _, err := srv.request(ctx, "reload", t); err != nil {
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
	if _, _, err := srv.request(ctx, "reload", t); err != nil {
		s.cfg.Log.Warn("plugins: reload failed", "dir", t.Dir, "err", err)
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
		return nil, errkind.ErrUnavailable.Errorf("plugins: install Bun (bun.sh) on this environment to run its project plugins")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	srv, err = startServer(s.ctx, bun, s.cfg.Runtime, s.cfg.Socket, s.cfg.DataDir, s.cfg.Log, s.pushed, s.notified, s.hosted, s.lost)
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

// pushed passes a subscription's news to its device.
func (s *Service) pushed(p push) {
	s.mu.Lock()
	sub, ok := s.subs[p.Subscription]
	if !ok {
		s.mu.Unlock()
		return
	}
	sub.revision++
	data := Data{Subscription: sub.id, Revision: sub.revision, Result: p.Result, Items: p.Items, Done: p.Done, Error: p.Error.toError()}
	d := sub.device
	s.mu.Unlock()
	if err := s.cfg.Emit(d, data); err != nil {
		s.cfg.Log.Debug("plugins: data push failed", "device", d, "err", err)
	}
}

// notified passes a plugin's alert on to the phones.
func (s *Service) notified(n Notice) {
	if s.cfg.Notify == nil || n.Title == "" {
		return
	}
	s.cfg.Notify(n)
}

// hosted runs a plugin's ctx.host under the host's context, its failure
// carried back in the SDK's own codes.
func (s *Service) hosted(call hostCall) (json.RawMessage, *wireError) {
	if s.cfg.Host == nil {
		return nil, &wireError{Code: "INTERNAL", Message: "plugins: this host runs no host methods for plugins"}
	}
	params := call.Params
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage("{}")
	}
	result, err := s.cfg.Host(s.ctx, call.Plugin, call.Method, params)
	if err != nil {
		return nil, &wireError{Code: sdkCode(err), Message: err.Error()}
	}
	return result, nil
}

// sdkCode is an error's kind as the SDK names it.
func sdkCode(err error) string {
	switch errkind.Of(err) {
	case errkind.Invalid:
		return "VALIDATION"
	case errkind.NotFound:
		return "NOT_FOUND"
	case errkind.Denied:
		return "FORBIDDEN"
	}
	return "INTERNAL"
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
// ids, and sends each device its answer as a push: a stream's is empty, so
// the device drops what it held as the stream starts over.
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
		data := Data{Subscription: sub.id, Error: pluginErr.toError()}
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
