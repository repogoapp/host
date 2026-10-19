package conformance_test

import (
	"testing"

	"github.com/repogoapp/host/internal/clientcontract"
	"github.com/repogoapp/host/internal/testhost"
)

const swiftTypes = "../../../../swift/Packages/RemoteTransport/Sources/HostContract/HostAPI.swift"

// Explicit exports keep unrelated feature contracts out of the Clip.
func TestSwiftTypesMatchRouter(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../../swift")
	testhost.Golden(t, swiftTypes, []byte(clientcontract.Methods(newHost(t).router.Specs())), *update, updateHint)
}
