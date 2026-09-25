package steelthread_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestDeriveSeed_SubtractsSessionWrittenTuples is the load-bearing test.
//
// Seeding a tuple the SESSION wrote would let the bundle pass with the
// relationship-writing code broken — the run would find the grant already
// present and never exercise the code that creates it. Three classes are
// written at run time and must all be subtracted: the session's own MCP
// dispatches (relwrites_audit), channelsd's started_by, and the approval
// binder's slot grants.
func TestDeriveSeed_SubtractsSessionWrittenTuples(t *testing.T) {
	recs := steelthread.Records{
		Decisions: []authzdecision.Decision{
			{ResourceType: "crm_company", ResourceID: "c1", Permission: "contact_access", Outcome: "allowed"},
		},
		RelWrites: []relwritesaudit.Audit{{Tuples: []relwritesaudit.Tuple{
			{Resource: "crm_company:c1", Relation: "watcher", Subject: "user:bob"},
		}}},
	}
	expand := func(_ context.Context, _, _, _ string) ([]steelthread.Tuple, error) {
		return []steelthread.Tuple{
			{Resource: "crm_company:c1", Relation: "owner", Subject: "user:alice"},                                  // pre-existing: KEEP
			{Resource: "crm_company:c1", Relation: "watcher", Subject: "user:bob"},                                  // relwrites: DROP
			{Resource: "crm_company:c1", Relation: "slot_grant_contact_access", Subject: "agentsession:default/s1"}, // slot grant: DROP
			{Resource: "agentsession:default/s1", Relation: "started_by", Subject: "user:alice"},                    // session object: DROP
		}, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Equal(t, []steelthread.Tuple{
		{Resource: "crm_company:c1", Relation: "owner", Subject: "user:alice"},
	}, got.Tuples, "only tuples that PRE-EXISTED the session may be seeded")
}

// TestDeriveSeed_SlotGrantFilterIsIndependent isolates subtraction class 3.
//
// The slot-grant tuple in TestDeriveSeed_SubtractsSessionWrittenTuples has
// an agentsession subject, so isSessionTuple alone drops it — deleting the
// isSlotGrantTuple check from the drop condition left every test in this
// file green. This case gives the slot-grant-relation tuple a subject that
// is NOT an agentsession, so isSlotGrantTuple is the only filter that can
// drop it.
//
// Production always sets SubjectType "agentsession" on a slot grant
// (pkg/authz/slot_grant.go:50-57) — this fixture is deliberately more
// permissive than reality so the test isolates one filter. Do not "fix" the
// subject back to agentsession; that would reintroduce the redundancy this
// test exists to remove.
func TestDeriveSeed_SlotGrantFilterIsIndependent(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "crm_company", ResourceID: "c1", Permission: "contact_access", Outcome: "allowed"},
	}}
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		return []steelthread.Tuple{
			{Resource: "crm_company:c1", Relation: "slot_grant_contact_access", Subject: "user:carol"},
		}, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Tuples, "a slot-grant relation must be dropped even when its subject is not an agentsession")
}

// TestDeriveSeed_DeniedDecisionsSeedNothing pins that absence is the fixture's
// default. Seeding for a denial would make the denial impossible to reproduce.
func TestDeriveSeed_DeniedDecisionsSeedNothing(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "crm_company", ResourceID: "c9", Permission: "contact_access", Outcome: "denied"},
	}}
	var called int
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		called++
		return nil, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Tuples)
	assert.Zero(t, called, "a denied decision must not even be expanded")
}

// TestDeriveSeed_ReportsAnAllowThatSeedsNothing pins the silent-incompleteness
// case. An allowed decision whose expansion subtracts to nothing means the
// grant came from somewhere the fixture will not have — the capture must SAY
// so, because the alternative is a bundle that replays with that call denied
// and reports the divergence several minutes and one layer away from the cause.
func TestDeriveSeed_ReportsAnAllowThatSeedsNothing(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "crm_company", ResourceID: "c2", Permission: "list", Outcome: "allowed"},
	}}
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		return nil, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Tuples)
	assert.Equal(t, []string{"crm_company:c2#list"}, got.Unseeded)
}

