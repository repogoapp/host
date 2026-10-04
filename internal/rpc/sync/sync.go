// Package sync is the `sync.pull` method for the mirrored chats: rows to upsert
// since a revision, and deletes as a set difference against what the client
// holds. The algorithm is store.Pull.
package sync

import (
	"context"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/store"
)

type Deps struct {
	Self device.ID
	// Mirror answers one pull, every row stamped with Self.
	Mirror *store.Store
	// ModelLabel names each row's model for the phone, as chats.list does.
	ModelLabel func(agent.Kind, string) string
	// Imported waits for the cache's first sweep: a pull from a cache rebuilt
	// at startup would otherwise read as most chats deleted.
	Imported func(context.Context) error
}

// Remote-reachable. Every chat mirrored is one chats.* would already serve.
func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "sync.pull", d.pull)
}

func (d Deps) pull(ctx context.Context, _ rpc.Caller, a store.PullRequest) (store.PullReply, error) {
	if err := d.Imported(ctx); err != nil {
		return store.PullReply{}, err
	}
	reply, err := d.Mirror.Pull(a, string(d.Self))
	for i := range reply.Upsert {
		reply.Upsert[i].Stamp(string(d.Self), d.ModelLabel)
	}
	return reply, err
}
