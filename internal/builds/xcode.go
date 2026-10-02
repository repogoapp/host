package builds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	iosTimeout      = 60 * time.Minute
	settingsTimeout = 2 * time.Minute
	ipaName         = "app.ipa"
)

// Tools the iOS pipeline runs, by absolute path: a login shell's PATH may not
// hold them. Variables so a test can put fakes in their place.
var (
	xcodebuild = "/usr/bin/xcodebuild"
	plutil     = "/usr/bin/plutil"
)

var bundleIDPattern = regexp.MustCompile(`PRODUCT_BUNDLE_IDENTIFIER = ([A-Za-z0-9.\-]+);`)

// preferWorkspaces drops a project whose workspace sits beside it (the
// CocoaPods layout): the workspace is what builds.
func preferWorkspaces(bundles []string) []string {
	workspaces := map[string]bool{}
	for _, b := range bundles {
		if filepath.Ext(b) == ".xcworkspace" {
			workspaces[strings.TrimSuffix(b, ".xcworkspace")] = true
		}
	}
	var out []string
	for _, b := range bundles {
		if filepath.Ext(b) == ".xcworkspace" || !workspaces[strings.TrimSuffix(b, ".xcodeproj")] {
			out = append(out, b)
		}
	}
	return shallowestFirst(out)
}

// xcodeApp reads schemes off disk rather than asking xcodebuild, which costs
// seconds per project in a picker.
func xcodeApp(root, bundle string) App {
	name := strings.TrimSuffix(filepath.Base(bundle), filepath.Ext(bundle))
	schemes := map[string]bool{}
	containers := []string{bundle}
	if filepath.Ext(bundle) == ".xcworkspace" {
		containers = append(containers, workspaceProjects(bundle)...)
	}
	for _, c := range containers {
		collectSchemes(c, schemes)
	}
	targets := make([]string, 0, len(schemes))
	for s := range schemes {
		targets = append(targets, s)
	}
	sort.Strings(targets)
	configurations := []string{"Release", "Debug"}
	return App{
		Platform: PlatformIOS, Path: relative(root, bundle), Name: name, BundleID: projectBundleID(containers),
		Targets: targets, DefaultTarget: pick(targets, name, ""),
		Configurations: configurations, DefaultConfiguration: configurations[0],
	}
}

func collectSchemes(container string, into map[string]bool) {
	dirs := []string{filepath.Join(container, "xcshareddata", "xcschemes")}
	users, _ := filepath.Glob(filepath.Join(container, "xcuserdata", "*.xcuserdatad", "xcschemes"))
	for _, dir := range append(dirs, users...) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.xcscheme"))
		for _, m := range matches {
			into[strings.TrimSuffix(filepath.Base(m), ".xcscheme")] = true
		}
	}
}

// workspaceProjects is the .xcodeproj bundles a workspace references.
func workspaceProjects(workspace string) []string {
	data, err := os.ReadFile(filepath.Join(workspace, "contents.xcworkspacedata"))
	if err != nil {
		return nil
	}
	var out []string
	for _, chunk := range strings.Split(string(data), `location = "`)[1:] {
		location, _, _ := strings.Cut(chunk, `"`)
		for _, prefix := range []string{"group:", "container:", "self:", "absolute:"} {
			location = strings.TrimPrefix(location, prefix)
		}
		if strings.HasSuffix(location, ".xcodeproj") {
			out = append(out, filepath.Join(filepath.Dir(workspace), location))
		}
	}
	return out
}

