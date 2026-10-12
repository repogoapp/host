package repogomcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/builds"
)

const (
	// maxBuildWait bounds one status wait under the shortest MCP call
	// timeout an agent CLI sets; the agent calls again for a longer build.
	maxBuildWait = 50 * time.Second
	buildPoll    = 2 * time.Second
	// buildLogTail is how much of the log a status carries, enough for the error.
	buildLogTail = 4 << 10
	// maxApprovalWait leaves room under Codex's 300-second MCP call timeout.
	maxApprovalWait = 4 * time.Minute
)

// The approval's answers; the phone fills the allow_once button.
const (
	optionUpload = "upload"
	optionReject = "reject"
)

var appsTool = tool{
	Name: "apps",
	Description: "The iOS and Android apps in this project, and the Xcodes installed on the user's environment. " +
		"An app's `path` and one of its `targets` are what `build` and `publish` take; an iOS app's `version` and " +
		"`build_number` are its current numbers, where a `publish` starts. Pass an Xcode's `path` as `xcode` to " +
		"build or publish with it; leave it out to use the selected one.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

var buildTool = tool{
	Name: "build",
	Description: "Build this project's iOS or Android app on the user's environment so they can install it on their phone. " +
		"Starts the build and returns its `id` at once; call `status` with that id until `status` is no longer \"building\". " +
		"iOS builds need Xcode on a Mac and an app whose signing team is set; the phone must be on the team's provisioning profile. " +
		"Android builds run the project's Gradle wrapper. Leave path, target and configuration empty to build the project's first app with its defaults.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"platform":      map[string]any{"type": "string", "enum": []string{builds.PlatformIOS, builds.PlatformAndroid}},
			"path":          prop("string", "The .xcodeproj or .xcworkspace, or the Gradle root, relative to the project."),
			"target":        prop("string", "The Xcode scheme, or the Gradle module such as :app."),
			"configuration": prop("string", "Release or Debug on iOS (default Release); a variant such as debug or release on Android (default debug)."),
			"method":        map[string]any{"type": "string", "enum": []string{builds.MethodAdHoc, builds.MethodDevelopment}, "description": "iOS export method; default ad_hoc."},
			"xcode":         prop("string", "An Xcode `path` from `apps`, for iOS; empty uses the selected one."),
		},
		"required": []string{"platform"},
	},
}

var publishTool = tool{
	Name: "publish",
	Description: "Archive a Release build of an iOS app and upload it to App Store Connect, where it reaches TestFlight. " +
		"The user approves the upload on their phone first, and the call waits for their answer. " +
		"Returns the publication and its `id`; call `status` with that id until `status` is no longer \"publishing\". " +
		"Uploaded means delivered to Apple, not reviewed or released. Take path, target and the current numbers from `apps`.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":         prop("string", "The .xcodeproj or .xcworkspace, relative to the project."),
			"target":       prop("string", "The Xcode scheme."),
			"version":      prop("string", "The marketing version: one to three dot-separated integers such as 1.2.0."),
			"build_number": prop("string", "Empty lets Xcode pick the next free one."),
			"xcode":        prop("string", "An Xcode `path` from `apps`; empty uses the selected one."),
		},
		"required": []string{"path", "target", "version"},
	},
}

var statusTool = tool{
	Name: "status",
	Description: "The status of a build from `build` or an upload from `publish`, waiting up to 50 seconds for it to finish. " +
		"Returns it under `build` or `publication`, and the end of its log. Once a build succeeded it carries an `install` link: " +
		"give the user `install.page_url`, which opens a page with an Install button on their phone. If `install_error` says " +
		"to open a tunnel, tell the user to open a tunnel to that port from the Ports sheet in RepoGo, then call this again.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": prop("string", "The id `build` or `publish` returned."),
		},
		"required": []string{"id"},
	},
}

var cancelTool = tool{
	Name:        "cancel",
	Description: "Stop a running build or upload.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": prop("string", "The id `build` or `publish` returned."),
		},
		"required": []string{"id"},
	},
}

type appsResult struct {
	Apps   []appEntry     `json:"apps"`
	Xcodes []builds.Xcode `json:"xcodes"`
}

// appEntry is an app with, for iOS, its default target's current numbers.
type appEntry struct {
	builds.App
	Version     string `json:"version,omitempty"`
	BuildNumber string `json:"build_number,omitempty"`
}

// listApps reads numbers only where Xcode can; a scheme it cannot resolve
// keeps its entry without them.
func listApps(ctx context.Context, svc *builds.Service, turn agent.RunningTurn) toolResult {
	apps, err := svc.Apps(turn.Cwd)
	if err != nil {
		return failed(err.Error())
	}
	out := appsResult{Apps: []appEntry{}, Xcodes: []builds.Xcode{}}
	ios := builds.IOSSupported()
	for _, app := range apps {
		entry := appEntry{App: app}
		if ios && app.Platform == builds.PlatformIOS && app.DefaultTarget != "" {
			if n, err := svc.ProjectNumbers(ctx, turn.Cwd, app.Path, app.DefaultTarget); err == nil {
				entry.Version, entry.BuildNumber = n.Version, n.BuildNumber
			}
		}
		out.Apps = append(out.Apps, entry)
	}
	if ios {
		if out.Xcodes, err = svc.Xcodes(ctx); err != nil {
			return failed(err.Error())
		}
	}
	return succeeded(out)
}

type buildArgs struct {
	Platform      string `json:"platform"`
	Path          string `json:"path"`
	Target        string `json:"target"`
	Configuration string `json:"configuration"`
	Method        string `json:"method"`
	Xcode         string `json:"xcode"`
}

