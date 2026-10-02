package fly

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMachineSummary(t *testing.T) {
	got := readMachines(t.Context(), "api", func(_ context.Context, args []string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"machine", "list", "-a", "api", "--json"}) {
			t.Errorf("args = %v", args)
		}
		return []byte(`[
			{"state":"started","region":"sjc","config":{"guest":{"cpus":2,"memory_mb":1024}}},
			{"state":"stopped","region":"iad","config":{"guest":{"cpus":1,"memory_mb":512}}},
			{"state":"started","region":"iad","config":{"guest":{"cpus":1,"memory_mb":512}}},
			{"state":"destroyed","region":"lhr","config":{"guest":{"cpus":8,"memory_mb":8192}}}
		]`), nil
	})
	want := &MachineSummary{Count: 3, Running: 2, State: "mixed", Regions: []string{"iad", "sjc"}, CPUs: 4, MemoryMB: 2048}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestMachineSummaryEmptyAndPartial(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want *MachineSummary
	}{
		{"empty", `[]`, &MachineSummary{Regions: []string{}}},
		{"stopped", `[{"state":"stopped","region":"iad","config":{"guest":{"cpus":2,"memory_mb":512}}}]`,
			&MachineSummary{Count: 1, State: "stopped", Regions: []string{"iad"}, CPUs: 2, MemoryMB: 512}},
		{"partial capacity", `[{"state":"started","region":"iad","config":{"guest":{"cpus":2,"memory_mb":1024}}},{"state":"started","region":"iad","config":null}]`,
			&MachineSummary{Count: 2, Running: 2, State: "started", Regions: []string{"iad"}}},
		{"unknown state", `[{"region":"iad"}]`, &MachineSummary{Count: 1, State: "unknown", Regions: []string{"iad"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readMachines(t.Context(), "api", func(context.Context, []string) ([]byte, error) { return []byte(tc.json), nil })
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("summary = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMachineFailureKeepsDeployment(t *testing.T) {
	got, err := readWith(t.Context(), "api", func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "machine":
			return nil, errors.New("not available\nlong diagnostic")
		case "releases":
			return []byte(`[{"Version":3,"Status":"complete","CreatedAt":"2026-10-01T19:00:00Z"}]`), nil
		default:
			return []byte(`[]`), nil
		}
	})
	if err != nil || got.Latest == nil || got.Latest.Version != 3 || got.Machines == nil || got.Machines.Error != "not available" {
		t.Fatalf("app = %+v, error = %v", got, err)
	}
}

func TestMachineMalformedResponseIsNotEmptySuccess(t *testing.T) {
	got := readMachines(t.Context(), "api", func(context.Context, []string) ([]byte, error) { return []byte(`{}`), nil })
	if got.Error == "" {
		t.Fatalf("summary = %+v", got)
	}
}
