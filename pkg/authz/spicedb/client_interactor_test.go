//go:build integration

package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTouchInteractor_WritesAndIsIdempotent locks the write-shape of
// agentclass#interactor — the tuple TouchInteractor writes is what
// agentclass#can_personalize (`permission can_personalize = interactor`)
// resolves through, and can_personalize is the App Home preferences pane's
// enumeration gate. Two touches of the same tuple must both succeed (TOUCH
// semantics, not CREATE), and the resulting tuple must satisfy
// can_personalize for the touched subject.
//
// Asserts via the raw CheckOnResource form rather than
// LookupPersonalizableClasses: that lookup is Task 3's contract and does not
// exist yet. The write + idempotency + check-shape match is the whole of
// Task 2's contract.
func TestTouchInteractor_WritesAndIsIdempotent(t *testing.T) {
	c := newIntegrationClient(t)
	ctx := context.Background()

	classNS := "default"
	className := uniq(t, "interactor-class-")
	canonicalID := uniqCanon(t, "interactor-user-")
	subjectRef := "user:" + canonicalID.String()

	require.NoError(t, c.TouchInteractor(ctx, classNS, className, subjectRef), "TouchInteractor (first write)")
	require.NoError(t, c.TouchInteractor(ctx, classNS, className, subjectRef), "TouchInteractor (idempotent second write)")

	allowed, err := c.CheckOnResource(ctx, "agentclass", classNS+"/"+className, "can_personalize", canonicalID, true /*fullyConsistent*/)
	require.NoError(t, err, "CheckOnResource agentclass#can_personalize")
	assert.True(t, allowed,
		"agentclass#can_personalize must hold for the subject TouchInteractor wrote to #interactor — "+
			"write-shape and check-shape must match, the same class of bug TestIntegration_ApproveFlow_GrantsCheckInteract guards on agentsession")

	// Verify LookupPersonalizableClasses also returns the written class.
	personalizable, err := c.LookupPersonalizableClasses(ctx, canonicalID, 100, true /*fullyConsistent*/)
	require.NoError(t, err, "LookupPersonalizableClasses")
	assert.Contains(t, personalizable.Names(), classNS+"/"+className,
		"LookupPersonalizableClasses must include the class the subject was touched into #interactor for")

	// A stranger who was never touched must not incidentally satisfy
	// can_personalize — proves the permission actually gates on #interactor
	// rather than something broader (e.g. every user).
	stranger := uniqCanon(t, "interactor-stranger-")
	strangerAllowed, err := c.CheckOnResource(ctx, "agentclass", classNS+"/"+className, "can_personalize", stranger, true)
	require.NoError(t, err, "CheckOnResource agentclass#can_personalize (stranger)")
	assert.False(t, strangerAllowed, "can_personalize must not hold for a subject that was never touched as #interactor")
}

// TestTouchInteractor_RefusesUnrepresentableName mirrors
// TestEnsureAgentClassStarters_RefusesUnrepresentableName: a class name
// SpiceDB cannot express as an object id is a PERMANENT failure, and
// TouchInteractor composes its object id through the same AgentClassObjectID
// single site as every other agentclass writer, so it must refuse the same
// way rather than opening a second, unvalidated composition path.
func TestTouchInteractor_RefusesUnrepresentableName(t *testing.T) {
	c := newIntegrationClient(t)
	err := c.TouchInteractor(context.Background(), "default", "has.a.dot", "user:alice")
	require.ErrorIs(t, err, ErrUnrepresentableObjectID)
}

// TestLookupPersonalizableClasses_EmptyForUninvolvedUser verifies that a user
// with no interactor tuples gets an empty personalizable classes list.
func TestLookupPersonalizableClasses_EmptyForUninvolvedUser(t *testing.T) {
	c := newIntegrationClient(t)
	res, err := c.LookupPersonalizableClasses(context.Background(), uniqCanon(t, "uninvolved-"), 100, true)
	require.NoError(t, err)
	assert.Empty(t, res.Names())
}
