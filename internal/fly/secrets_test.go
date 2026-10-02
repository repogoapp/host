package fly

import (
	"context"
	"reflect"
	"testing"
)

func TestListSecrets(t *testing.T) {
	run := func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		if want := []string{"secrets", "list", "-a", "web", "--json"}; !reflect.DeepEqual(args, want) {
			t.Fatalf("args %v", args)
		}
		return []byte(`[{"name":"DATABASE_URL","digest":"ec6c98f63b8eb8ad","status":"Deployed"}]`), nil
	}
	got, err := listSecretsWith(t.Context(), "web", run)
	if err != nil || !reflect.DeepEqual(got, []Secret{{Name: "DATABASE_URL"}}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The value goes on stdin; staging leaves the machines alone until a deploy.
func TestSetSecretStagesUnlessDeploying(t *testing.T) {
	for deploy, flag := range map[bool]string{false: "--stage", true: "--detach"} {
		var gotArgs []string
		var gotIn []byte
		run := func(_ context.Context, args []string, stdin []byte) ([]byte, error) {
			gotArgs, gotIn = args, stdin
			return nil, nil
		}
		if err := setSecretWith(t.Context(), "web", "TOKEN", "a=b # c", deploy, run); err != nil {
			t.Fatal(err)
		}
		if want := []string{"secrets", "set", "TOKEN=-", "-a", "web", flag}; !reflect.DeepEqual(gotArgs, want) || string(gotIn) != "a=b # c" {
			t.Fatalf("args %v stdin %q", gotArgs, gotIn)
		}
	}
}

func TestUnsetSecret(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, args []string, _ []byte) ([]byte, error) { gotArgs = args; return nil, nil }
	if err := unsetSecretWith(t.Context(), "web", "TOKEN", false, run); err != nil {
		t.Fatal(err)
	}
	if want := []string{"secrets", "unset", "TOKEN", "-a", "web", "--stage"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args %v", gotArgs)
	}
}
