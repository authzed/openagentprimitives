// EXTERNAL test package (webui_test): the contract test imports
// pkg/web/webui/registry, which imports pkg/web/webui — an internal `package webui`
// test would form an import cycle. As an external test it depends on webui,
// registry, and webassets with no cycle, and uses the exported
// webassets.Manifest() rather than the unexported loadManifest().
package webui_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
)

// The built-in `system` app MUST always be present in the committed web
// manifest: it is the design-system page every non-React browser response
// renders through, so its absence breaks error and notice pages everywhere.
//
// That is the ONLY live assertion here, and deliberately so. The Page.App loop
// below cannot bite in this test binary and is kept only so a future in-package
// UI is not silently skipped: registry.All() is empty here (no WebUI package is
// imported), and — the part that is easy to get wrong — **blank-importing them
// would still assert nothing**, because the loop calls Routes(nil) and every
// Page-bearing WebUI fails closed on nil/uncastable Deps and returns no routes
// at all (chat.go, artifactview.go, sessionview.go, adminui.go; adminui
// additionally requires a non-empty AdmindBaseURL/AdmindToken).
//
// The assertion that does bite lives where real Deps already exist, over the
// blank imports that decide the actual served set:
// internal/cmd/webd's TestRegisteredPageAppsResolveInBuiltManifest.
func TestSystemAppResolvesInManifest(t *testing.T) {
	mf, err := webassets.Manifest()
	require.NoError(t, err, "embedded manifest must parse")
	assert.Contains(t, mf, "system", "built-in system app must be in the manifest")

	for _, ui := range registry.All() {
		for _, rt := range ui.Routes(nil) {
			if rt.Page == nil {
				continue
			}
			assert.Containsf(t, mf, rt.Page.App,
				"WebUI %q route %q references appKey %q absent from pkg/web/webui/webassets/dist/manifest.json — run `mage web:build`",
				ui.Name(), rt.Pattern, rt.Page.App)
		}
	}
}
