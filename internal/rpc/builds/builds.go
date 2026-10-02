// Package builds is the `builds.*` methods: iOS and Android builds of a
// project's app and their install links; the building lives in internal/builds.
package builds

import (
	"context"

	buildscore "github.com/repogo/host/internal/builds"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Builds *buildscore.Service
}

type ProjectParams struct {
	Project string `json:"project"`
}

type AppsResult struct {
	Apps         []buildscore.App `json:"apps" wire:"array"`
	IOSSupported bool             `json:"ios_supported"`
	// InstallPort is the local port a tunnel must reach for install links to work.
	InstallPort int `json:"install_port"`
}

type ListResult struct {
	Builds []buildscore.Build `json:"builds" wire:"array"`
}

type StartParams struct {
	Project       string `json:"project"`
	Platform      string `json:"platform"`
	Path          string `json:"path"`
	Target        string `json:"target"`
	Configuration string `json:"configuration"`
	Method        string `json:"method"`
	Version       string `json:"version"`
	BuildNumber   string `json:"build_number"`
}

type BuildResult struct {
	Build buildscore.Build `json:"build"`
}

type BuildParams struct {
	BuildID string `json:"build_id"`
}

type LogParams struct {
	BuildID string `json:"build_id"`
	Offset  int64  `json:"offset"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "builds.apps", d.apps)
	rpc.Add(r, "builds.list", d.list)
	rpc.Add(r, "builds.get", d.get)
	// start answers once the build is recorded; it runs on without the caller.
	rpc.Add(r, "builds.start", d.start)
	rpc.Add(r, "builds.cancel", d.cancel)
	rpc.Add(r, "builds.delete", d.delete)
	rpc.Add(r, "builds.log", d.log)
	rpc.Add(r, "builds.install_link", d.installLink)
	rpc.Add(r, "builds.app_numbers", d.appNumbers)
	rpc.Add(r, "builds.publish", d.publish)
	rpc.Add(r, "builds.publish_status", d.publishStatus)
	rpc.Add(r, "builds.publish_list", d.publishList)
	rpc.Add(r, "builds.publish_log", d.publishLog)
	rpc.Add(r, "builds.publish_cancel", d.publishCancel)
}

func (d Deps) apps(_ context.Context, _ rpc.Caller, a ProjectParams) (AppsResult, error) {
	apps, err := d.Builds.Apps(a.Project)
	return AppsResult{Apps: apps, IOSSupported: buildscore.IOSSupported(), InstallPort: d.Builds.InstallPort()}, err
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ProjectParams) (ListResult, error) {
	builds, err := d.Builds.List(a.Project)
	return ListResult{Builds: builds}, err
}

func (d Deps) get(_ context.Context, _ rpc.Caller, a BuildParams) (BuildResult, error) {
	b, err := d.Builds.Get(a.BuildID)
	return BuildResult{Build: b}, err
}

func (d Deps) start(_ context.Context, _ rpc.Caller, a StartParams) (BuildResult, error) {
	b, err := d.Builds.Start(buildscore.Request(a))
	return BuildResult{Build: b}, err
}

func (d Deps) cancel(_ context.Context, _ rpc.Caller, a BuildParams) (rpc.Ack, error) {
	return rpc.OK, d.Builds.Cancel(a.BuildID)
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a BuildParams) (rpc.Ack, error) {
	return rpc.OK, d.Builds.Delete(a.BuildID)
}

func (d Deps) log(_ context.Context, _ rpc.Caller, a LogParams) (buildscore.LogChunk, error) {
	return d.Builds.Log(a.BuildID, a.Offset)
}

func (d Deps) installLink(_ context.Context, _ rpc.Caller, a BuildParams) (buildscore.InstallLink, error) {
	return d.Builds.InstallLink(a.BuildID)
}