// TestDeriveSeed_AnAllowHeldOnlyByRunTimeTuplesIsNotUnseeded is the negative
// control for the test above, and the case the round-trip test surfaced.
//
// "The expansion returned nothing" and "everything the expansion returned is
// written at run time" both leave the seed empty, and they mean opposite
// things. The first is a grant this derivation cannot see, and the replay will
// deny where the run allowed — a hard finding. The second is the plan-gate and
// slot design working exactly as intended: the allow exists only because a
// human approved, the approval minted a slot grant against the session, and the
// REPLAY re-approves and re-mints it. Seeding that tuple is precisely what the
// subtraction rules exist to prevent, so reporting its absence as a defect
// refuses every scenario the feature was built for — which is what it did:
// every plan-gate and slot bundle in the bronze suite was rejected this way.
func TestDeriveSeed_AnAllowHeldOnlyByRunTimeTuplesIsNotUnseeded(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "extrepo_repo", ResourceID: "r1", Permission: "push", Outcome: "allowed"},
	}}
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		return []steelthread.Tuple{
			{Resource: "extrepo_repo:r1", Relation: "slot_grant_push", Subject: "agentsession:default/s1"},
			{Resource: "agentsession:default/s1", Relation: "started_by", Subject: "user:alice"},
		}, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Tuples, "a run-time-written tuple is still never seeded")
	assert.Empty(t, got.Unseeded,
		"an allow reachable only through tuples the replay writes for itself needs no seed, "+
			"so it must not be reported as one the fixture cannot reproduce")
}

// TestDeriveSeed_DeduplicatesAcrossDecisions pins that a resource checked more
// than once contributes its tuples once.
//
// The output-count assertion alone does not prove the redundant expand is
// avoided: the final tuple set is deduped regardless of how many times
// expand ran, so removing the seen-key short-circuit and calling expand
// twice for the same key still leaves the output count at 1. The call
// counter is what pins "expand is not re-invoked for a key already seen",
// which is what the test's name and docstring claim.
func TestDeriveSeed_DeduplicatesAcrossDecisions(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "crm_company", ResourceID: "c1", Permission: "list", Outcome: "allowed"},
		{ResourceType: "crm_company", ResourceID: "c1", Permission: "list", Outcome: "allowed"},
	}}
	var calls int
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		calls++
		return []steelthread.Tuple{
			{Resource: "crm_company:c1", Relation: "owner", Subject: "user:alice"},
		}, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand, nil)
	require.NoError(t, err)
	assert.Len(t, got.Tuples, 1, "a resource checked twice contributes its tuples once")
	assert.Equal(t, 1, calls, "a key already seen must not re-invoke expand")
}

// grantsDeclaring builds the AgentSessionGrants an AgentClass reconciler writes
// for a class declaring exactly these (resourceType, permission) SLOTS.
//
// Built from the CRD type rather than from a hand-written pair list, because
// that is how DeriveSeed reads it: a test that transcribed the pairs into a
// shape of its own would stop testing the field the capture actually consults.
func grantsDeclaring(t *testing.T, slots ...spiceboxv1alpha1.GrantPair) *spiceboxv1alpha1.AgentSessionGrants {
	t.Helper()
	return &spiceboxv1alpha1.AgentSessionGrants{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-grants", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionGrantsSpec{Slots: slots},
	}
}

