package conformance_test

import (
	"testing"

	"github.com/repogoapp/host/internal/clientcontract"
	"github.com/repogoapp/host/internal/testhost"
)

const kotlinTypes = "../../../../android/Packages/RemoteTransport/Sources/HostContract/HostAPI.kt"

// The Android contract names what the Swift one does, so a port reads the same types.
func TestKotlinTypesMatchRouter(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../../android")
	testhost.Golden(t, kotlinTypes, []byte(clientcontract.Kotlin(newHost(t).router.Specs())), *update, updateHint)
}
