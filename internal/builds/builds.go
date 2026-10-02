// Package builds turns a project's iOS or Android app into a build a phone
// can install: xcodebuild archives and exports an IPA, Gradle
// assembles APKs, and a token-guarded loopback server hands them to the
// phone's installer through a public tunnel. One resource: build artifacts.
package builds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/terminal"
)

const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"

	StatusBuilding = "building"
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"

	MethodAdHoc       = "ad_hoc"
	MethodDevelopment = "development"

	// MaxLogRead bounds one builds.log answer; the phone pages with next_offset.
	MaxLogRead = 256 << 10

	// classifyBytes is how much of a log's end a failure is classified from.
	classifyBytes = 64 << 10
)

var (
	ErrInvalid     = errkind.New(errkind.Invalid, "builds: invalid build")
	ErrNotFound    = errkind.New(errkind.NotFound, "builds: no such build")
	ErrUnsupported = errkind.New(errkind.Unavailable, "builds: iOS builds need a Mac with Xcode")
	ErrNotReady    = errkind.New(errkind.Unavailable, "builds: the build has not succeeded")
	ErrNoTunnel    = errkind.New(errkind.Unavailable, "builds: no public tunnel serves the install port")

	errCanceled = errors.New("build canceled")
	idPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Artifact is one installable file a build produced.
type Artifact struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// Build is one run, as build.json keeps it and the phone sees it. Target is
// the Xcode scheme or Gradle module; Configuration the Xcode configuration
// or Gradle variant; Method the iOS export method.
type Build struct {
	ID            string     `json:"id"`
	Platform      string     `json:"platform"`
	Project       string     `json:"project"`
	Path          string     `json:"path"`
	Target        string     `json:"target"`
	Configuration string     `json:"configuration"`
	Method        string     `json:"method"`
	Status        string     `json:"status"`
	Phase         string     `json:"phase"`
	BundleID      string     `json:"bundle_id"`
	Version       string     `json:"version"`
	BuildNumber   string     `json:"build_number"`
	SizeBytes     int64      `json:"size_bytes"`
	Artifacts     []Artifact `json:"artifacts" wire:"array"`
	ErrorCode     string     `json:"error_code"`
	ErrorMessage  string     `json:"error_message"`
	CreatedAt     int64      `json:"created_at"`
	FinishedAt    int64      `json:"finished_at"`
}

// Request is what to build; its fields are Build's.
type Request struct {
	Project       string
	Platform      string
	Path          string
	Target        string
	Configuration string
	Method        string
	Version       string
	BuildNumber   string
}

// LogChunk is part of a build's log from Offset on.
type LogChunk struct {
	Data       []byte `json:"data"`
	NextOffset int64  `json:"next_offset"`
	Done       bool   `json:"done"`
}

// failure is a pipeline's error with the code the phone shows it by.
type failure struct {
	code    string
	message string
}

func (f *failure) Error() string { return f.message }

func fail(code, format string, args ...any) *failure {
	return &failure{code: code, message: fmt.Sprintf(format, args...)}
}

type Config struct {
	// Dir holds one folder per build, the token secret and the serving port.
	Dir   string
	Paths files.Container
	// URLs is the public URL serving each local port: tunnel.Service.URLs.
	URLs func() map[int]string
	Log  *slog.Logger
}

type Service struct {
	ctx    context.Context
	cfg    Config
	secret []byte

	mu         sync.Mutex
	running    map[string]context.CancelCauseFunc
	publishing map[string]publishRun
	runs       sync.WaitGroup
	server     *server
}

// Open settles builds a stopped host left running and loads the token secret.
// Builds run on ctx, so they end with the host.
func Open(ctx context.Context, cfg Config) (*Service, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	secret, err := loadSecret(filepath.Join(cfg.Dir, "secret"))
	if err != nil {
		return nil, err
	}
	s := &Service{ctx: ctx, cfg: cfg, secret: secret, running: map[string]context.CancelCauseFunc{}, publishing: map[string]publishRun{}}
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.Status == StatusBuilding {
			s.finish(&b, fail("host_stopped", "the host stopped during the build"))
		}
	}
	if err := s.settlePublications(); err != nil {
		return nil, err
	}
	return s, nil
}

// IOSSupported reports whether this machine can archive an iOS app.
func IOSSupported() bool { return runtime.GOOS == "darwin" }

// Start validates req, records the build, and runs it detached; the answer is
// the build as it starts.
func (s *Service) Start(req Request) (Build, error) {
	b, _, err := s.start(req, s.ctx, "", nil)
	return b, err
}

