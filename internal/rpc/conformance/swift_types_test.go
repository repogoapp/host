package conformance_test

import (
	"testing"

	"github.com/repogo/host/internal/swiftcontract"
	"github.com/repogo/host/internal/testhost"
)

const swiftTypes = "../../../../swift/Packages/RemoteTransport/Sources/HostContract/HostAPI.swift"

// Explicit exports keep unrelated feature contracts out of the Clip.
func TestSwiftTypesMatchRouter(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../../swift")
	testhost.Golden(t, swiftTypes, []byte(swiftcontract.Methods(newHost(t).router.Specs())), *update, updateHint)
}
