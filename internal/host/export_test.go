package host

import (
	"context"

	"github.com/repogoapp/host/internal/hostlink"
	"github.com/repogoapp/host/internal/repogomcp"
)

// Spawn runs fn as an owned worker, for a test that stands in for one.
func (h *Host) Spawn(fn func(context.Context)) { h.spawn(fn) }

// WaitWithin is Close's bounded wait for its workers.
var WaitWithin = waitWithin

// PushLinks is the link chat rides and the one push.send goes over.
func (h *Host) PushLinks() (link, push *hostlink.Link) { return h.link, h.pushLink }

// Tools is RepoGo's own MCP server as the host wired it.
func (h *Host) Tools() *repogomcp.Server { return h.tools }
