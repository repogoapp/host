package pairing

import (
	"context"
	"errors"
	"testing"

	"github.com/repogoapp/host/internal/release"
	"github.com/repogoapp/host/internal/rpc"
)

// A release host refuses a reusable invite before touching the pairer.
func TestReleaseHostsRefuseReusableInvites(t *testing.T) {
	version := release.Version
	release.Version = "9.9.9"
	t.Cleanup(func() { release.Version = version })

	if _, err := (Deps{}).reusable(context.Background(), rpc.Caller{}, ReusableParams{Hours: 24}); !errors.Is(err, ErrReusableDevOnly) {
		t.Fatalf("reusable on a release host: %v", err)
	}
}
