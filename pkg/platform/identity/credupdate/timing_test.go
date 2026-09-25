package credupdate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
)

// TestCredentialUpdateTimingOrderingHoldsAcrossAllThreeBinaries asserts the
// "tool wait <= park TTL <= link lifetime" ordering timing.go documents, read
// from the THREE consuming packages' own exported knobs rather than from
// credupdate's constants -- so it fails if any one of them stops deriving
// from the shared base, not merely if the base itself changes.
//
// Each site also carries a compile-time assertion of its own half of the
// ordering (see timing.go), which is the stronger guard: an ordering-breaking
// edit there fails the build outright. This test is the belt to that
// suspenders -- it is the single place a reader can see the WHOLE relation at
// once, and it is the only check that survives a site swapping its
// `const _ = uint64(...)` guard out along with the value.
//
// This lives in an EXTERNAL test package (credupdate_test) on purpose: it
// imports two packages that themselves import credupdate, which only an
// external test package may do without an import cycle.
func TestCredentialUpdateTimingOrderingHoldsAcrossAllThreeBinaries(t *testing.T) {
	// The runner's tool floor. credupdate.DefaultToolWait is what
	// pkg/agent/tool/meta/capability's (unexported) credentialUpdateMinWait
	// aliases; that package asserts the alias holds in its own test.
	toolWait := credupdate.DefaultToolWait
	parkTTL := credentialupdaterequest.DefaultCredentialUpdateIdleTTL
	linkLifetime := pipeline.DefaultCredentialUpdateLinkTimeout

	assert.LessOrEqual(t, toolWait, parkTTL,
		"the runner tool must give up no LATER than the operator expires the request, "+
			"or it blocks on a decision that can never arrive")
	assert.LessOrEqual(t, parkTTL, linkLifetime,
		"the card's signed link must stay valid at least as long as the request stays Open, "+
			"or a human who clicks near the deadline lands on a dead link")

	assert.Positive(t, toolWait, "a zero tool wait would make the tool return before anyone could act")
	assert.Positive(t, parkTTL, "a zero park TTL would expire every request on its first reconcile")
}
