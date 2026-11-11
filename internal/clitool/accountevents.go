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

// OnAccountsChanged tells f each change to a CLI's accounts, set once at assembly.
func (in *Inventory) OnAccountsChanged(f func(AccountsChanged)) { in.accountsChanged = f }

func (in *Inventory) announceAccounts(kind string) {
	if in.accountsChanged != nil {
		in.accountsChanged(AccountsChanged{Kind: kind})
	}
}
