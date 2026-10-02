package rpc

import (
	"reflect"
	"strings"

	"github.com/repogo/host/internal/wiredoc"
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
