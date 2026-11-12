package chatlive

import (
	"log/slog"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/store"
)

// Peers names this host, which stamps each row it announces.
type Peers interface {
	Identity() *device.Identity
}

// Announcer is the one path for every way a chat row can move: the store
// says which rows a commit touched, and every paired device hears them, as
// does every client on this machine (the desktop app), which is no peer.
type Announcer struct {
	peers      Peers
	emit       *emit.Emitter
	db         *store.Store
	modelLabel func(agent.Kind, string) string
	agentIcon  func(cwd, projectAgent string) string
	log        *slog.Logger
}

func NewAnnouncer(peers Peers, em *emit.Emitter, db *store.Store,
	modelLabel func(agent.Kind, string) string, agentIcon func(cwd, projectAgent string) string, log *slog.Logger) *Announcer {
	return &Announcer{peers: peers, emit: em, db: db, modelLabel: modelLabel, agentIcon: agentIcon, log: log}
}

// Announce sends each changed row as it reads after the commit, and each
// removed id. A row deleted since is skipped; its removal follows.
func (a *Announcer) Announce(c store.Change) {
	for _, id := range c.Changed {
		chat, err := a.db.Info(id)
		if err != nil {
			a.log.Debug("chat row not announced", "chat", id, "err", err)
			continue
		}
		chat.Stamp(string(a.peers.Identity().ID), a.modelLabel, a.agentIcon)
		a.emit.All(Changed{Chat: chat})
	}
	for _, id := range c.Removed {
		a.emit.All(Removed{ChatID: string(id)})
	}
}

// Attention goes to every device, whichever chat it has open.
func (a *Announcer) Attention(at Attention) {
	a.emit.All(at)
}
