package clitool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/clilogin"
)

// Stripe's status is the project it uses, read from its config: signed in
// while its keys last, named by its account, never by a key.
func TestStripeStatusReadsItsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("STRIPE_PROJECT_NAME", "")
	valid := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	config := "color = ''\n\n[default]\naccount_id = 'acct_123'\ndisplay_name = 'VenueRun'\ntest_mode_key_expires_at = '" + valid +
		"'\ntest_mode_api_key = 'rk_test_secret'\n\n[boppr]\naccount_id = 'acct_456'\ndisplay_name = 'Boppr'\ntest_mode_key_expires_at = '2020-01-01'\n"
	if err := os.MkdirAll(filepath.Dir(stripeConfig()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stripeConfig(), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ok, who := stripeStatus(ctx, "")
	if !ok || who.Login != "VenueRun · acct_123" || strings.Contains(who.Login, "rk_test") {
		t.Fatalf("default = %v, %+v", ok, who)
	}
	t.Setenv("STRIPE_PROJECT_NAME", "boppr")
	if ok, _ := stripeStatus(ctx, ""); ok {
		t.Fatal("expired keys read as signed in")
	}
	t.Setenv("STRIPE_PROJECT_NAME", "missing")
	if ok, _ := stripeStatus(ctx, ""); ok {
		t.Fatal("a missing project read as signed in")
	}
}

// Stripe's sign-in shows a pairing code and the dashboard page to confirm it.
func TestStripeReadsItsPairingCode(t *testing.T) {
	var stripe Tool
	for _, d := range DeclaredCLIs {
		if d.Kind == "stripe" {
			stripe = d.Tool()
		}
	}
	var c clilogin.Code
	done := false
	for _, line := range []string{
		"Your pairing code is: enjoy-quick-sunny-gold",
		"This pairing code verifies your authentication with Stripe.",
		"Press Enter to open the browser or visit https://dashboard.stripe.com/stripecli/confirm_auth?t=abcDEF123 (^C to quit)",
	} {
		done = stripe.Spec.LoginRead(line, &c)
	}
	if !done || c.UserCode != "enjoy-quick-sunny-gold" || c.URL != "https://dashboard.stripe.com/stripecli/confirm_auth?t=abcDEF123" {
		t.Fatalf("code = %+v, done %v", c, done)
	}
}
