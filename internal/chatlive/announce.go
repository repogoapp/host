package chatlive

import (
	"log/slog"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/store"
)

// Peers is the paired devices a chat list change goes to.
type Peers interface {
	Identity() *device.Identity
	Peers() []device.Peer
}

// Announcer is the one path for every way a chat row can move: the store
// says which rows a commit touched, and every paired device hears them, as
// does every client on this machine (the desktop app), which is no peer.
type Announcer struct {
	peers      Peers
	locals     func() []device.ID
	emit       *emit.Emitter
	db         *store.Store
	modelLabel func(agent.Kind, string) string
	agentIcon  func(cwd, projectAgent string) string
	log        *slog.Logger
}

func NewAnnouncer(peers Peers, locals func() []device.ID, em *emit.Emitter, db *store.Store,
	modelLabel func(agent.Kind, string) string, agentIcon func(cwd, projectAgent string) string, log *slog.Logger) *Announcer {
	return &Announcer{peers: peers, locals: locals, emit: em, db: db, modelLabel: modelLabel, agentIcon: agentIcon, log: log}
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
		a.send(Changed{Chat: chat})
	}
	for _, id := range c.Removed {
		a.send(Removed{ChatID: string(id)})
	}
}

// Attention goes to every device, whichever chat it has open.
func (a *Announcer) Attention(at Attention) {
	a.send(at)
}

func (a *Announcer) send(ev emit.Event) {
	for _, id := range device.Recipients(a.peers.Peers(), a.locals()) {
		if err := a.emit.To(id, ev); err != nil {
			a.log.Debug("chat list update not delivered", "device", id, "err", err)
		}
	}
}
