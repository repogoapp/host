package repogomcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/builds"
)

const (
	// maxBuildWait bounds one build_status wait under the shortest MCP call
	// timeout an agent CLI sets; the agent calls again for a longer build.
	maxBuildWait = 50 * time.Second
	buildPoll    = 2 * time.Second
	// buildLogTail is how much of the log a status carries, enough for the error.
	buildLogTail = 4 << 10
)

var buildTool = tool{
	Name: "build",
	Description: "Build this project's iOS or Android app on the user's environment so they can install it on their phone. " +
		"Starts the build and returns its `id` at once; call `build_status` with that id until `status` is no longer \"building\". " +
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
		},
		"required": []string{"platform"},
	},
}

var buildStatusTool = tool{
	Name: "build_status",
	Description: "The status of a build started with `build`, waiting up to 50 seconds for it to finish. " +
		"Returns the build, the end of its log, and once it succeeded an `install` link: give the user `install.page_url`, " +
		"which opens a page with an Install button on their phone. If `install_error` says to open a tunnel, tell the user to open " +
		"a tunnel to that port from the Ports sheet in RepoGo, then call this again.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"build_id": prop("string", "The id `build` returned."),
		},
		"required": []string{"build_id"},
	},
}

type buildArgs struct {
	Platform      string `json:"platform"`
	Path          string `json:"path"`
	Target        string `json:"target"`
	Configuration string `json:"configuration"`
	Method        string `json:"method"`
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
		Configuration: a.Configuration, Method: a.Method,
	})
	if err != nil {
		return failed(err.Error())
	}
	return succeeded(b)
}

type buildStatus struct {
	Build        builds.Build        `json:"build"`
	LogTail      string              `json:"log_tail"`
	Install      *builds.InstallLink `json:"install,omitempty"`
	InstallError string              `json:"install_error,omitempty"`
}

// buildState waits for the build to end, bounded by the call and maxBuildWait.
func buildState(ctx context.Context, svc *builds.Service, raw json.RawMessage) toolResult {
	var a struct {
		BuildID string `json:"build_id"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return failed("invalid arguments: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, maxBuildWait)
	defer cancel()
	tick := time.NewTicker(buildPoll)
	defer tick.Stop()
	b, err := svc.Get(a.BuildID)
wait:
	for err == nil && b.Status == builds.StatusBuilding {
		select {
		case <-ctx.Done():
			break wait
		case <-tick.C:
			b, err = svc.Get(a.BuildID)
		}
	}
	if err != nil {
		return failed(err.Error())
	}
	out := buildStatus{Build: b, LogTail: svc.LogTail(b.ID, buildLogTail)}
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
