// Package fs is the `fs.*` methods: browsing and editing the user's project
// files; containment lives in internal/files.
package fs

import (
	"context"
	"fmt"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/rpc"
)

// Watcher runs one watcher per folder for the devices holding it, pushing
// fs.change and git.changed.
type Watcher interface {
	Watch(caller device.ID, paths, active []string, resync bool) error
	Stop(caller device.ID)
}

type Deps struct {
	Files *files.Service
	Watch Watcher
}

// WatchParams is the device's whole watch set, which also renews its lease.
// Active is the part the user has open: it starts a project's environment.
type WatchParams struct {
	Paths  []string `json:"paths" wire:"array"`
	Active []string `json:"active" wire:"array"`
	// Resync resends every path's git state: the device relaunched and holds nothing.
	Resync bool `json:"resync,omitempty"`
}

type NewProjectParams struct {
	Name string `json:"name"`

	// Parent is a folder the picker can show; empty makes it under ~/RepoGo.
	Parent string `json:"parent"`
}

type NewProjectResult struct {
	Path string `json:"path"`
}

type PathParams struct {
	Path string `json:"path"`
}

// ReadParams reads one file; Scope, when set, narrows what it may be.
type ReadParams struct {
	Path string `json:"path"`
	// ModTime, when set, is the mod_time last read: a file unchanged since
	// comes back Unchanged, without its content.
	ModTime int64 `json:"mod_time"`
	files.Scope
}

// ReadResult is the file, or with Unchanged set only its path, size and
// mod_time: the caller's copy is current.
type ReadResult struct {
	files.File
	Unchanged bool `json:"unchanged"`
}

// ListParams lists one folder: inside a root all of it, and past the roots
// the pickers' names under home, starting there when path is empty.
type ListParams struct {
	Path string `json:"path"`

	// DirsOnly is the folder picker: folders alone.
	DirsOnly bool `json:"dirs_only"`
	// Hidden is the env source picker: past the roots, dot entries and
	// ~/Library too, since every .env is a dotfile.
	Hidden bool `json:"hidden"`

	// Extensions lists every file under path with one of these extensions
	// (["md"], compared without case), each named by its path relative to
	// path, instead of path's own entries.
	Extensions []string `json:"extensions" wire:"array"`
	// Within, with Extensions, holds the listed folder to one project.
	Within string `json:"within"`
}

type ListResult struct {
	Path    string        `json:"path"`
	Entries []files.Entry `json:"entries" wire:"array"`

	// Truncated is an Extensions list cut short at the walk's bound.
	Truncated bool `json:"truncated"`
}

type WriteParams struct {
	Path    string `json:"path"`
	Content []byte `json:"content" wire:"array"`
	// Create fails rather than replace an existing file.
	Create bool `json:"create"`
	// ModTime, when set, is the mod_time last read: a file changed since is
	// refused rather than overwritten.
	ModTime int64 `json:"mod_time"`
	files.Scope
}

// ReplaceParams swaps the one place Old appears for New.
type ReplaceParams struct {
	Path    string `json:"path"`
	Old     string `json:"old"`
	New     string `json:"new"`
	ModTime int64  `json:"mod_time"`
	files.Scope
}

type SearchParams struct {
	Path          string           `json:"path"`
	Query         string           `json:"query"`
	Mode          files.SearchMode `json:"mode"`
	Limit         int              `json:"limit"`
	CaseSensitive bool             `json:"case_sensitive"`
}

type RenameParams struct {
	Path    string `json:"path"`
	NewPath string `json:"new_path"`
}

func Register(r *rpc.Router, d Deps) {
	// Remote-reachable: the guard is root containment, not caller scope; past
	// the roots fs.list shows names under home, never contents.
	rpc.Add(r, "fs.list", d.list)
	// watch replaces the caller's watch set and renews its lease; stop drops it.
	rpc.Add(r, "fs.watch", d.watch)
	rpc.Add(r, "fs.stop", d.stop)
	rpc.Add(r, "fs.read", d.read)
	rpc.Add(r, "fs.write", d.write)
	rpc.Add(r, "fs.replace", d.replace)
	rpc.Add(r, "fs.search", d.search)
	rpc.Add(r, "fs.delete", d.delete)
	rpc.Add(r, "fs.rename", d.rename)
	rpc.Add(r, "fs.mkdir", d.mkdir)
	rpc.Add(r, "fs.new_project", d.newProject)
	rpc.Add(r, "fs.add_project", d.addProject)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	if len(a.Extensions) > 0 {
		if a.DirsOnly {
			return ListResult{}, fmt.Errorf("%w: extensions lists files, dirs_only folders", files.ErrInvalidOperation)
		}
		entries, truncated, err := d.Files.ListFiles(a.Path, a.Extensions, a.Within)
		return ListResult{Path: a.Path, Entries: entries, Truncated: truncated}, err
	}
	dir, entries, err := d.Files.Browse(a.Path, files.BrowseOptions{DirsOnly: a.DirsOnly, Hidden: a.Hidden})
	return ListResult{Path: dir, Entries: entries}, err
}

func (d Deps) read(_ context.Context, _ rpc.Caller, a ReadParams) (ReadResult, error) {
	file, changed, err := d.Files.ReadChanged(a.Path, a.Scope, a.ModTime)
	return ReadResult{File: file, Unchanged: err == nil && !changed}, err
}

func (d Deps) write(_ context.Context, _ rpc.Caller, a WriteParams) (files.File, error) {
	return d.Files.WriteWith(a.Path, a.Content, files.WriteOptions{Create: a.Create, ModTime: a.ModTime, Scope: a.Scope})
}

func (d Deps) replace(_ context.Context, _ rpc.Caller, a ReplaceParams) (files.File, error) {
	return d.Files.Replace(a.Path, a.Old, a.New, files.WriteOptions{ModTime: a.ModTime, Scope: a.Scope})
}

func (d Deps) search(_ context.Context, _ rpc.Caller, a SearchParams) (files.SearchResult, error) {
	return d.Files.Search(a.Path, a.Query, a.Mode, a.Limit, a.CaseSensitive)
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a PathParams) (rpc.Ack, error) {
	return rpc.OK, d.Files.Delete(a.Path)
}

func (d Deps) rename(_ context.Context, _ rpc.Caller, a RenameParams) (rpc.Ack, error) {
	return rpc.OK, d.Files.Rename(a.Path, a.NewPath)
}

func (d Deps) mkdir(_ context.Context, _ rpc.Caller, a PathParams) (rpc.Ack, error) {
	return rpc.OK, d.Files.Mkdir(a.Path)
}

func (d Deps) newProject(_ context.Context, _ rpc.Caller, a NewProjectParams) (NewProjectResult, error) {
	path, err := d.Files.NewProject(a.Parent, a.Name)
	return NewProjectResult{Path: path}, err
}

// addProject is the picker's Select, making a folder it could only see a root.
func (d Deps) addProject(_ context.Context, _ rpc.Caller, a PathParams) (NewProjectResult, error) {
	path, err := d.Files.AddProject(a.Path)
	return NewProjectResult{Path: path}, err
}

func (d Deps) watch(_ context.Context, c rpc.Caller, a WatchParams) (rpc.Ack, error) {
	return rpc.OK, d.Watch.Watch(c.Device, a.Paths, a.Active, a.Resync)
}

func (d Deps) stop(_ context.Context, c rpc.Caller, _ rpc.None) (rpc.Ack, error) {
	d.Watch.Stop(c.Device)
	return rpc.OK, nil
}
