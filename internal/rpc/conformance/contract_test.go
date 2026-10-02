package conformance_test

import (
	"flag"
	"testing"

	"github.com/repogo/host/internal/testhost"
)

var update = flag.Bool("update", false, "rewrite docs/host-methods.md, docs/methods and RemoteTransport's HostAPI.swift from the router")

const (
	methodsDoc = "../../../docs/host-methods.md"
	methodsDir = "../../../docs/methods"
	updateHint = "run: go test ./internal/rpc/conformance -update"
)

// docs/host-methods.md is the method table for humans, generated from the
// handlers' own types, so reading it is reading the code.
func TestMethodDocMatchesRouter(t *testing.T) {
	testhost.Golden(t, methodsDoc, []byte(newHost(t).router.Describe()), *update, updateHint)
}

// docs/methods holds an example of every result, which RemoteTransport's tests
// decode with the app's types: a field renamed on either side fails there.
func TestMethodExamplesMatchRouter(t *testing.T) {
	want, err := newHost(t).router.Examples()
	if err != nil {
		t.Fatal(err)
	}
	testhost.GoldenDir(t, methodsDir, want, *update, updateHint)
}
