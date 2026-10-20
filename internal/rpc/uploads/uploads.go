// Package uploads is the `uploads.*` methods: a phone sends a file in chunks
// for a plugin call to read; the staging lives in internal/uploads.
package uploads

import (
	"context"

	"github.com/repogoapp/host/internal/rpc"
	uploadscore "github.com/repogoapp/host/internal/uploads"
)

type Deps struct {
	Uploads *uploadscore.Store
}

// WriteParams is one chunk of a file. The first names the file; every chunk
// repeats it unchanged, so a write after a restart needs nothing remembered.
type WriteParams struct {
	Project string `json:"project" doc:"The project whose plugin the file is for"`
	ID      string `json:"id" doc:"A UUID the phone picks for the upload"`
	Name    string `json:"name"`
	Type    string `json:"type" doc:"The file's MIME type"`
	Size    int64  `json:"size" doc:"The whole file's bytes"`
	Offset  int64  `json:"offset" doc:"Where this chunk starts"`
	Data    []byte `json:"data" wire:"array"`
	Done    bool   `json:"done,omitempty" doc:"This chunk ends the file"`
}

type RemoveParams struct {
	ID string `json:"id"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "uploads.write", d.write, rpc.Paired)
	rpc.Add(r, "uploads.remove", d.remove, rpc.Paired)
}

func (d Deps) write(_ context.Context, c rpc.Caller, p WriteParams) (uploadscore.Progress, error) {
	return d.Uploads.Write(uploadscore.Chunk{
		Device: c.Device, Project: p.Project, ID: p.ID, Name: p.Name, Type: p.Type,
		Size: p.Size, Offset: p.Offset, Data: p.Data, Done: p.Done,
	})
}

func (d Deps) remove(_ context.Context, c rpc.Caller, p RemoveParams) (rpc.Ack, error) {
	return rpc.OK, d.Uploads.Remove(c.Device, p.ID)
}
