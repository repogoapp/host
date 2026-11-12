package clitool

import "github.com/repogoapp/host/internal/emit"

func init() {
	emit.Register(AccountsChanged{})
}

// AccountsChanged tells every paired device a CLI's accounts or a project's
// pick of one moved, so a Connect card or an accounts list asks again.
type AccountsChanged struct {
	Kind string `json:"kind"`
}

func (AccountsChanged) Method() string { return "tools.accounts_changed" }

// SendTo names where each change to a CLI's accounts goes, set once at assembly.
func (in *Inventory) SendTo(events emit.Sink) { in.events = events }

func (in *Inventory) announceAccounts(kind string) {
	if in.events != nil {
		in.events.All(AccountsChanged{Kind: kind})
	}
}
