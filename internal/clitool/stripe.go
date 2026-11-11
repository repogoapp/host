package clitool

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stripeStatus is who the Stripe CLI is signed in as, from its config: it has
// no status command. Only the project it uses (STRIPE_PROJECT_NAME, else
// "default") is read, and only its name, account and expiry, never a key. A
// sign-in lasts about 90 days.
func stripeStatus(context.Context, string) (bool, *Account) {
	p := readStripeProject(cmp.Or(os.Getenv("STRIPE_PROJECT_NAME"), "default"))
	if p.accountID == "" || !p.current(time.Now()) {
		return false, nil
	}
	login := p.accountID
	if p.displayName != "" {
		login = p.displayName + " · " + p.accountID
	}
	return true, &Account{Login: login}
}

// stripeProject is one section of the config, with the fields status reads.
type stripeProject struct {
	displayName, accountID, expires string
}

// current is a sign-in whose keys haven't expired.
func (p stripeProject) current(now time.Time) bool {
	expires, err := time.Parse("2006-01-02", p.expires)
	return err == nil && !now.After(expires.Add(24*time.Hour))
}

// readStripeProject reads the config's [name] section; none is empty.
func readStripeProject(name string) stripeProject {
	var p stripeProject
	data, err := os.ReadFile(stripeConfig())
	if err != nil {
		return p
	}
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			in = strings.Trim(line, "[]") == name
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !in || !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `'"`)
		switch strings.TrimSpace(key) {
		case "display_name":
			p.displayName = value
		case "account_id":
			p.accountID = value
		case "test_mode_key_expires_at":
			p.expires = value
		}
	}
	return p
}

// stripeConfig is the Stripe CLI's config file, as it finds it.
func stripeConfig() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "stripe", "config.toml")
}