func (s *Service) start(req Request, parent context.Context, publicationID string, publishLog io.Writer) (Build, <-chan struct{}, error) {
	if err := validateOverrides(req); err != nil {
		return Build{}, nil, err
	}
	project, err := s.cfg.Paths.Contain(req.Project)
	if err != nil {
		return Build{}, nil, err
	}
	b := Build{
		ID: newID(), Platform: req.Platform, Project: project, Path: req.Path, Target: req.Target,
		Configuration: req.Configuration, Method: req.Method, Status: StatusBuilding,
		Version: req.Version, BuildNumber: req.BuildNumber,
		Artifacts: []Artifact{}, CreatedAt: time.Now().UnixMilli(),
	}
	var pipeline func(context.Context, *Build, string, io.Writer) error
	switch req.Platform {
	case PlatformIOS:
		if !IOSSupported() {
			return Build{}, nil, ErrUnsupported
		}
		// A publication's build keeps its archive for the App Store Connect export.
		pipeline, err = s.checkIOS(&b, publicationID != "")
	case PlatformAndroid:
		pipeline, err = s.checkAndroid(&b)
	default:
		return Build{}, nil, fmt.Errorf("%w: platform must be %q or %q", ErrInvalid, PlatformIOS, PlatformAndroid)
	}
	if err != nil {
		return Build{}, nil, err
	}

	dir := s.dir(b.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Build{}, nil, err
	}
	if err := s.save(b); err != nil {
		return Build{}, nil, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	s.mu.Lock()
	s.running[b.ID] = cancel
	if publicationID != "" {
		run := s.publishing[publicationID]
		run.buildID = b.ID
		s.publishing[publicationID] = run
	}
	s.mu.Unlock()
	if publishLog != nil {
		buildPipeline := pipeline
		pipeline = func(ctx context.Context, b *Build, dir string, log io.Writer) error {
			return buildPipeline(ctx, b, dir, io.MultiWriter(log, publishLog))
		}
	}
	done := make(chan struct{})
	s.runs.Go(func() {
		defer close(done)
		s.run(ctx, b, pipeline)
	})
	return b, done, nil
}

func (s *Service) run(ctx context.Context, b Build, pipeline func(context.Context, *Build, string, io.Writer) error) {
	defer func() {
		s.mu.Lock()
		cancel := s.running[b.ID]
		delete(s.running, b.ID)
		s.mu.Unlock()
		cancel(nil)
	}()
	dir := s.dir(b.ID)
	log, err := os.OpenFile(filepath.Join(dir, "build.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.finish(&b, fail("host_error", "open the build log: %v", err))
		return
	}
	defer log.Close()

	err = pipeline(ctx, &b, dir, log)
	switch {
	case errors.Is(context.Cause(ctx), errCanceled):
		b.Status = StatusCanceled
		s.finish(&b, nil)
	case s.ctx.Err() != nil:
		s.finish(&b, fail("host_stopped", "the host stopped during the build"))
	case err != nil:
		var f *failure
		if !errors.As(err, &f) {
			f = fail("host_error", "%v", err)
		}
		s.finish(&b, f)
	default:
		b.Status = StatusSuccess
		s.finish(&b, nil)
		s.pruneDuplicates(b)
	}
	s.cfg.Log.Info("build finished", "build", b.ID, "platform", b.Platform, "status", b.Status, "error", b.ErrorCode)
}

// finish records how a build ended; a failure sets failed and its reason.
func (s *Service) finish(b *Build, f *failure) {
	if f != nil {
		b.Status, b.ErrorCode, b.ErrorMessage = StatusFailed, f.code, f.message
	}
	b.Phase = ""
	b.FinishedAt = time.Now().UnixMilli()
	if err := s.save(*b); err != nil {
		s.cfg.Log.Warn("builds: saving a finished build failed", "build", b.ID, "err", err)
	}
}

// phase records the step a running build is on, so a list shows it.
func (s *Service) phase(b *Build, phase string, log io.Writer) {
	b.Phase = phase
	fmt.Fprintf(log, "\n==> %s\n", phase)
	if err := s.save(*b); err != nil {
		s.cfg.Log.Warn("builds: saving a build's phase failed", "build", b.ID, "err", err)
	}
}

// Cancel stops a running build; it ends canceled.
func (s *Service) Cancel(id string) error {
	s.mu.Lock()
	cancel := s.running[id]
	s.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%w: %s is not running", ErrNotFound, id)
	}
	cancel(errCanceled)
	return nil
}

// Running is how many builds are going, for an update's busy check.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running) + len(s.publishing)
}

