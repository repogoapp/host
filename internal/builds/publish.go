package builds

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	StatusPublishing = "publishing"
	StatusUploaded   = "uploaded"
	StatusUnknown    = "unknown"
)

var numericVersion = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,2}$`)

// Publication is one upload to App Store Connect: a fresh Release build, then
// its export. Uploaded means delivered, not reviewed or released. An empty
// BuildNumber is Xcode's to pick; the upload fills it in.
type Publication struct {
	ID           string `json:"id"`
	Project      string `json:"project"`
	Path         string `json:"path"`
	Target       string `json:"target"`
	BuildID      string `json:"build_id"`
	BundleID     string `json:"bundle_id"`
	Version      string `json:"version"`
	BuildNumber  string `json:"build_number"`
	Status       string `json:"status"`
	Phase        string `json:"phase"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
	CreatedAt    int64  `json:"created_at"`
	FinishedAt   int64  `json:"finished_at"`
}

// PublishRequest names the app the way builds.apps does. An empty
// BuildNumber lets Xcode pick the next free one when it uploads.
type PublishRequest struct {
	RequestID   string
	Project     string
	Path        string
	Target      string
	Version     string
	BuildNumber string
}

type publishRun struct {
	buildID string
	cancel  context.CancelCauseFunc
}

// Publish reserves the request id before starting, so a lost reply cannot
// duplicate delivery. App Store Connect judges the numbers at upload.
func (s *Service) Publish(req PublishRequest) (Publication, error) {
	if !idPattern.MatchString(req.RequestID) {
		return Publication{}, fmt.Errorf("%w: request_id must be 32 lowercase hexadecimal characters", ErrInvalid)
	}
	if !IOSSupported() {
		return Publication{}, ErrUnsupported
	}
	project, err := s.cfg.Paths.Contain(req.Project)
	if err != nil {
		return Publication{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.Publication(req.RequestID); err == nil {
		// An automatic build number is filled in once uploaded, so only a chosen one must match.
		if existing.Project != project || existing.Path != req.Path || existing.Target != req.Target ||
			existing.Version != req.Version || req.BuildNumber != "" && existing.BuildNumber != req.BuildNumber {
			return Publication{}, fmt.Errorf("%w: request_id belongs to a different publish request", ErrInvalid)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Publication{}, err
	}
	if !numericVersion.MatchString(req.Version) || req.BuildNumber != "" && !numericVersion.MatchString(req.BuildNumber) {
		return Publication{}, fmt.Errorf("%w: version and build_number must be one to three dot-separated integers", ErrInvalid)
	}
	prior, err := s.Publications(project, req.Path, req.Target)
	if err != nil {
		return Publication{}, err
	}
	for _, p := range prior {
		if p.Status == StatusPublishing {
			return Publication{}, fmt.Errorf("%w: %s %s is still uploading", ErrNotReady, p.Target, p.Version)
		}
	}
	p := Publication{
		ID: req.RequestID, Project: project, Path: req.Path, Target: req.Target,
		Version: req.Version, BuildNumber: req.BuildNumber,
		Status: StatusPublishing, Phase: "build", CreatedAt: time.Now().UnixMilli(),
	}
	if err := os.MkdirAll(s.publicationDir(p.ID), 0o700); err != nil {
		return Publication{}, err
	}
	if err := s.savePublication(p); err != nil {
		return Publication{}, err
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	s.publishing[p.ID] = publishRun{cancel: cancel}
	s.runs.Go(func() { s.runPublication(ctx, p) })
	return p, nil
}

func (s *Service) runPublication(ctx context.Context, p Publication) {
	defer func() {
		s.mu.Lock()
		run := s.publishing[p.ID]
		delete(s.publishing, p.ID)
		s.mu.Unlock()
		run.cancel(nil)
	}()
	logPath := filepath.Join(s.publicationDir(p.ID), "publish.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.finishPublication(&p, StatusFailed, "host_error", err.Error())
		return
	}
	defer log.Close()
	err = s.publish(ctx, &p, log)
	status, code, message := StatusUploaded, "", ""
	switch {
	case err == nil:
	case p.Phase == "upload":
		status, code, message = StatusUnknown, "upload_uncertain", "delivery could not be confirmed; check App Store Connect before uploading these numbers again"
		if refused, why := uploadRefusal(fileTail(logPath, classifyBytes), p); refused != "" {
			status, code, message = StatusFailed, refused, why
		}
	case errors.Is(context.Cause(ctx), errCanceled):
		status = StatusCanceled
	default:
		status, code, message = StatusFailed, "publish_failed", err.Error()
		if s.ctx.Err() != nil {
			code = "host_stopped"
		}
	}
	fmt.Fprintf(log, "\n==> %s\n%s\n", status, message)
	s.finishPublication(&p, status, code, message)
}

// publish builds Release with the publication's numbers, then exports that
// archive straight to App Store Connect and drops it.
func (s *Service) publish(ctx context.Context, p *Publication, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*iosTimeout)
	defer cancel()
	fmt.Fprintf(log, "==> build %s %s (%s)\n", p.Target, p.Version, cmp.Or(p.BuildNumber, "automatic"))
	b, done, err := s.start(Request{
		Project: p.Project, Platform: PlatformIOS, Path: p.Path, Target: p.Target, Configuration: "Release",
		Version: p.Version, BuildNumber: p.BuildNumber,
	}, ctx, p.ID, log)
	if err != nil {
		return err
	}
	p.BuildID = b.ID
	// Wait even after a failed save so the build stops writing before this log closes.
	if err := s.savePublication(*p); err != nil {
		_ = s.Cancel(b.ID)
		<-done
		return err
	}
	<-done
	b, err = s.Get(b.ID)
	if err != nil {
		return err
	}
	if b.Status != StatusSuccess {
		return fmt.Errorf("the build ended %s: %s", b.Status, b.ErrorMessage)
	}
	archive := filepath.Join(s.dir(b.ID), "app.xcarchive")
	defer os.RemoveAll(archive)
	p.BundleID = b.BundleID
	dir := s.publicationDir(p.ID)
	options := filepath.Join(dir, "ExportOptions.plist")
	automatic := p.BuildNumber == ""
	if err := os.WriteFile(options, []byte(publishOptions(automatic)), 0o600); err != nil {
		return err
	}
	defer os.Remove(options)
	p.Phase = "upload"
	if err := s.savePublication(*p); err != nil {
		p.Phase = "build"
		return err
	}
	fmt.Fprintln(log, "\n==> export and upload to App Store Connect")
	exportDir := filepath.Join(dir, "export")
	if err := exportArchive(ctx, dir, log, "-archivePath", archive,
		"-exportPath", exportDir, "-exportOptionsPlist", options, "-allowProvisioningUpdates").Run(); err != nil {
		return err
	}
	if automatic {
		p.BuildNumber = uploadedBuildNumber(ctx, exportDir)
		fmt.Fprintf(log, "Xcode uploaded build number %s\n", cmp.Or(p.BuildNumber, "(not reported)"))
	}
	return nil
}

// publishOptions exports for App Store Connect upload. With automatic,
// Xcode asks App Store Connect for the next free build number and uses it.
func publishOptions(automatic bool) string {
	manage := "<false/>"
	if automatic {
		manage = "<true/>"
	}
	// Xcode uses the archive's team and its own account; no credentials enter the RPC.
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>method</key><string>app-store-connect</string>
  <key>destination</key><string>upload</string>
  <key>signingStyle</key><string>automatic</string>
  <key>manageAppVersionAndBuildNumber</key>` + manage + `
  <key>testFlightInternalTestingOnly</key><false/>
  <key>uploadSymbols</key><true/>
</dict></plist>
`
}

// uploadedBuildNumber reads the build number Xcode gave the app from the
// export's DistributionSummary.plist: {"<name>.ipa": [{"buildNumber": ...}]}.
func uploadedBuildNumber(ctx context.Context, exportDir string) string {
	out, err := exec.CommandContext(ctx, plutil, "-convert", "json", "-o", "-",
		filepath.Join(exportDir, "DistributionSummary.plist")).Output()
	if err != nil {
		return ""
	}
	var summary map[string][]struct {
		BuildNumber string `json:"buildNumber"`
	}
	if json.Unmarshal(out, &summary) != nil {
		return ""
	}
	for _, apps := range summary {
		if len(apps) > 0 {
			return apps[0].BuildNumber
		}
	}
	return ""
}

// uploadRefusal recognizes App Store Connect turning the numbers down, which
// is a definite failure rather than an uncertain delivery.
func uploadRefusal(logTail string, p Publication) (code, message string) {
	tail := strings.ToLower(logTail)
	switch {
	case strings.Contains(tail, "redundant binary upload") || strings.Contains(tail, "already uploaded a build with"):
		return "build_number_used", fmt.Sprintf(
			"App Store Connect already has build %s for version %s; leave the build number on Automatic or pick a higher one",
			cmp.Or(p.BuildNumber, "this build number"), p.Version)
	case strings.Contains(tail, "is closed for new build submissions"):
		return "version_closed", fmt.Sprintf("version %s is closed for new builds in App Store Connect; raise the version", p.Version)
	case strings.Contains(tail, "must contain a higher version than that of the previously"):
		return "version_too_low", fmt.Sprintf("App Store Connect needs a version higher than %s; raise the version", p.Version)
	}
	return "", ""
}

func validateOverrides(req Request) error {
	if req.Version == "" && req.BuildNumber == "" {
		return nil
	}
	if req.Platform != PlatformIOS {
		return fmt.Errorf("%w: version overrides are supported for iOS builds only", ErrInvalid)
	}
	if req.Version != "" && !numericVersion.MatchString(req.Version) || req.BuildNumber != "" && !numericVersion.MatchString(req.BuildNumber) {
		return fmt.Errorf("%w: version/build must contain one to three dot-separated integers", ErrInvalid)
	}
	return nil
}
