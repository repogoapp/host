package emit

import (
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/repogo/host/internal/wiredoc"
)

// The catalog is every event producers declare in init; only the test that
// the client handles the same set reads it.

var (
	catalogMu sync.Mutex
	catalog   = map[string]Event{}
)

// Register adds events to the catalog. Registering the same method twice is a
// programming error and panics, so two producers cannot silently share a name.
func Register(events ...Event) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	register(catalog, events...)
}

func register(into map[string]Event, events ...Event) {
	for _, ev := range events {
		if _, dup := into[ev.Method()]; dup {
			panic("emit: " + ev.Method() + " registered twice")
		}
		into[ev.Method()] = ev
	}
}

// Catalog returns every registered event, ordered by method name.
func Catalog() []Event {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	out := make([]Event, 0, len(catalog))
	for _, ev := range catalog {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method() < out[j].Method() })
	return out
}

// Describe renders the catalog as Markdown: one section per method with its
// JSON fields, so the wire contract can be read without reading Go.
func Describe() string {
	var b strings.Builder
	b.WriteString("# Host events\n\n")
	b.WriteString("Generated from the Go event catalog by `go test ./internal/emit -update`. ")
	b.WriteString("Each is a JSON-RPC notification whose method is the heading and whose params are the fields.\n")
	b.WriteString("Fields are additive only: a breaking change is a new method.\n")
	for _, ev := range Catalog() {
		b.WriteString("\n## `" + ev.Method() + "`\n\n")
		if s, ok := ev.(Stateful); ok {
			b.WriteString("Stateful, key `" + s.StateKey() + "`: the last one is handed to a device joining the room; `revision` orders them within an `epoch`.\n\n")
		} else {
			b.WriteString("Signal: not replayed; a missed one is recovered by refetching.\n\n")
		}
		b.WriteString(wiredoc.List(reflect.TypeOf(ev)))
	}
	return b.String()
}

// Examples renders every event as the files the client's tests decode.
func Examples() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, ev := range Catalog() {
		if err := wiredoc.Examples(out, ev.Method(), reflect.TypeOf(ev)); err != nil {
			return nil, err
		}
	}
	return out, nil
}
