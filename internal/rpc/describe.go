package rpc

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/repogoapp/host/internal/wiredoc"
)

// Describe renders the vocabulary as Markdown, one section per method with
// who may call it and its params and result, read off the handlers' types.
func (r *Router) Describe() string {
	var b strings.Builder
	b.WriteString("# Host methods\n\n")
	b.WriteString("Generated from the router by `go test ./internal/rpc/conformance -update`. ")
	b.WriteString("Each is a JSON-RPC request whose method is the heading; the result is always an object.\n")
	b.WriteString("Fields are additive only: a breaking change is a new method.\n")
	for _, s := range r.Specs() {
		b.WriteString("\n## `" + s.Name + "`\n\n")
		switch s.Access {
		case LocalOnly:
			b.WriteString("Local only: refused to a paired device.\n\n")
		case PairedOnly:
			b.WriteString("Paired devices only: refused at the machine.\n\n")
		}
		if s.Unpaired {
			b.WriteString("Unpaired: reachable before pairing, guarded by the pairing code.\n\n")
		}
		if s.Detached {
			b.WriteString("Detached: runs to completion if the caller disconnects.\n\n")
		}
		if s.Tool != "" {
			b.WriteString("Tool (" + s.Effect.String() + "): " + s.Tool + "\n\n")
		}
		if len(s.LiveOn) > 0 {
			b.WriteString("Live on: `" + strings.Join(s.LiveOn, "`, `") + "`\n\n")
		}
		b.WriteString("Params:\n\n" + fieldList(s.Params) + "\nResult:\n\n" + fieldList(s.Result))
	}
	return b.String()
}

func fieldList(t reflect.Type) string {
	if list := wiredoc.List(t); list != "" {
		return list
	}
	return "- none\n"
}

// Examples renders every method's result as the files the client's tests decode.
func (r *Router) Examples() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, s := range r.Specs() {
		if err := wiredoc.Examples(out, s.Name, s.Result); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ToolDef is one Tool method as the repogo MCP offers it to agents.
type ToolDef struct {
	Name        string         `json:"name"`
	Method      string         `json:"method"`
	Description string         `json:"description"`
	ReadOnly    bool           `json:"read_only"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolName is a method's tool name: MCP clients take letters, digits, _ and -.
func ToolName(method string) string { return strings.ReplaceAll(method, ".", "_") }

// Tools renders every Tool method as the file the repogo MCP embeds, so what
// agents see is reviewed in a diff and stays in step with the handlers.
func (r *Router) Tools() ([]byte, error) {
	defs := []ToolDef{}
	for _, s := range r.Specs() {
		if s.Tool == "" {
			continue
		}
		defs = append(defs, ToolDef{
			Name: ToolName(s.Name), Method: s.Name, Description: s.Tool,
			ReadOnly: s.Effect == Reads, InputSchema: wiredoc.Schema(s.Params),
		})
	}
	b, err := json.MarshalIndent(defs, "", "  ")
	return append(b, '\n'), err
}
