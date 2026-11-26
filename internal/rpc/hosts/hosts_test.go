package hosts

import (
	"context"
	"testing"

	"github.com/repogoapp/host/internal/remotehost"
	"github.com/repogoapp/host/internal/rpc"
)

// The user's own devices see every SSH connection; an agent, a plugin or a
// shell sees only the ones the user switched on.
func TestOnlyThePhoneSeesConnectionsThatAreOff(t *testing.T) {
	svc := remotehost.NewService(remotehost.Deps{
		SSHConfig: func() []byte { return []byte("Host studio\nHost client-prod\n") },
		Changed:   func() {},
		Run: func(context.Context, string, string) ([]byte, []byte, int) {
			return []byte(`{"host_id": "H1"}`), nil, 0
		},
	})
	ctx := context.Background()
	if _, err := svc.SetListing(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authorize(ctx, "studio", true); err != nil {
		t.Fatal(err)
	}
	d := Deps{Hosts: svc}

	phone, _ := d.list(ctx, rpc.Caller{Scope: rpc.ScopeRemote, Device: "phone"}, ListParams{})
	if len(phone.Hosts) != 2 {
		t.Fatalf("the phone sees %+v, want both connections", phone.Hosts)
	}
	for name, caller := range map[string]rpc.Caller{
		"agent":  {Scope: rpc.ScopeLocal, Device: "phone", Agent: &rpc.AgentCaller{ChatID: "claude:s1"}},
		"plugin": {Scope: rpc.ScopeRemote, Plugin: "notes"},
		"shell":  {Scope: rpc.ScopeLocal},
	} {
		got, _ := d.list(ctx, caller, ListParams{})
		if len(got.Hosts) != 1 || got.Hosts[0].ID != "studio" {
			t.Errorf("%s sees %+v, want only studio", name, got.Hosts)
		}
	}
}
