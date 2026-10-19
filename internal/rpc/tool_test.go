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
