package tunnel

import "github.com/repogo/host/internal/emit"

func init() {
	emit.Register(Changed{})
}

// Changed is the whole list, and whether the gateway is connected, after any
// change; sent to every paired device so each Ports sheet stays current.
type Changed struct {
	Tunnels   []Tunnel `json:"tunnels" wire:"array"`
	Connected bool     `json:"connected"`
}

func (Changed) Method() string { return "tunnels.changed" }
