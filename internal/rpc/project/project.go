// Package project exposes the project list and read-only project metadata discovery.
package project

import (
	"context"
	"time"

	"github.com/repogo/host/internal/device"
	projectcore "github.com/repogo/host/internal/project"
	"github.com/repogo/host/internal/projectsync"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/store"
)

type Deps struct {
	Projects *projectcore.Service
	// Self stamps every listed project, because a phone holds more than one host's.
	Self  device.ID
	Store *store.Store
	// Sync recomputes the project set; a failed pass answers from what is stored.
	Sync *projectsync.Syncer
}

type DetectIconParams struct {
	Path        string `json:"path"`
	IfNoneMatch string `json:"if_none_match"`
}

type ListResult struct {
	Projects []store.Project `json:"projects" wire:"array"`
}

type RenameParams struct {
	Path string `json:"path"`
	// Blank clears the rename, back to the repository or folder name.
	Name string `json:"name"`
}

type PinParams struct {
	Path   string `json:"path"`
	Pinned bool   `json:"pinned"`
}

// Remote-reachable: every project listed is a folder fs.* would already serve.
func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "project.list", d.list)
	rpc.Add(r, "project.detect_icon", d.detectIcon)
	// The user's marks on a listed project. Each answers the project's row and
	// sends it to every other device as project.changed.
	rpc.Add(r, "project.rename", d.rename)
	rpc.Add(r, "project.pin", d.pin)
}

func (d Deps) list(ctx context.Context, _ rpc.Caller, _ rpc.None) (ListResult, error) {
	// A project that appeared a moment ago is not in the table until the sweep
	// runs, and listing is exactly when a client asks what changed.
	d.Sync.Once(ctx)
	projects, err := d.Store.ListProjects(string(d.Self))
	return ListResult{Projects: projects}, err
}

func (d Deps) rename(_ context.Context, _ rpc.Caller, a RenameParams) (store.Project, error) {
	row, err := d.Store.Rename(a.Path, a.Name)
	return d.marked(row, err)
}

func (d Deps) pin(_ context.Context, _ rpc.Caller, a PinParams) (store.Project, error) {
	row, err := d.Store.SetPinned(a.Path, a.Pinned, time.Now())
	return d.marked(row, err)
}

// marked announces a row the user changed and answers it, stamped as listed.
func (d Deps) marked(row store.Project, err error) (store.Project, error) {
	if err != nil {
		return store.Project{}, err
	}
	d.Sync.Announce(row)
	row.HostID = string(d.Self)
	return row, nil
}

func (d Deps) detectIcon(ctx context.Context, _ rpc.Caller, a DetectIconParams) (projectcore.Result, error) {
	return d.Projects.DetectIcon(ctx, a.Path, a.IfNoneMatch)
}
