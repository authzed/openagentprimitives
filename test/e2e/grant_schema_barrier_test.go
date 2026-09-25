//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// asgWith builds an AgentSessionGrants carrying the given SchemaIncluded
// condition. status=="" leaves the CR with no conditions at all — the state a
// freshly-created CR sits in until the guardian's first completed pass.
func asgWith(name string, status metav1.ConditionStatus, reason string) spiceboxv1alpha1.AgentSessionGrants {
	t := spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if status != "" {
		t.Status.Conditions = []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentSessionGrantsConditionSchemaIncluded,
			Status: status,
			Reason: reason,
		}}
	}
	return t
}

// TestGrantSchemaReady covers the predicate behind the WaitForGrantSchema
// barrier. The guardian composes AgentSessionGrants pairs into the
// `agentsession` definition asynchronously, one debounce-delayed reconcile
// AFTER the AgentClass reports Valid — so an approval that lands in that window
// tries to write a grant relation the schema does not have yet, SpiceDB rejects
// it with FailedPrecondition, and the runner reports the approval as an expiry
// nobody acted on. SchemaIncluded=True is patched only after the schema write
// lands, which makes it the signal that the relations are live.
//
// The zero-CR case is the one worth stating twice: "no CRs yet" is
// indistinguishable in shape from "nothing to wait for", and reading it as
// success would hand back a barrier that returns instantly in exactly the
// situation it exists to cover.
func TestGrantSchemaReady(t *testing.T) {
	cases := []struct {
		name      string
		items     []spiceboxv1alpha1.AgentSessionGrants
		wantReady bool
		wantWhy   string
	}{
		{
			name:      "no CRs yet: NOT ready — the AgentClass has not written its grants",
			items:     nil,
			wantReady: false,
			wantWhy:   "no AgentSessionGrants",
		},
		{
			name:      "CR exists but unreconciled (no conditions): NOT ready",
			items:     []spiceboxv1alpha1.AgentSessionGrants{asgWith("a", "", "")},
			wantReady: false,
			wantWhy:   "a",
		},
		{
			name:      "SchemaIncluded=False: NOT ready",
			items:     []spiceboxv1alpha1.AgentSessionGrants{asgWith("a", metav1.ConditionFalse, "SpiceDBWriteFailed")},
			wantReady: false,
			wantWhy:   "SpiceDBWriteFailed",
		},
		{
			name:      "SchemaIncluded=True: ready",
			items:     []spiceboxv1alpha1.AgentSessionGrants{asgWith("a", metav1.ConditionTrue, "AllPairsResolved")},
			wantReady: true,
		},
		{
			name: "one of two still False: NOT ready — every CR's pairs must be live",
			items: []spiceboxv1alpha1.AgentSessionGrants{
				asgWith("a", metav1.ConditionTrue, "AllPairsResolved"),
				asgWith("b", metav1.ConditionFalse, "PairsSkipped"),
			},
			wantReady: false,
			wantWhy:   "b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, why := e2e.GrantSchemaReadyForTest(tc.items)
			assert.Equal(t, tc.wantReady, ready, "ready")
			if tc.wantWhy != "" {
				assert.Contains(t, why, tc.wantWhy,
					"the not-ready reason must name what is outstanding, so a timeout says which CR stalled")
			}
		})
	}
}