// projectBundleID is for display; the build reads the real one off the archived app.
func projectBundleID(containers []string) string {
	for _, c := range containers {
		data, err := os.ReadFile(filepath.Join(c, "project.pbxproj"))
		if err != nil {
			continue
		}
		if m := bundleIDPattern.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// checkIOS validates an iOS request, filling its defaults, and returns its pipeline.
func (s *Service) checkIOS(b *Build, forUpload bool) (func(context.Context, *Build, string, io.Writer) error, error) {
	bundle, err := s.cfg.Paths.Contain(filepath.Join(b.Project, b.Path))
	if err != nil {
		return nil, err
	}
	if ext := filepath.Ext(bundle); ext != ".xcodeproj" && ext != ".xcworkspace" {
		return nil, fmt.Errorf("%w: path must be an .xcodeproj or .xcworkspace", ErrInvalid)
	}
	if b.Target == "" {
		return nil, fmt.Errorf("%w: target (the scheme) is required", ErrInvalid)
	}
	if b.Configuration == "" {
		b.Configuration = "Release"
	}
	if b.Method == "" {
		b.Method = MethodAdHoc
	}
	if b.Method != MethodAdHoc && b.Method != MethodDevelopment {
		return nil, fmt.Errorf("%w: method must be %q or %q", ErrInvalid, MethodAdHoc, MethodDevelopment)
	}
	// A Debug build is signed for development, which needs Developer Mode on the phone.
	if strings.EqualFold(b.Configuration, "Debug") {
		b.Method = MethodDevelopment
	}
	return func(ctx context.Context, b *Build, dir string, log io.Writer) error {
		return s.archive(ctx, b, bundle, dir, log, forUpload)
	}, nil
}

// archive reads the team, archives for any iOS device, exports a signed IPA,
// and keeps it with the app's icons. Only the phones already on the team's
// profile can install it; no device is registered.
func (s *Service) archive(ctx context.Context, b *Build, bundle, dir string, log io.Writer, forUpload bool) error {
	wantedVersion, wantedBuild := b.Version, b.BuildNumber
	ctx, cancel := context.WithTimeout(ctx, iosTimeout)
	defer cancel()
	container := "-project"
	if filepath.Ext(bundle) == ".xcworkspace" {
		container = "-workspace"
	}
	cwd := filepath.Dir(bundle)

	s.phase(b, "settings", log)
	settingsCtx, cancelSettings := context.WithTimeout(ctx, settingsTimeout)
	defer cancelSettings()
	settingsCmd := command(settingsCtx, cwd, nil, xcodebuild, "-showBuildSettings", "-json", container, bundle,
		"-scheme", b.Target, "-configuration", b.Configuration, "-destination", "generic/platform=iOS")
	settingsCmd.Stderr = log
	out, err := settingsCmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(classifyArchive(logTail(dir, classifyBytes)), "xcodebuild could not read the build settings of %q: %v", b.Target, err)
	}
	team := developmentTeam(out)
	if team == "" {
		return fail("team_not_selected", "scheme %q has no development team; choose one in Xcode's Signing & Capabilities", b.Target)
	}

	s.phase(b, "archive", log)
	archivePath := filepath.Join(dir, "app.xcarchive")
	keepArchive := false
	defer func() {
		if !keepArchive {
			_ = os.RemoveAll(archivePath)
		}
	}()
	args := []string{container, bundle, "-scheme", b.Target,
		"-configuration", b.Configuration, "-destination", "generic/platform=iOS",
		"-archivePath", archivePath, "-hideShellScriptEnvironment", "archive", "-allowProvisioningUpdates"}
	if wantedVersion != "" {
		args = append(args, "MARKETING_VERSION="+wantedVersion)
	}
	if wantedBuild != "" {
		args = append(args, "CURRENT_PROJECT_VERSION="+wantedBuild)
	}
	if err := command(ctx, cwd, log, xcodebuild, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(classifyArchive(logTail(dir, classifyBytes)), "xcodebuild archive failed: %v", err)
	}

	s.phase(b, "export", log)
	exportDir := filepath.Join(dir, "export")
	defer os.RemoveAll(exportDir)
	options := filepath.Join(dir, "ExportOptions.plist")
	defer os.Remove(options)
	if err := os.WriteFile(options, []byte(exportOptions(team, b.Method)), 0o600); err != nil {
		return err
	}
	if err := command(ctx, cwd, log, xcodebuild, "-exportArchive", "-archivePath", archivePath,
		"-exportPath", exportDir, "-exportOptionsPlist", options, "-allowProvisioningUpdates").Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(classifyExport(logTail(dir, classifyBytes)), "xcodebuild -exportArchive failed: %v", err)
	}

	app := firstWithExt(filepath.Join(archivePath, "Products", "Applications"), ".app")
	if app == "" {
		return fail("export_failed", "the archive holds no .app")
	}
	if !isFile(filepath.Join(app, "embedded.mobileprovision")) {
		return fail("unsigned", "the archived app has no provisioning profile")
	}
	ipa := firstWithExt(exportDir, ".ipa")
	if ipa == "" {
		return fail("export_failed", "the export produced no .ipa")
	}
	if err := os.Rename(ipa, filepath.Join(dir, ipaName)); err != nil {
		return err
	}
	artifact, err := describe(filepath.Join(dir, ipaName))
	if err != nil {
		return err
	}
	b.Artifacts = []Artifact{artifact}
	b.SizeBytes = artifact.SizeBytes
	info := infoPlist(ctx, filepath.Join(app, "Info.plist"))
	b.BundleID, b.Version, b.BuildNumber = info.BundleID, info.Version, info.BuildNumber
	if wantedVersion != "" && b.Version != wantedVersion || wantedBuild != "" && b.BuildNumber != wantedBuild {
		return fail("version_mismatch", "the archived app did not use the requested version/build; check its Info.plist build settings")
	}
	if b.BundleID == "" {
		return fail("export_failed", "the archived app has no bundle identifier")
	}
	copyIcons(app, dir)
	keepArchive = forUpload
	return nil
}

// developmentTeam is the app target's team, else any target's.
func developmentTeam(settingsJSON []byte) string {
	targets := buildSettings(settingsJSON)
	for _, t := range targets {
		if t["WRAPPER_EXTENSION"] == "app" && t["DEVELOPMENT_TEAM"] != "" {
			return t["DEVELOPMENT_TEAM"]
		}
	}
	for _, t := range targets {
		if team := t["DEVELOPMENT_TEAM"]; team != "" {
			return team
		}
	}
	return ""
}