// TestDeriveSeed_CollectedSlotBindingIsExemptOnlyForItsDeclaredPair covers BOTH
// directions of the exemption a class's declared slots create.
//
// A session-only slot binding is written when a human approves and deleted when
// the session ends, so by capture time live SpiceDB has nothing left to expand.
// That is byte-for-byte the same observation as "nobody ever granted this" —
// zero tuples to keep, zero to subtract — and reporting it made every
// git-workspace session uncapturable, because the workspace fetch every such
// session opens with is held by exactly that kind of binding.
//
// The class's own declaration is the only thing separating them, so the
// negative direction matters as much as the positive: a pair the class does NOT
// declare as a slot is a grant the fixture genuinely will not reproduce, and it
// must still hard-fail. Widening the exemption to the resource TYPE, or reading
// a missing CR as permission, would switch the check off exactly where it earns
// its keep.
func TestDeriveSeed_CollectedSlotBindingIsExemptOnlyForItsDeclaredPair(t *testing.T) {
	fetch := spiceboxv1alpha1.GrantPair{ResourceType: "extrepo_repo", Permission: "fetch"}
	read := spiceboxv1alpha1.GrantPair{ResourceType: "extrepo_repo", Permission: "read"}

	cases := []struct {
		name              string
		grants            *spiceboxv1alpha1.AgentSessionGrants
		resourceType      string
		permission        string
		wantUnseeded      []string
		wantDeclaredSlots int
	}{
		{
			name:              "the pair IS a declared slot: a collected binding is not reported",
			grants:            grantsDeclaring(t, fetch, read),
			resourceType:      "extrepo_repo",
			permission:        "fetch",
			wantUnseeded:      nil,
			wantDeclaredSlots: 2,
		},
		{
			name:              "a declared TYPE with an undeclared permission: still reported",
			grants:            grantsDeclaring(t, fetch, read),
			resourceType:      "extrepo_repo",
			permission:        "push",
			wantUnseeded:      []string{"extrepo_repo:r1#push"},
			wantDeclaredSlots: 2,
		},
		{
			name:              "a declared PERMISSION on an undeclared type: still reported",
			grants:            grantsDeclaring(t, fetch, read),
			resourceType:      "crm_company",
			permission:        "fetch",
			wantUnseeded:      []string{"crm_company:r1#fetch"},
			wantDeclaredSlots: 2,
		},
		{
			name:              "a class declaring no slots at all: nothing is exempt",
			grants:            grantsDeclaring(t),
			resourceType:      "extrepo_repo",
			permission:        "fetch",
			wantUnseeded:      []string{"extrepo_repo:r1#fetch"},
			wantDeclaredSlots: 0,
		},
		{
			name:              "no AgentSessionGrants gathered: absence is the strict answer, not a slot",
			grants:            nil,
			resourceType:      "extrepo_repo",
			permission:        "fetch",
			wantUnseeded:      []string{"extrepo_repo:r1#fetch"},
			wantDeclaredSlots: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs := steelthread.Records{Decisions: []authzdecision.Decision{
				{ResourceType: tc.resourceType, ResourceID: "r1", Permission: tc.permission, Outcome: "allowed"},
			}}
			// What a COLLECTED binding leaves behind: the tuple that carried
			// the allow is already gone, so there is nothing to classify.
			expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
				return nil, nil
			}

			got, err := steelthread.DeriveSeed(recs, expand, tc.grants)
			require.NoError(t, err)
			assert.Empty(t, got.Tuples, "a zero expansion has nothing to seed either way")
			assert.Equal(t, tc.wantUnseeded, got.Unseeded)
			assert.Equal(t, tc.wantDeclaredSlots, got.DeclaredSlots,
				"the finding's message reads how many slots were consulted off this field")
		})
	}
}

// TestDeriveSeed_ADeclaredSlotDoesNotSuppressARealSeed pins that the exemption
// reaches the zero-expansion branch ONLY.
//
// Its whole justification is that there was no tuple to classify. A declared
// slot whose expansion DID return a pre-existing relationship must still seed
// it — dropping that tuple because the pair happens to be declared would emit a
// fixture missing the very grant the run was allowed by, and the bundle would
// then deny at replay with no finding pointing back here.
func TestDeriveSeed_ADeclaredSlotDoesNotSuppressARealSeed(t *testing.T) {
	recs := steelthread.Records{Decisions: []authzdecision.Decision{
		{ResourceType: "extrepo_repo", ResourceID: "r1", Permission: "fetch", Outcome: "allowed"},
	}}
	expand := func(context.Context, string, string, string) ([]steelthread.Tuple, error) {
		return []steelthread.Tuple{
			{Resource: "extrepo_repo:r1", Relation: "owner", Subject: "user:alice"},
		}, nil
	}

	got, err := steelthread.DeriveSeed(recs, expand,
		grantsDeclaring(t, spiceboxv1alpha1.GrantPair{ResourceType: "extrepo_repo", Permission: "fetch"}))
	require.NoError(t, err)
	assert.Equal(t, []steelthread.Tuple{
		{Resource: "extrepo_repo:r1", Relation: "owner", Subject: "user:alice"},
	}, got.Tuples, "a declared slot exempts a MISSING grant from being reported; it must never drop a "+
		"pre-existing tuple the expansion returned")
	assert.Empty(t, got.Unseeded)
}
