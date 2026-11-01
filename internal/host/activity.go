package host

import (
	"strings"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/shipping"
	"github.com/repogoapp/host/internal/store"
)

// trackAgentTime runs the agent clock: spans a stopped host left open end at
// their chat's last sign of life, then each recorded chat status moves its
// chat's span in the usage ledger. Before the chat cache's first write.
func (h *Host) trackAgentTime() {
	usage := h.Services.Usage
	lastSeen := func(chat string) (time.Time, bool) { return h.db.LastSeen(store.ChatID(chat)) }
	if n, err := usage.CloseOpenAgents(h.ctx, lastSeen); err != nil {
		h.log.Warn("activity: closing agent spans left open failed", "err", err)
	} else if n > 0 {
		h.log.Info("activity: closed agent spans left open", "spans", n)
	}
	h.db.OnStatus(func(c store.StatusChange) {
		kind, _, _ := strings.Cut(string(c.Chat), ":")
		status := shipping.AgentStatus{
			ChatID: string(c.Chat), Agent: kind, Cwd: c.Cwd, TurnID: c.TurnKey,
			Working: c.Status == agent.ChatWorking, At: c.At,
		}
		if err := usage.AgentStatus(h.ctx, status); err != nil {
			h.log.Warn("activity: agent clock failed", "chat", c.Chat, "status", c.Status, "err", err)
		}
	})
}
