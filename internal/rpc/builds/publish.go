package builds

import (
	"context"
	"fmt"

	buildscore "github.com/repogo/host/internal/builds"
	"github.com/repogo/host/internal/rpc"
)

// PublishParams names the app as builds.apps lists it: project, path and target
// (the scheme). An empty build_number lets Xcode pick the next free one.
type PublishParams struct {
	RequestID   string `json:"request_id"`
	Project     string `json:"project"`
	Path        string `json:"path"`
	Target      string `json:"target"`
	Version     string `json:"version"`
	BuildNumber string `json:"build_number"`
}

type AppParams struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Target  string `json:"target"`
}

type PublicationParams struct {
	PublicationID string `json:"publication_id"`
}

type PublishLogParams struct {
	PublicationID string `json:"publication_id"`
	Offset        int64  `json:"offset"`
}

type PublicationResult struct {
	Publication buildscore.Publication `json:"publication"`
}

type PublicationsResult struct {
	Publications []buildscore.Publication `json:"publications" wire:"array"`
}

// appNumbers is the scheme's current version and build number, where the Publish form starts.
func (d Deps) appNumbers(ctx context.Context, _ rpc.Caller, a AppParams) (buildscore.Numbers, error) {
	return d.Builds.ProjectNumbers(ctx, a.Project, a.Path, a.Target)
}

func (d Deps) publish(_ context.Context, _ rpc.Caller, a PublishParams) (PublicationResult, error) {
	p, err := d.Builds.Publish(buildscore.PublishRequest(a))
	return PublicationResult{Publication: p}, err
}

func (d Deps) publishStatus(_ context.Context, _ rpc.Caller, a PublicationParams) (PublicationResult, error) {
	p, err := d.Builds.Publication(a.PublicationID)
	return PublicationResult{Publication: p}, err
}

// publishList is the app's uploads, newest first; the phone's form starts from the last one.
func (d Deps) publishList(_ context.Context, _ rpc.Caller, a AppParams) (PublicationsResult, error) {
	if a.Project == "" {
		return PublicationsResult{}, fmt.Errorf("%w: project is required", rpc.ErrInvalidParams)
	}
	ps, err := d.Builds.Publications(a.Project, a.Path, a.Target)
	return PublicationsResult{Publications: ps}, err
}

func (d Deps) publishLog(_ context.Context, _ rpc.Caller, a PublishLogParams) (buildscore.LogChunk, error) {
	return d.Builds.PublishLog(a.PublicationID, a.Offset)
}

func (d Deps) publishCancel(_ context.Context, _ rpc.Caller, a PublicationParams) (rpc.Ack, error) {
	return rpc.OK, d.Builds.CancelPublish(a.PublicationID)
}
