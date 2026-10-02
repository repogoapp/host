package cloudenv

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/vercel"
)

// Every provider's list comes out in one shape; only Vercel has targets.
func TestListIsOneShape(t *testing.T) {
	s := &Service{p: Providers{
		VercelList: func(context.Context, string, string) ([]vercel.EnvVar, error) {
			return []vercel.EnvVar{{ID: "e1", Key: "API_URL", Targets: []string{"preview"}, GitBranch: "feat"}}, nil
		},
		FlyList: func(context.Context, string) ([]fly.Secret, error) { return []fly.Secret{{Name: "DATABASE_URL"}}, nil },
		WorkerList: func(_ context.Context, worker, account string) ([]cloudflare.Secret, error) {
			if worker != "api" || account != "acc_1" {
				t.Fatalf("worker %q account %q", worker, account)
			}
			return []cloudflare.Secret{{Name: "TOKEN"}}, nil
		},
	}}
	for _, c := range []struct {
		ref  Ref
		want List
	}{
		{Ref{"vercel", "team_a", "prj_1"}, List{Vars: []Var{{ID: "e1", Key: "API_URL", Targets: []string{"preview"}, GitBranch: "feat"}}, Targets: vercel.EnvTargets}},
		{Ref{"fly", "", "web"}, List{Vars: []Var{{ID: "DATABASE_URL", Key: "DATABASE_URL", Secret: true, Targets: []string{}}}, Targets: []string{}}},
		{Ref{"cloudflare", "acc_1", "api"}, List{Vars: []Var{{ID: "TOKEN", Key: "TOKEN", Secret: true, Targets: []string{}}}, Targets: []string{}}},
	} {
		got, err := s.List(t.Context(), c.ref)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %+v, %v", c.ref.Provider, got, err)
		}
	}
	if _, err := s.List(t.Context(), Ref{"netlify", "", "site"}); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}

// A Vercel write redeploys only when asked, and a failed redeploy still
// says the variable was saved.
func TestSetVercelRedeploysWhenAsked(t *testing.T) {
	redeploys := 0
	s := &Service{p: Providers{
		VercelSet: func(_ context.Context, w vercel.EnvWrite) (vercel.EnvVar, error) {
			return vercel.EnvVar{ID: "e1", Key: w.Key, Targets: w.Targets}, nil
		},
		VercelRedeploy: func(context.Context, string, string) error { redeploys++; return errors.New("no deployment") },
	}}
	ref := Ref{"vercel", "team_a", "prj_1"}
	if _, err := s.Set(t.Context(), ref, Write{Key: "API_URL", Value: "v", Targets: []string{"production"}}); err != nil || redeploys != 0 {
		t.Fatalf("err %v, redeploys %d", err, redeploys)
	}
	_, err := s.Set(t.Context(), ref, Write{Key: "API_URL", Value: "v", Targets: []string{"production"}, Deploy: true})
	if err == nil || err.Error() != "saved, but couldn't redeploy: no deployment" || redeploys != 1 {
		t.Fatalf("err %v, redeploys %d", err, redeploys)
	}
}

func TestSetPassesDeployToFlyAndWorkers(t *testing.T) {
	var got []string
	s := &Service{p: Providers{
		FlySet: func(_ context.Context, app, name, value string, deploy bool) error {
			got = append(got, app, name, value, map[bool]string{true: "deploy", false: "stage"}[deploy])
			return nil
		},
		WorkerPut: func(_ context.Context, worker, account, name, value string, deploy bool) error {
			got = append(got, worker, account, name, value, map[bool]string{true: "deploy", false: "stage"}[deploy])
			return nil
		},
	}}
	v, err := s.Set(t.Context(), Ref{"fly", "", "web"}, Write{Key: "TOKEN", Value: "a", Deploy: true})
	if err != nil || !reflect.DeepEqual(v, Var{ID: "TOKEN", Key: "TOKEN", Secret: true, Targets: []string{}}) {
		t.Fatalf("got %+v, %v", v, err)
	}
	if _, err := s.Set(t.Context(), Ref{"cloudflare", "acc_1", "api"}, Write{Key: "TOKEN", Value: "b"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"web", "TOKEN", "a", "deploy", "api", "acc_1", "TOKEN", "b", "stage"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

// What a provider can't do is refused before any CLI runs.
func TestSetRefuses(t *testing.T) {
	s := &Service{}
	for _, c := range []struct {
		ref Ref
		w   Write
	}{
		{Ref{"fly", "", "web"}, Write{Key: "BAD-KEY", Value: "v"}},
		{Ref{"fly", "", "web"}, Write{Key: "1KEY", Value: "v"}},
		{Ref{"fly", "", "web"}, Write{Key: "KEY", Value: "v", Targets: []string{"production"}}},
		{Ref{"cloudflare", "acc_1", "api"}, Write{Key: "KEY"}},
		{Ref{"vercel", "team_a", ""}, Write{Key: "KEY", Value: "v"}},
	} {
		if _, err := s.Set(t.Context(), c.ref, c.w); !errors.Is(err, errkind.ErrInvalid) {
			t.Fatalf("%+v %+v: err %v", c.ref, c.w, err)
		}
	}
	if _, err := s.Value(t.Context(), Ref{"fly", "", "web"}, "TOKEN"); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("value err %v", err)
	}
}

func TestRemove(t *testing.T) {
	var removed, redeployed bool
	s := &Service{p: Providers{
		VercelRemove:   func(_ context.Context, _, _, id string) error { removed = id == "e1"; return nil },
		VercelRedeploy: func(context.Context, string, string) error { redeployed = true; return nil },
	}}
	if err := s.Remove(t.Context(), Ref{"vercel", "team_a", "prj_1"}, "e1", true); err != nil || !removed || !redeployed {
		t.Fatalf("err %v removed %v redeployed %v", err, removed, redeployed)
	}
	if err := s.Remove(t.Context(), Ref{"vercel", "team_a", "prj_1"}, "", false); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err %v", err)
	}
}
