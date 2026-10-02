package chatlive

import (
	"log/slog"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
)

// Peers is the paired devices a chat list change goes to.
type Peers interface {
	Identity() *device.Identity
	ActivePeers() []device.Peer
}

// Announcer is the one path for every way a chat row can move: the store
// says which rows a commit touched, and every paired device hears them.
type Announcer struct {
	peers      Peers
	emit       *emit.Emitter
	db         *store.Store
	modelLabel func(agent.Kind, string) string
	log        *slog.Logger
}

func NewAnnouncer(peers Peers, em *emit.Emitter, db *store.Store, modelLabel func(agent.Kind, string) string, log *slog.Logger) *Announcer {
	return &Announcer{peers: peers, emit: em, db: db, modelLabel: modelLabel, log: log}
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
		chat.Stamp(string(a.peers.Identity().ID), a.modelLabel)
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
	for _, peer := range a.peers.ActivePeers() {
		if err := a.emit.To(peer.ID, ev); err != nil {
			a.log.Debug("chat list update not delivered", "device", peer.ID, "err", err)
		}
	}
}
