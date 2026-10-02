//go:build native

package agents_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/shipping"
)

// usageProvider is a provider both parsers can read usage with.
type usageProvider interface {
	shipping.Provider
	shipping.FileParser
}

// A request the two parsers key differently would count twice after a build
// switch, so they must agree on every record. -native-local adds this
// machine's own history to the fixtures.
func TestNativeUsageParity(t *testing.T) {
	base := t.TempDir()
	usageFixtures(t, base)
	fixtures := []usageProvider{claude.New(agent.Dependencies{Root: base}), codex.New(agent.Dependencies{Root: base})}
	compareUsage(t, fixtures, 0)
	if *nativeLocal {
		compareUsage(t, []usageProvider{claude.New(agent.Dependencies{}), codex.New(agent.Dependencies{})}, 5)
	}
}

func compareUsage(t *testing.T, providers []usageProvider, maxReported int) {
	t.Helper()
	for _, p := range providers {
		var files, records, mismatches int
		for _, root := range p.UsageRoots() {
			_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
					return nil
				}
				want := goUsage(t, p, path)
				got, ok := p.ParseUsageFile(path)
				if !ok {
					t.Errorf("%s: native could not read %s", p.Kind(), path)
					return nil
				}
				files++
				records += len(want)
				if len(got) == 0 && len(want) == 0 {
					return nil
				}
				if !reflect.DeepEqual(got, want) {
					mismatches++
					if maxReported == 0 || mismatches <= maxReported {
						t.Errorf("%s: %s:\n go     %+v\n native %+v", p.Kind(), path, want, got)
					}
				}
				return nil
			})
		}
		t.Logf("%s: %d files, %d requests, %d files differ", p.Kind(), files, records, mismatches)
	}
}

func goUsage(t *testing.T, p shipping.Provider, path string) []shipping.Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parse := p.NewUsageParser()
	var out []shipping.Record
	for {
		i := strings.IndexByte(string(raw), '\n')
		if i < 0 {
			return out
		}
		if rec := parse(raw[:i+1]); rec != nil {
			out = append(out, *rec)
		}
		raw = raw[i+1:]
	}
}
