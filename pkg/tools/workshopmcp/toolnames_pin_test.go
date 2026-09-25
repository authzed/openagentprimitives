package workshopmcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestToolNamesPinnedForUnsanctionedFixture pins the six tool-name constants
// that internal/cmd/workshop/unsanctioned_test.go carries as LITERALS.
//
// That test is the named security test for "an unsanctioned builder class gets
// zero workshop tools", and its positive control depends on the withheld names
// being the real ones rather than typos. Before these tools moved into this
// package the two files shared the same symbols, so a rename either propagated
// or failed to compile — the control was true BY CONSTRUCTION. The constants
// are unexported (deliberately: this package exports only Server, NewServer,
// Register and WorkshopIdentity), so that test now spells them out, which
// makes the control true only by coincidence.
//
// This test restores the tripwire at the rename site: change a constant here
// and this fails, naming the file that must change in lockstep. It is the same
// reflex as tools_declaration_test.go, which exists because a hand-maintained
// list can silently diverge from the real Go constants.
func TestToolNamesPinnedForUnsanctionedFixture(t *testing.T) {
	const dependent = "keep in sync with internal/cmd/workshop/unsanctioned_test.go's literal fixture"

	assert.Equal(t, "inventory", toolInventory, dependent)
	assert.Equal(t, "apply", toolWorkshopApply, dependent)
	assert.Equal(t, "render_summary", toolRenderSummary, dependent)
	assert.Equal(t, "validate_spec", toolValidateSpec, dependent)
	assert.Equal(t, "request_credential", toolRequestCredential, dependent)
	assert.Equal(t, "close_others", toolCloseOthers, dependent)
}
