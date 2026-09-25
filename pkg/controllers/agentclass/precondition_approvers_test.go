package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// requiredStanding is a resolved standing whose type SpiceDB governs: it names a
// permission an approver must hold, so its approver pool has a shape at
// admission even though no subject may occupy it yet.
func requiredStanding(resourceType string) spiceboxv1alpha1.ResolvedResourceStanding {
	return spiceboxv1alpha1.ResolvedResourceStanding{
		ResourceType:       resourceType,
		Standing:           spiceboxv1alpha1.StandingRequired,
		ApproverPermission: "approve",
	}
}

// sessionStanding is a resolved standing that consults no SpiceDB permission:
// the session's own #approve set decides, which always exists structurally.
func sessionStanding(resourceType string) spiceboxv1alpha1.ResolvedResourceStanding {
	return spiceboxv1alpha1.ResolvedResourceStanding{
		ResourceType: resourceType,
		Standing:     spiceboxv1alpha1.StandingSessionOnly,
	}
}

// newSlotPrecondition builds one SlotPrecondition. The CEL/messages are fixed
// valid fillers — this check reads only Approvers — and approvers is threaded
// through verbatim so a nil (unset), an explicit empty, and a whitespace-only
// list are all expressible.
func newSlotPrecondition(approvers []string) spiceboxv1alpha1.SlotPrecondition {
	return spiceboxv1alpha1.SlotPrecondition{
		CEL:              "facts.observed.is_cross_repository == false",
		UndeterminedHint: "observe it first",
		RefusalMessage:   "the head branch is on a fork",
		Approvers:        approvers,
	}
}

// gatedSlot is a slot with one precondition, on a fill source that runs the
// gate (observed) rather than the channel_thread source validateSlotDeclarations
// refuses beside a precondition.
func gatedSlot(resourceType string, approvers []string) spiceboxv1alpha1.AuthzSlot {
	return spiceboxv1alpha1.AuthzSlot{
		ResourceType: resourceType,
		Permission:   "read",
		FillFrom:     []string{"observed"},
		Requires:     []spiceboxv1alpha1.SlotPrecondition{newSlotPrecondition(approvers)},
	}
}

// A precondition's waiver must have SOMEONE to route to. The field's contract is
// "unset approvers[] defaults to the slot's standing; a set approvers[] is
// authoritative", so admission can prove two empty-by-shape pools: a SET list
// that names no subject (overrides the standing default with a route to no one),
// and an UNSET list whose slot type resolves to no standing at all. Each row
// fixes the precondition's approvers and the resolved standings and asserts the
// refusal (with the unique waiver-message substring, not reason-only) or the
// acceptance.
func TestValidatePreconditionApprovers(t *testing.T) {
	const unroutable = "waiver card no subject is authorized to answer"
	cases := []struct {
		name       string
		slots      []spiceboxv1alpha1.AuthzSlot
		standings  []spiceboxv1alpha1.ResolvedResourceStanding
		wantReason string
		wantMsg    []string
	}{
		{
			// Unset branch: no explicit approvers AND the type has no standing to
			// default the pool from. (At the real call site resolveSlotValueKeying
			// pre-empts this; the direct call exercises the branch anyway.)
			name:       "unset approvers + undeclared standing: refused (no default pool)",
			slots:      []spiceboxv1alpha1.AuthzSlot{gatedSlot("fork_repo", nil)},
			standings:  nil,
			wantReason: spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
			wantMsg:    []string{"authz.slots[fork_repo].requires[0]", "resolves to no standing", unroutable},
		},
		{
			// Set-but-blank branch: an explicit list naming no subject. Refused
			// even though the type's standing is undeclared here — the set list is
			// what makes it unroutable.
			name:       "explicit empty approvers + undeclared standing: refused (set names no subject)",
			slots:      []spiceboxv1alpha1.AuthzSlot{gatedSlot("fork_repo", []string{})},
			standings:  nil,
			wantReason: spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
			wantMsg:    []string{"authz.slots[fork_repo].requires[0]", "names no subject", unroutable},
		},
		{
			// The KEY model row: a set-but-blank list is authoritative, so it is
			// refused EVEN WHEN the slot type has a declared standing. Silently
			// falling back to the standing here would bury the author's mistake.
			name:       "explicit empty approvers + declared standing: refused (set overrides the standing default)",
			slots:      []spiceboxv1alpha1.AuthzSlot{gatedSlot("github_repo", []string{})},
			standings:  []spiceboxv1alpha1.ResolvedResourceStanding{requiredStanding("github_repo")},
			wantReason: spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
			wantMsg:    []string{"authz.slots[github_repo].requires[0]", "names no subject", unroutable},
		},
		{
			// Only-blank entries are the empty pool by shape too, and (unlike an
			// explicit []) this shape survives the CRD's omitempty round-trip, so
			// it is the one the reconcile-driven test exercises.
			name:       "whitespace-only approvers + declared standing: refused (blank names no subject)",
			slots:      []spiceboxv1alpha1.AuthzSlot{gatedSlot("http_target", []string{"", "  "})},
			standings:  []spiceboxv1alpha1.ResolvedResourceStanding{sessionStanding("http_target")},
			wantReason: spiceboxv1alpha1.ReasonSlotPreconditionUnroutable,
			wantMsg:    []string{"authz.slots[http_target].requires[0]", unroutable},
		},
		{
			// Unset + a declared required standing: the pool defaults to the
			// SpiceDB subjects holding the permission, which has a shape.
			name:      "unset approvers + declared required standing: accepted",
			slots:     []spiceboxv1alpha1.AuthzSlot{gatedSlot("github_repo", nil)},
			standings: []spiceboxv1alpha1.ResolvedResourceStanding{requiredStanding("github_repo")},
		},
		{
			// Unset + a session-only standing: the pool is the session's own
			// approvers, always structurally present.
			name:      "unset approvers + session-only standing: accepted",
			slots:     []spiceboxv1alpha1.AuthzSlot{gatedSlot("http_target", nil)},
			standings: []spiceboxv1alpha1.ResolvedResourceStanding{sessionStanding("http_target")},
		},
		{
			// A set list naming the session #approve set is routable with no
			// resolved standing needed at all.
			name: "explicit session-set approvers: accepted with no standing",
			slots: []spiceboxv1alpha1.AuthzSlot{
				gatedSlot("fork_repo", []string{"agentsession:demo-ns/demo-class#approve"}),
			},
			standings: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validatePreconditionApprovers(slotClass(tc.slots...), tc.standings)
			assert.Equal(t, tc.wantReason, reason, "reason")
			for _, want := range tc.wantMsg {
				assert.Contains(t, msg, want, "message substring")
			}
			if tc.wantReason == "" {
				assert.Empty(t, msg, "an accepted class must carry no message")
			}
		})
	}
}
