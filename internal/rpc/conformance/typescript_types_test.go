package conformance_test

import (
	"testing"

	"github.com/repogoapp/host/internal/clientcontract"
	"github.com/repogoapp/host/internal/testhost"
)

const typescriptTypes = "../../../../desktop/src/shared/HostAPI.ts"

// The desktop contract names what the Swift one does, keyed by the wire's own fields.
func TestTypeScriptTypesMatchRouter(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../../desktop")
	testhost.Golden(t, typescriptTypes, []byte(clientcontract.TypeScript(newHost(t).router.Specs())), *update, updateHint)
}
