package conformance_test

import (
	"testing"

	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/testhost"
)

const hostTools = "../../repogomcp/hosttools.json"

// The repogo MCP offers the methods marked rpc.Tool from this file, so what
// agents are told is the handlers' own description and params.
func TestHostToolsMatchRouter(t *testing.T) {
	want, err := newHost(t).router.Tools()
	if err != nil {
		t.Fatal(err)
	}
	testhost.Golden(t, hostTools, want, *update, updateHint)
	if !*update && string(repogomcp.HostToolsFile()) != string(want) {
		t.Fatalf("the repogo MCP embeds a stale %s; %s", hostTools, updateHint)
	}
}

// A card reloads on a Read's LiveOn events, so each must be one the host sends.
func TestLiveOnEventsExist(t *testing.T) {
	sent := map[string]bool{}
	for _, ev := range emit.Catalog() {
		sent[ev.Method()] = true
	}
	for _, s := range newHost(t).router.Specs() {
		for _, ev := range s.LiveOn {
			if !sent[ev] {
				t.Errorf("%s is live on %s, which no package registers with emit", s.Name, ev)
			}
		}
	}
}
