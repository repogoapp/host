package host

import (
	"context"

	"github.com/repogoapp/host/internal/reminder"
	"github.com/repogoapp/host/internal/repogomcp"
)

// Spawn runs fn as an owned worker, for a test that stands in for one.
func (h *Host) Spawn(fn func(context.Context)) { h.spawn(fn) }

// WaitWithin is Close's bounded wait for its workers.
var WaitWithin = waitWithin

// Remind pushes a reminder as the reminders worker does when one comes due.
func (h *Host) Remind(ctx context.Context, a reminder.Alert) { h.remind(ctx, a) }

// Tools is RepoGo's own MCP server as the host wired it.
func (h *Host) Tools() *repogomcp.Server { return h.tools }
