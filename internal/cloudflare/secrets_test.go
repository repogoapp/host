package cloudflare

import (
	"context"
	"reflect"
	"testing"
)

func TestListSecrets(t *testing.T) {
	run := func(_ context.Context, env, args []string, _ []byte) ([]byte, error) {
		if !reflect.DeepEqual(env, []string{"CLOUDFLARE_ACCOUNT_ID=acc_1"}) {
			t.Fatalf("env %v", env)
		}
		if want := []string{"secret", "list", "--name", "api", "--format", "json"}; !reflect.DeepEqual(args, want) {
			t.Fatalf("args %v", args)
		}
		return []byte(`[{"name":"TOKEN","type":"secret_text"}]`), nil
	}
	got, err := listSecretsWith(t.Context(), "api", "acc_1", run)
	if err != nil || !reflect.DeepEqual(got, []Secret{{Name: "TOKEN"}}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// Without deploy the secret goes into a version that isn't deployed; a card
// without an account uses the login's only one.
func TestPutSecret(t *testing.T) {
	for deploy, want := range map[bool][]string{
		false: {"versions", "secret", "put", "TOKEN", "--name", "api"},
		true:  {"secret", "put", "TOKEN", "--name", "api"},
	} {
		var gotArgs, gotEnv []string
		var gotIn []byte
		run := func(_ context.Context, env, args []string, stdin []byte) ([]byte, error) {
			if args[0] == "whoami" {
				return []byte(`{"accounts":[{"id":"acc_only"}]}`), nil
			}
			gotEnv, gotArgs, gotIn = env, args, stdin
			return nil, nil
		}
		if err := putSecretWith(t.Context(), "api", "", "TOKEN", "s3cret", deploy, run); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotArgs, want) || string(gotIn) != "s3cret" || gotEnv[0] != "CLOUDFLARE_ACCOUNT_ID=acc_only" {
			t.Fatalf("args %v env %v stdin %q", gotArgs, gotEnv, gotIn)
		}
	}
}

func TestDeleteSecret(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, _, args []string, _ []byte) ([]byte, error) { gotArgs = args; return nil, nil }
	if err := deleteSecretWith(t.Context(), "api", "acc_1", "TOKEN", true, run); err != nil {
		t.Fatal(err)
	}
	if want := []string{"secret", "delete", "TOKEN", "--name", "api"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args %v", gotArgs)
	}
}
