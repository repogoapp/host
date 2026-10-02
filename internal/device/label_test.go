package device

import (
	"os"
	"strings"
	"testing"
)

// A cloud host's VM hostname is noise; its provisioner names it instead.
func TestLabelPrefersTheProvisionedName(t *testing.T) {
	t.Setenv(LabelEnv, "  widgets  ")
	if got := Label(); got != "widgets" {
		t.Fatalf("Label() = %q, want widgets", got)
	}
	t.Setenv(LabelEnv, " ")
	host, _ := os.Hostname()
	if want := strings.TrimSuffix(host, ".local"); host != "" && Label() != want {
		t.Fatalf("Label() = %q, want hostname %q", Label(), want)
	}
}
