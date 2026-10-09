//go:build native

package agents_test

import (
	"flag"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
)

var nativeLocal = flag.Bool("native-local", false, "compare the native parser against the Go parser on local chat history")

// The content hash the store keeps is computed from these events, so the two
// parsers must agree on every field of every event or a build switch would
// bump every chat's generation and wipe every client's cache.
func TestNativeMatchesGoParser(t *testing.T) {
	if !*nativeLocal {
		t.Skip("pass -native-local to read local chat history")
	}
	for _, p := range []session.Provider{claude.New(agent.Dependencies{}).Sessions(), codex.New(agent.Dependencies{}).Sessions()} {
		metas, err := p.List()
		if err != nil {
			t.Fatalf("%s: list: %v", p.Kind(), err)
		}
		var files, events, mismatches int
		for _, m := range metas {
			for _, f := range m.Files() {
				want, _, err := session.ReadFile(f, 0, p, m.ID, 0)
				if err != nil {
					continue
				}
				got, err := p.(session.FileParser).ParseFile(f, m.ID)
				if err != nil {
					t.Errorf("%s: native failed on %s: %v", p.Kind(), f, err)
					continue
				}
				for i := range got {
					got[i].Seq = uint64(i + 1)
					got[i].SessionID = m.ID
				}
				files++
				events += len(want)
				if diff := firstDiff(want, got); diff != "" {
					mismatches++
					if mismatches <= 5 {
						t.Errorf("%s: %s: %s", p.Kind(), f, diff)
					}
				}
			}
		}
		t.Logf("%s: %d files, %d events, %d files differ", p.Kind(), files, events, mismatches)
		if files == 0 {
			t.Logf("%s: no local sessions", p.Kind())
		}
	}
}

func firstDiff(want, got []agent.Event) string {
	if len(want) != len(got) {
		return fmt.Sprintf("event count: go %d, native %d", len(want), len(got))
	}
	for i := range want {
		a, b := want[i], got[i]
		// Input is compared as bytes: the store hashes the raw text.
		if a.Kind != b.Kind || a.TurnID != b.TurnID || a.At != b.At || a.Text != b.Text || a.Error != b.Error ||
			(a.Tool == nil) != (b.Tool == nil) ||
			(a.Tool != nil && (a.Tool.CallID != b.Tool.CallID || a.Tool.Name != b.Tool.Name || a.Tool.Output != b.Tool.Output ||
				a.Tool.IsError != b.Tool.IsError || string(a.Tool.Input) != string(b.Tool.Input) ||
				!reflect.DeepEqual(a.Tool.Result, b.Tool.Result))) {
			if !reflect.DeepEqual(a, b) {
				return fmt.Sprintf("event %d (%s): go %s | native %s", i, a.Kind, summarize(a), summarize(b))
			}
		}
	}
	return ""
}

func summarize(e agent.Event) string {
	s := fmt.Sprintf("turn=%q at=%d text=%.60q err=%q", e.TurnID, e.At, e.Text, e.Error)
	if e.Tool != nil {
		s += fmt.Sprintf(" tool{id=%q name=%q in=%.60q out=%.60q err=%v}", e.Tool.CallID, e.Tool.Name, string(e.Tool.Input), e.Tool.Output, e.Tool.IsError)
	}
	return s
}

func TestNativeFixtureParity(t *testing.T) {
	for _, p := range []session.Provider{&claude.Sessions{}, &codex.Sessions{}} {
		path := filepath.Join(string(p.Kind()), "testdata", "transcript.jsonl")
		want, _, err := session.ReadFile(path, 0, p, "fixture", 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.(session.FileParser).ParseFile(path, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			t.Fatalf("%s: empty fixture", p.Kind())
		}
		for i := range got {
			got[i].Seq = uint64(i + 1)
			got[i].SessionID = "fixture"
		}
		if !reflect.DeepEqual(want, got) {
			diff := firstDiff(want, got)
			if diff == "" {
				diff = fmt.Sprintf("go %+v | native %+v", want, got)
			}
			t.Fatalf("%s: %s", p.Kind(), diff)
		}
	}
}
