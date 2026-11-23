package rpc_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/repogoapp/host/internal/rpc"
)

type pin struct {
	ChatID string `json:"chat_id" doc:"The chat"`
	Pinned bool   `json:"pinned"`
}

func ack(context.Context, rpc.Caller, pin) (rpc.Ack, error) { return rpc.OK, nil }

// A Tool says whether it reads, is open to every caller, and only a Read is live;
// anything else is a wiring bug caught when the host starts.
func TestToolOptionsAreChecked(t *testing.T) {
	for name, opts := range map[string][]rpc.Option{
		"no effect":    {rpc.Tool("x")},
		"effect alone": {rpc.Write},
		"live alone":   {rpc.LiveOn("t.changed")},
		"local tool":   {rpc.Tool("x"), rpc.Read, rpc.Local},
		"paired tool":  {rpc.Tool("x"), rpc.Write, rpc.Paired},
		"a live write": {rpc.Tool("x"), rpc.Write, rpc.LiveOn("t.changed")},
		"mcp alone":    {rpc.MCP, rpc.Write},
		"child rule":   {rpc.Tool("x"), rpc.Write, rpc.NotFromChild},
		"remote rule":  {rpc.Tool("x"), rpc.Write, rpc.NotRemote},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registered")
				}
			}()
			rpc.Add(rpc.New(slog.New(slog.DiscardHandler)), "t.pin", ack, opts...)
		})
	}
}

// Tools lists only Tool methods, named for MCP, with their params as a schema.
func TestToolsListsToolMethods(t *testing.T) {
	r := rpc.New(slog.New(slog.DiscardHandler))
	rpc.Add(r, "t.pin", ack, rpc.Tool("Pin it."), rpc.Write)
	rpc.Add(r, "t.list", ack, rpc.Tool("List them."), rpc.Read, rpc.LiveOn("t.changed"))
	rpc.Add(r, "t.hidden", ack)
	b, err := r.Tools()
	if err != nil {
		t.Fatal(err)
	}
	var defs []rpc.ToolDef
	if err := json.Unmarshal(b, &defs); err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != "t_pin" || defs[0].ReadOnly || defs[1].Name != "t_list" || !defs[1].ReadOnly {
		t.Fatalf("defs %+v", defs)
	}
	chatID := defs[1].InputSchema["properties"].(map[string]any)["chat_id"].(map[string]any)
	if chatID["type"] != "string" || chatID["description"] != "The chat" {
		t.Fatalf("chat_id %v", chatID)
	}
}

// Every caller meets one check on the router's path: what each declaration
// lets in, as .plans' access matrix draws it.
func TestTheRouterAllowsEachCallerWhatItsMethodDeclares(t *testing.T) {
	r := rpc.New(slog.New(slog.DiscardHandler))
	rpc.Add(r, "t.plain", ack)
	rpc.Add(r, "t.local", ack, rpc.Local)
	rpc.Add(r, "t.paired", ack, rpc.Paired)
	rpc.Add(r, "t.plugins", ack, rpc.Plugins)
	rpc.Add(r, "t.card", ack, rpc.Tool("Cards and plugins."), rpc.Write)
	rpc.Add(r, "t.mcp", ack, rpc.MCP, rpc.Tool("Agents too."), rpc.Write)
	rpc.Add(r, "t.nochild", ack, rpc.MCP, rpc.NotFromChild, rpc.Tool("Not a child."), rpc.Write)
	rpc.Add(r, "t.noremote", ack, rpc.MCP, rpc.NotRemote, rpc.Tool("Not remote."), rpc.Write)

	callers := []struct {
		name string
		c    rpc.Caller
	}{
		{"phone", rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}},
		{"cli", rpc.Caller{Scope: rpc.ScopeLocal}},
		{"agent", rpc.Caller{Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{ChatID: "c1"}}},
		{"child", rpc.Caller{Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{ChatID: "c1", Child: true}}},
		{"remote", rpc.Caller{Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{ChatID: "c1", HostID: "H2"}}},
		{"plugin", rpc.Caller{Scope: rpc.ScopeLocal, Plugin: "p"}},
	}
	// Allowed, in the callers' order above.
	want := map[string]string{
		"t.plain":    "phone cli",
		"t.local":    "cli",
		"t.paired":   "phone",
		"t.plugins":  "phone cli plugin",
		"t.card":     "phone cli plugin",
		"t.mcp":      "phone cli agent child remote plugin",
		"t.nochild":  "phone cli agent remote plugin",
		"t.noremote": "phone cli agent child plugin",
	}
	for method, allowed := range want {
		got := ""
		for _, c := range callers {
			if _, err := r.Call(context.Background(), c.c, method, json.RawMessage(`{}`)); err == nil {
				got += " " + c.name
			}
		}
		if got != " "+allowed {
			t.Errorf("%s allows%s, want %s", method, got, allowed)
		}
	}
}