// Close waits for the builds the host's context ended, then stops serving.
func (s *Service) Close() {
	s.runs.Wait()
	s.mu.Lock()
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		srv.close(s.ctx)
	}
}

// List is a project's builds, or every build when project is empty, newest first.
func (s *Service) List(project string) ([]Build, error) {
	if project != "" {
		var err error
		if project, err = s.cfg.Paths.Contain(project); err != nil {
			return nil, err
		}
	}
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	out := []Build{}
	for _, b := range all {
		if project == "" || b.Project == project {
			out = append(out, b)
		}
	}
	return out, nil
}

// Get is one build.
func (s *Service) Get(id string) (Build, error) {
	if !idPattern.MatchString(id) {
		return Build{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	var b Build
	found, err := apphome.ReadJSON(filepath.Join(s.dir(id), "build.json"), &b)
	if err != nil {
		return Build{}, err
	}
	if !found {
		return Build{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return b, nil
}

// Delete removes a finished build and its files; its install links die with them.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.Get(id)
	if err != nil {
		return err
	}
	if b.Status == StatusBuilding {
		return fmt.Errorf("%w: cancel the build before deleting it", ErrInvalid)
	}
	for _, run := range s.publishing {
		if run.buildID == id {
			return fmt.Errorf("%w: wait for publishing to finish before deleting the build", ErrInvalid)
		}
	}
	return os.RemoveAll(s.dir(id))
}

// Log reads a build's log from offset; Done is set once the build ended and
// the read reached the end.
func (s *Service) Log(id string, offset int64) (LogChunk, error) {
	b, err := s.Get(id)
	if err != nil {
		return LogChunk{}, err
	}
	return readLog(filepath.Join(s.dir(id), "build.log"), offset, b.Status != StatusBuilding)
}

func readLog(path string, offset int64, finished bool) (LogChunk, error) {
	if offset < 0 {
		return LogChunk{}, fmt.Errorf("%w: offset must not be negative", ErrInvalid)
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return LogChunk{Data: []byte{}, NextOffset: offset, Done: finished}, nil
	}
	if err != nil {
		return LogChunk{}, err
	}
	defer f.Close()
	data := make([]byte, MaxLogRead)
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return LogChunk{}, err
	}
	return LogChunk{
		Data: data[:n], NextOffset: offset + int64(n),
		Done: finished && n < MaxLogRead,
	}, nil
}

// pruneDuplicates keeps only the newest finished build of the same app, target
// and configuration; a different configuration is a different build.
func (s *Service) pruneDuplicates(current Build) {
	all, err := s.all()
	if err != nil {
		return
	}
	for _, b := range all {
		if b.ID == current.ID || b.Status == StatusBuilding || b.Platform != current.Platform ||
			b.Project != current.Project || b.Path != current.Path || b.Target != current.Target ||
			b.Configuration != current.Configuration || b.Method != current.Method {
			continue
		}
		if err := s.Delete(b.ID); err != nil && !errors.Is(err, ErrInvalid) {
			s.cfg.Log.Warn("builds: pruning a superseded build failed", "build", b.ID, "err", err)
		}
	}
}

// all is every recorded build, newest first; an unreadable folder is skipped.
func (s *Service) all() ([]Build, error) {
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []Build
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		if b, err := s.Get(e.Name()); err == nil {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

func (s *Service) save(b Build) error {
	return apphome.WriteJSON(filepath.Join(s.dir(b.ID), "build.json"), b, 0o600)
}

func (s *Service) dir(id string) string { return filepath.Join(s.cfg.Dir, id) }

// LogTail is the last size bytes of a build's log.
func (s *Service) LogTail(id string, size int64) string {
	if !idPattern.MatchString(id) {
		return ""
	}
	return logTail(s.dir(id), size)
}

// logTail is the end of a build's log, which a failure is classified from.
func logTail(dir string, size int64) string {
	return fileTail(filepath.Join(dir, "build.log"), size)
}

// fileTail is the last size bytes of the file at path, or "" when it can't be read.
func fileTail(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-size, 0)
	buf := make([]byte, info.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	return string(buf[:n])
}

// command runs name through the user's login shell, so xcodebuild and Gradle
// see the PATH, JAVA_HOME and ANDROID_HOME the user's own terminal would.
func command(ctx context.Context, dir string, log io.Writer, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, terminal.LoginShell(), append([]string{"-l", "-c", `exec "$0" "$@"`, name}, args...)...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = log, log
	// Its own process group, so a cancel reaches the compilers xcodebuild and Gradle spawn.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
