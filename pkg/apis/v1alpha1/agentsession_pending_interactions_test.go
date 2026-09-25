package v1alpha1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPendingInteractionsOwnedByApprovals guards that the new generic pending
// list is classified under OwnerApprovals (channelsd writes it) — the
// TestEveryStatusFieldHasExactlyOneOwner completeness guard fails otherwise.
func TestPendingInteractionsOwnedByApprovals(t *testing.T) {
	if !approvalOwnedStatusFields["pendingInteractions"] {
		t.Fatal("pendingInteractions must be owned by OwnerApprovals")
	}
	if operatorOwnedStatusFields["pendingInteractions"] {
		t.Fatal("pendingInteractions must not be operator-owned")
	}
}

// TestPendingInteractions_DeepCopyIsIndependent populates every field of a
// PendingInteraction, deep-copies the owning AgentSessionStatus, and asserts
// the copy starts out equal but mutating it (including the RequestedAt
// metav1.Time and the slice's backing array) does not affect the original —
// proving AgentSessionStatus.DeepCopy() actually deep-copies
// PendingInteractions rather than aliasing the slice/struct.
func TestPendingInteractions_DeepCopyIsIndependent(t *testing.T) {
	requestedAt := metav1.NewTime(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	orig := AgentSessionStatus{
		PendingInteractions: []PendingInteraction{
			{
				RequestID:        "req-demo-1",
				Category:         "content_inspection",
				ApproverSubject:  "agentsession:default/demo-session#approve",
				RequestRef:       "req-demo-1",
				RequestedAt:      requestedAt,
				AgentDisplayName: "demo-agent",
				Summary:          "example.com content flagged for review",
			},
		},
	}

	cp := orig.DeepCopy()
	require.NotNil(t, cp)
	require.Len(t, cp.PendingInteractions, 1)

	// Sanity: the copy starts out equal on all 7 fields.
	assert.Equal(t, orig.PendingInteractions[0], cp.PendingInteractions[0])

	// The slices must not share a backing array.
	require.NotSame(t, &orig.PendingInteractions[0], &cp.PendingInteractions[0])

	// Mutate the copy; the original must be unaffected if DeepCopy actually
	// deep-copied rather than aliased the slice/struct.
	cp.PendingInteractions[0].Summary = "mutated"
	cp.PendingInteractions[0].RequestedAt = metav1.NewTime(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))

	assert.Equal(t, "example.com content flagged for review", orig.PendingInteractions[0].Summary,
		"mutating the copy must not affect the original's Summary")
	assert.Equal(t, requestedAt, orig.PendingInteractions[0].RequestedAt,
		"mutating the copy must not affect the original's RequestedAt")
}
