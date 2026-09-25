package sessionrelease

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// TestCategory_registeredInteractiveOwnerGated mirrors
// pkg/channels/channelinteractions/categories/categories_test.go's per-row
// style (TestPortalAndPermissionRegistered): Get the real registered row and
// assert on its real fields, rather than inventing a parallel Category type
// with its own Name()/IsInteractive() methods — channelinteractions.Category
// is a declarative struct (Name/Notice/Deciders/… fields), not an interface,
// and every one of the 33 existing categories is tested this way.
func TestCategory_registeredInteractiveOwnerGated(t *testing.T) {
	c, ok := channelinteractions.Get(CategoryName)
	require.True(t, ok, "importing this package registers session_release")
	assert.False(t, c.Notice, "a release needs a human decision, not a notice")
	assert.Equal(t, channelinteractions.DecideOwner, c.Deciders, "releasing is an owner decision")
	assert.Equal(t, channelinteractions.ResumeNone, c.Resume, "no runner is blocked on this decision — a held session has none")
	assert.Equal(t, "", c.Park, "a held session has already been parked by lifecycle.Held; there is no phase for this category to occupy")
}

// TestFailsClosedOnTimeout_true: an unanswered release request is NOT a
// release. channelinteractions.Category has no timeout field of its own (see
// the package doc for why); this is the property test for the invariant the
// SessionHold reconciler actually enforces (pkg/controllers/sessionhold never
// auto-transitions status.phase to Released except on an explicit approval).
func TestFailsClosedOnTimeout_true(t *testing.T) {
	assert.True(t, FailsClosedOnTimeout(), "an unanswered release request is NOT a release")
}

func TestBuildCard_carriesNoAgentAuthoredField(t *testing.T) {
	card := BuildCard(CardInput{
		SessionName:    "demo-session",
		Reason:         "12 consecutive out-of-ceiling calls",
		Source:         "tripper/plangate-denial-streak",
		SnapshotHandle: "hold-h1",
	})
	require.NotEmpty(t, card.Body)
	assert.NotContains(t, card.Fields, "why",
		"on this card the agent is the SUBJECT of the decision, not the requester; it gets no voice")
}

func TestBuildCard_severityIsSevere(t *testing.T) {
	card := BuildCard(CardInput{SessionName: "demo-session", Reason: "r", Source: "manual"})
	assert.Equal(t, "severe", card.Severity,
		"a categorically different decision must also LOOK different")
}

func TestBuildCard_evidenceAndSnapshotAreOptionalButRenderedWhenPresent(t *testing.T) {
	bare := BuildCard(CardInput{SessionName: "demo-session", Reason: "r", Source: "manual"})
	assert.NotContains(t, bare.Fields, "evidence")
	assert.NotContains(t, bare.Fields, "snapshot")

	full := BuildCard(CardInput{
		SessionName:    "demo-session",
		Reason:         "r",
		Source:         "manual",
		Evidence:       "3 refused calls to git_repo/write",
		SnapshotHandle: "hold-h1",
	})
	assert.Equal(t, "3 refused calls to git_repo/write", full.Fields["evidence"])
	assert.Equal(t, "hold-h1", full.Fields["snapshot"])
}