// buildSettings is each target's settings from `xcodebuild -showBuildSettings -json`.
func buildSettings(settingsJSON []byte) []map[string]string {
	var targets []struct {
		BuildSettings map[string]string `json:"buildSettings"`
	}
	// The login shell's profile may print before xcodebuild does.
	if i := bytes.IndexByte(settingsJSON, '['); i > 0 {
		settingsJSON = settingsJSON[i:]
	}
	if json.Unmarshal(settingsJSON, &targets) != nil {
		return nil
	}
	out := make([]map[string]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.BuildSettings)
	}
	return out
}

// Numbers are the version and build number a scheme's app builds Release with.
type Numbers struct {
	Version     string `json:"version"`
	BuildNumber string `json:"build_number"`
}

// ProjectNumbers reads the scheme's MARKETING_VERSION and CURRENT_PROJECT_VERSION,
// the numbers Publish starts from. It takes a few seconds: xcodebuild resolves the scheme.
func (s *Service) ProjectNumbers(ctx context.Context, project, path, target string) (Numbers, error) {
	if !IOSSupported() {
		return Numbers{}, ErrUnsupported
	}
	project, err := s.cfg.Paths.Contain(project)
	if err != nil {
		return Numbers{}, err
	}
	bundle, err := s.cfg.Paths.Contain(filepath.Join(project, path))
	if err != nil {
		return Numbers{}, err
	}
	if ext := filepath.Ext(bundle); ext != ".xcodeproj" && ext != ".xcworkspace" {
		return Numbers{}, fmt.Errorf("%w: path must be an .xcodeproj or .xcworkspace", ErrInvalid)
	}
	if target == "" {
		return Numbers{}, fmt.Errorf("%w: target (the scheme) is required", ErrInvalid)
	}
	container := "-project"
	if filepath.Ext(bundle) == ".xcworkspace" {
		container = "-workspace"
	}
	ctx, cancel := context.WithTimeout(ctx, settingsTimeout)
	defer cancel()
	out, err := command(ctx, filepath.Dir(bundle), nil, xcodebuild, "-showBuildSettings", "-json", container, bundle,
		"-scheme", target, "-configuration", "Release", "-destination", "generic/platform=iOS").Output()
	if err != nil {
		return Numbers{}, fmt.Errorf("xcodebuild could not read the build settings of %q: %w", target, err)
	}
	targets := buildSettings(out)
	for _, t := range targets {
		if t["WRAPPER_EXTENSION"] == "app" {
			return Numbers{Version: t["MARKETING_VERSION"], BuildNumber: t["CURRENT_PROJECT_VERSION"]}, nil
		}
	}
	if len(targets) > 0 {
		return Numbers{Version: targets[0]["MARKETING_VERSION"], BuildNumber: targets[0]["CURRENT_PROJECT_VERSION"]}, nil
	}
	return Numbers{}, nil
}

func exportOptions(team, method string) string {
	// release-testing is current Xcode's name for ad hoc.
	name := "release-testing"
	if method == MethodDevelopment {
		name = "development"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>method</key><string>` + name + `</string>
  <key>signingStyle</key><string>automatic</string>
  <key>teamID</key><string>` + xmlEscape(team) + `</string>
  <key>stripSwiftSymbols</key><true/>
  <key>thinning</key><string>&lt;none&gt;</string>
</dict>
</plist>
`
}

type appInfo struct {
	BundleID    string `json:"CFBundleIdentifier"`
	Version     string `json:"CFBundleShortVersionString"`
	BuildNumber string `json:"CFBundleVersion"`
}

// infoPlist reads the archived Info.plist, which is binary; plutil converts it.
func infoPlist(ctx context.Context, path string) appInfo {
	var info appInfo
	out, err := exec.CommandContext(ctx, plutil, "-convert", "json", "-o", "-", path).Output()
	if err == nil {
		_ = json.Unmarshal(out, &info)
	}
	return info
}

// copyIcons keeps the smallest and largest AppIcon for the install prompt.
func copyIcons(app, dir string) {
	icons, _ := filepath.Glob(filepath.Join(app, "AppIcon*.png"))
	if len(icons) == 0 {
		return
	}
	size := func(p string) int64 {
		info, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	sort.Slice(icons, func(i, j int) bool { return size(icons[i]) < size(icons[j]) })
	_ = copyFile(icons[0], filepath.Join(dir, "icon-57.png"))
	_ = copyFile(icons[len(icons)-1], filepath.Join(dir, "icon-512.png"))
}

func firstWithExt(dir, ext string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func classifyArchive(tail string) string {
	text := strings.ToLower(tail)
	switch {
	case strings.Contains(text, "no signing certificate"), strings.Contains(text, "code signing is required"):
		return "no_signing_identity"
	case strings.Contains(text, "requires a development team"), strings.Contains(text, "select a development team"):
		return "team_not_selected"
	case strings.Contains(text, "does not contain a scheme named"):
		return "scheme_not_found"
	case strings.Contains(text, "provisioning profile"):
		return "provisioning"
	}
	return "build_failed"
}

func classifyExport(tail string) string {
	text := strings.ToLower(tail)
	switch {
	case strings.Contains(text, "no signing certificate"):
		return "no_signing_identity"
	case strings.Contains(text, "provisioning profile"):
		return "provisioning"
	}
	return "export_failed"
}