// startBuild builds the turn's project; an empty path takes its first app of the platform.
func startBuild(svc *builds.Service, turn agent.RunningTurn, raw json.RawMessage) toolResult {
	var a buildArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return failed("invalid arguments: " + err.Error())
	}
	if a.Path == "" {
		apps, err := svc.Apps(turn.Cwd)
		if err != nil {
			return failed(err.Error())
		}
		for _, app := range apps {
			if app.Platform == a.Platform {
				a.Path = app.Path
				if a.Target == "" {
					a.Target = app.DefaultTarget
				}
				break
			}
		}
		if a.Path == "" {
			return failed("no " + a.Platform + " app found in this project")
		}
	}
	b, err := svc.Start(builds.Request{
		Project: turn.Cwd, Platform: a.Platform, Path: a.Path, Target: a.Target,
		Configuration: a.Configuration, Method: a.Method, Xcode: a.Xcode,
	})
	if err != nil {
		return failed(err.Error())
	}
	return succeeded(b)
}

type publishArgs struct {
	Path        string `json:"path"`
	Target      string `json:"target"`
	Version     string `json:"version"`
	BuildNumber string `json:"build_number"`
	Xcode       string `json:"xcode"`
}

// startPublish asks the user before anything leaves the machine. Each call is
// a new request id: an agent that calls again means to upload again.
func startPublish(svc *builds.Service, turn agent.RunningTurn, raw json.RawMessage) toolResult {
	var a publishArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return failed("invalid arguments: " + err.Error())
	}
	if a.Path == "" || a.Target == "" || a.Version == "" {
		return failed("path, target and version are required; take them from `apps`")
	}
	if !builds.IOSSupported() {
		return failed(builds.ErrUnsupported.Error())
	}
	if turn.Ask == nil {
		return failed("this turn cannot ask the user to approve an upload, so nothing was uploaded")
	}
	id := newRequestID()
	ctx, cancel := context.WithTimeout(turn.Ctx, maxApprovalWait)
	defer cancel()
	answer, err := turn.Ask(ctx, agent.Approval{
		CallID: "publish:" + id, Title: "Upload " + a.Target + " " + a.Version + " to App Store Connect",
		Kind: "mcp", Input: raw,
		Options: []agent.ApprovalOption{
			{OptionID: optionUpload, Name: "Upload", Kind: "allow_once"},
			{OptionID: optionReject, Name: "Don't upload", Kind: "reject_once"},
		},
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return failed("the user did not answer within 4 minutes, so nothing was uploaded; ask them, then call publish again")
	case err != nil:
		return failed("nothing was uploaded: " + err.Error())
	}
	var picked string
	if json.Unmarshal(answer, &picked) != nil || picked != optionUpload {
		return failed("the user declined the upload, so nothing was uploaded")
	}
	p, err := svc.Publish(builds.PublishRequest{
		RequestID: id, Project: turn.Cwd, Path: a.Path, Target: a.Target,
		Version: a.Version, BuildNumber: a.BuildNumber, Xcode: a.Xcode,
	})
	if err != nil {
		return failed(err.Error())
	}
	return succeeded(p)
}

type jobArgs struct {
	ID string `json:"id"`
}

// jobStatus is a build or a publication; Kind says which is set.
type jobStatus struct {
	Kind         string              `json:"kind"`
	Build        *builds.Build       `json:"build,omitempty"`
	Publication  *builds.Publication `json:"publication,omitempty"`
	LogTail      string              `json:"log_tail"`
	Install      *builds.InstallLink `json:"install,omitempty"`
	InstallError string              `json:"install_error,omitempty"`
}

// jobState waits for a build or a publication to end, bounded by the call and maxBuildWait.
func jobState(ctx context.Context, svc *builds.Service, raw json.RawMessage) toolResult {
	var a jobArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return failed("invalid arguments: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, maxBuildWait)
	defer cancel()
	b, err := svc.Get(a.ID)
	if errors.Is(err, builds.ErrNotFound) {
		return publicationState(ctx, svc, a.ID)
	}
	for err == nil && b.Status == builds.StatusBuilding && wait(ctx) {
		b, err = svc.Get(a.ID)
	}
	if err != nil {
		return failed(err.Error())
	}
	out := jobStatus{Kind: "build", Build: &b, LogTail: svc.LogTail(b.ID, buildLogTail)}
	if b.Status == builds.StatusSuccess {
		link, err := svc.InstallLink(b.ID)
		switch {
		case err == nil:
			out.Install = &link
		case errors.Is(err, builds.ErrNoTunnel):
			out.InstallError = err.Error()
		default:
			return failed(err.Error())
		}
	}
	return succeeded(out)
}

func publicationState(ctx context.Context, svc *builds.Service, id string) toolResult {
	p, err := svc.Publication(id)
	for err == nil && p.Status == builds.StatusPublishing && wait(ctx) {
		p, err = svc.Publication(id)
	}
	if err != nil {
		return failed(err.Error())
	}
	return succeeded(jobStatus{Kind: "publish", Publication: &p, LogTail: svc.PublishLogTail(p.ID, buildLogTail)})
}

// wait sleeps one poll; false once ctx ends.
func wait(ctx context.Context) bool {
	t := time.NewTimer(buildPoll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func cancelJob(svc *builds.Service, raw json.RawMessage) toolResult {
	var a jobArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return failed("invalid arguments: " + err.Error())
	}
	err := svc.Cancel(a.ID)
	if errors.Is(err, builds.ErrNotFound) {
		err = svc.CancelPublish(a.ID)
	}
	if errors.Is(err, builds.ErrNotFound) {
		return failed("no build or upload with id " + a.ID + " is running")
	}
	if err != nil {
		return failed(err.Error())
	}
	return succeeded(map[string]bool{"canceled": true})
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
