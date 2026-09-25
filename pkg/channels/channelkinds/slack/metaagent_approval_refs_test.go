// pkg/channels/channelkinds/slack/metaagent_approval_refs_test.go
//
// Tests that Kind.RememberMetaagentApproval writes into the same per-process
// shared metaagentApprovalRefs cache the listener reads from, so a write by
// internal/cmd/channelsd (via the Kind) is visible to the listener's Show Details path.
package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestRememberMetaagentApproval_VisibleToListener verifies that a ref written
// via Kind.RememberMetaagentApproval is retrievable both from the Kind's shared
// cache and from a listener constructed from that same Kind — confirming both
// sides point at the same *metaagentApprovalRefCache instance.
func TestRememberMetaagentApproval_VisibleToListener(t *testing.T) {
	k := &Kind{}
	ref := MetaagentApprovalRef{
		RequestID:       "req-share",
		Requester:       "U_ALICE",
		Verbatim:        "allow read on linear",
		ApproverSummary: "Alice wants to read Linear issues.",
		ColdStart:       true,
		CleanedTask:     "summarize L-140",
	}

	k.RememberMetaagentApproval(ref)

	// The Kind's shared cache has it.
	got, ok := k.sharedMetaagentApprovalRefs().get("req-share")
	require.True(t, ok, "Kind's shared cache must contain the ref")
	assert.Equal(t, ref, got, "Kind's shared cache must return the exact ref")

	// A listener built from the same Kind reads the same instance.
	l := k.NewListener(channelkinds.Deps{}).(*slackListener)
	require.NotNil(t, l.metaagentApprovalRefs, "listener must have a metaagentApprovalRefs cache")
	require.Same(t, k.sharedMetaagentApprovalRefs(), l.metaagentApprovalRefs,
		"listener and Kind must share the same *metaagentApprovalRefCache instance")

	lGot, lOK := l.metaagentApprovalRefs.get("req-share")
	require.True(t, lOK, "listener's cache must contain the ref written via the Kind")
	assert.Equal(t, ref.Verbatim, lGot.Verbatim, "listener sees the real Verbatim")
	assert.Equal(t, ref.CleanedTask, lGot.CleanedTask, "listener sees the real CleanedTask")
}

// TestRememberMetaagentApproval_EmptyRequestIDGuard verifies that a ref with an
// empty RequestID is not written (and does not panic).
func TestRememberMetaagentApproval_EmptyRequestIDGuard(t *testing.T) {
	k := &Kind{}
	assert.NotPanics(t, func() {
		k.RememberMetaagentApproval(MetaagentApprovalRef{RequestID: "", Verbatim: "x"})
	}, "empty RequestID must not panic")

	_, ok := k.sharedMetaagentApprovalRefs().get("")
	assert.False(t, ok, "empty-RequestID ref must not be written")
}
