// Package projects is the `projects.*` methods: the project list, adding and
// creating projects, and the user's marks on them.
package projects

import (
	"context"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/files"
	projectcore "github.com/repogoapp/host/internal/project"
	"github.com/repogoapp/host/internal/projectsync"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/store"
)

type Deps struct {
	Projects *projectcore.Service
	// Self stamps every listed project, because a phone holds more than one host's.
	Self  device.ID
	Store *store.Store
	// Sync recomputes the project set; a list asks it for a pass.
	Sync *projectsync.Syncer
	// Files makes a folder a project root; containment lives there.
	Files *files.Service
}

type AddParams struct {
	Path string `json:"path"`
}

type DeleteParams struct {
	Path string `json:"path"`
}

type CreateParams struct {
	Name string `json:"name"`

	// Parent is a folder the picker can show; empty makes it under ~/RepoGo.
	Parent string `json:"parent"`
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
	rpc.Add(r, "projects.list", d.list, rpc.Tool("The projects on this environment, with each one's folder "+
		"(the cwd chats take), name, pin and changed files."), rpc.Read, rpc.LiveOn("projects.changed"))
	rpc.Add(r, "projects.detect_icon", d.detectIcon)
	// The user's marks on a listed project. Each answers the project's row and
	// sends it to every other device as projects.changed.
	rpc.Add(r, "projects.rename", d.rename)
	rpc.Add(r, "projects.pin", d.pin)
	// add is the folder picker's Select, making a folder it could only see a root.
	// add and create answer the new project's row, listed at once.
	rpc.Add(r, "projects.add", d.add)
	rpc.Add(r, "projects.create", d.create)
	// delete moves the folder to the Trash, or deletes it on a host with none
	// (host.status leaves out "trash"); the phone asks for its name first.
	rpc.Add(r, "projects.delete", d.delete)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, _ rpc.None) (ListResult, error) {
	// Answers from the table rather than waiting on a sweep, which reads every
	// project folder; a project that appeared since arrives as projects.changed.
	d.Sync.Nudge()
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

func (d Deps) detectIcon(ctx context.Context, _ rpc.Caller, a DetectIconParams) (projectcore.Icon, error) {
	return d.Projects.DetectIcon(ctx, a.Path, a.IfNoneMatch)
}

func (d Deps) add(ctx context.Context, _ rpc.Caller, a AddParams) (store.Project, error) {
	path, err := d.Files.AddProject(a.Path)
	return d.listed(ctx, path, err)
}

func (d Deps) create(ctx context.Context, _ rpc.Caller, a CreateParams) (store.Project, error) {
	path, err := d.Files.NewProject(a.Parent, a.Name)
	return d.listed(ctx, path, err)
}

// listed makes a pass now rather than at the next nudge, so the new folder's
// row exists to answer and every device hears it as projects.changed.
func (d Deps) listed(ctx context.Context, path string, err error) (store.Project, error) {
	if err != nil {
		return store.Project{}, err
	}
	d.Sync.Once(ctx)
	row, err := d.Store.Project(path)
	if err != nil {
		return store.Project{}, err
	}
	row.HostID = string(d.Self)
	return row, nil
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a DeleteParams) (rpc.Ack, error) {
	if _, err := d.Store.Project(a.Path); err != nil {
		return rpc.Ack{}, err
	}
	if err := d.Files.DeleteProject(a.Path); err != nil {
		return rpc.Ack{}, err
	}
	if err := d.Store.ForgetMarks(a.Path); err != nil {
		return rpc.Ack{}, err
	}
	// The pass finds the folder gone and sends its removal to every device.
	d.Sync.Nudge()
	return rpc.OK, nil
}
