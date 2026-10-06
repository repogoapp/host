package clitool

import "testing"

func TestAnAuthEnvVariableSignsTheToolIn(t *testing.T) {
	spec := Spec{AuthEnv: []string{"REPOGO_TEST_API_KEY"}}
	t.Setenv("REPOGO_TEST_API_KEY", "")
	if authed, _ := probeAuth(t.Context(), spec, t.TempDir()); authed {
		t.Fatal("an empty variable signed the tool in")
	}
	t.Setenv("REPOGO_TEST_API_KEY", "key")
	authed, source := probeAuth(t.Context(), spec, t.TempDir())
	if !authed || source != "env:REPOGO_TEST_API_KEY" {
		t.Fatalf("got %v %q, want signed in from env:REPOGO_TEST_API_KEY", authed, source)
	}
}
