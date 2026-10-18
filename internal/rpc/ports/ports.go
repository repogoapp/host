// Package ports is the `ports.*` methods: the sockets listening on this host,
// and stopping one; the scanning lives in internal/ports.
package ports

import (
	"context"

	portscore "github.com/repogoapp/host/internal/ports"
	"github.com/repogoapp/host/internal/rpc"
)

type ListParams struct {
	Dir string `json:"dir"`
}

type ListResult struct {
	Ports []portscore.Port `json:"ports" wire:"array"`
}

type KillParams struct {
	Port  int  `json:"port"`
	Force bool `json:"force"` // SIGKILL instead of SIGTERM
}

// Killed counts the processes that took the signal; none when nothing was
// listening or every listener refused.
type Killed struct {
	Processes int `json:"processes"`
}

func Register(r *rpc.Router) {
	// list is every listening socket on this machine, or only those started
	// under dir.
	rpc.Add(r, "ports.list", list)
	// kill stops whatever listens on a port, so a stuck dev server can be
	// cleared from the Ports sheet without a terminal.
	rpc.Add(r, "ports.kill", kill, rpc.Detached)
}

func list(ctx context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	return ListResult{Ports: portscore.ListPorts(ctx, a.Dir)}, nil
}

func kill(ctx context.Context, _ rpc.Caller, a KillParams) (Killed, error) {
	processes, err := portscore.KillPort(ctx, a.Port, a.Force)
	return Killed{Processes: processes}, err
}
