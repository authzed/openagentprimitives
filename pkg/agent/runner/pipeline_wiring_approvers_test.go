package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	plangateaudit "github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// lookupBy returns a SpiceDBLookupSubjects fn that answers from a map keyed on
// the COMBINED "resource#permission" subjectRef the builders pass through. Any
// key absent from the map yields an empty set.
func lookupBy(byRef map[string][]string) func(ctx context.Context, resource, permission string) ([]string, error) {
	return func(_ context.Context, resource, permission string) ([]string, error) {
		return byRef[resource+"#"+permission], nil
	}
}

// TestBuildToolCallApprovalAsk_DisjointOwners_RoutesToResourceOwner asserts the
// production shape of the owner-approval flow: the requester (sole session
// owner) does NOT own the resource, and the ask must still be built, routed to
// the resource's owner-set. This is the exact case that regressed on
// 2026-07-02 ("no one has standing to approve") when eligibility was computed
// as session owners ∩ resource owners — see the rationale on
// authz.ResolveApprovers.
func TestBuildToolCallApprovalAsk_DisjointOwners_RoutesToResourceOwner(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	// Session approvers = {alice} (the requester); resource owners = {bob}.
	// Disjoint — bob alone is the approver.
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		"github_repo:spicedb#owner":        {"user:bob"},
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
	ask, err := l.buildToolCallApprovalAsk(context.Background(), "do_thing",
		map[string]any{"repo": "spicedb"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err, "requester ≠ resource owner is the NORMAL approval case; it must build an ask")
	require.NotNil(t, ask)
	assert.Equal(t, "github_repo:spicedb#owner",
		extractString(ask.Payload, "approver_subject"),
		"payload approver_subject must route delivery to the resource owner-set")
}

// TestBuildToolCallApprovalAsk_UnownedResource_FailsClosed asserts the builder
// refuses to emit an ask when the named resource has no owners at all — the
// only remaining "no one has standing" case for a resource-scoped gate.
func TestBuildToolCallApprovalAsk_UnownedResource_FailsClosed(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		// github_repo:spicedb#owner deliberately absent ⇒ no owners.
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
	ask, err := l.buildToolCallApprovalAsk(context.Background(), "do_thing",
		map[string]any{"repo": "spicedb"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.Error(t, err, "an unowned resource must fail-closed")
	assert.Nil(t, ask, "no ask should be built when no one has standing")
	assert.Contains(t, strings.ToLower(err.Error()), "no one has standing")
}

// TestBuildToolCallApprovalAsk_NoResource_UsesSessionApproveSet asserts the
// owner-only branch: a permission with no resolvable resource keeps the
// session approve-set as both pool and delivery subject.
func TestBuildToolCallApprovalAsk_NoResource_UsesSessionApproveSet(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
	})

	perm := authz.Permission{StateImpact: authz.External}
	ask, err := l.buildToolCallApprovalAsk(context.Background(), "do_thing",
		map[string]any{}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)
	assert.Equal(t, "agentsession:default/tw1#approve",
		extractString(ask.Payload, "approver_subject"),
		"no resource ⇒ owner-only gate delivered to the session approve-set")
}

// TestBuildLeakageApprovalAsk_DisjointOwners_RoutesToDataOwner mirrors the
// tool_call case for leakage_share: the tainted data's owner approves the
// share regardless of session standing.
func TestBuildLeakageApprovalAsk_DisjointOwners_RoutesToDataOwner(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		"issue:ENG-1#owner":                {"user:bob"},
	})

	taint := []infoleakagetaint.TaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}}
	ask, err := l.buildLeakageApprovalAsk(context.Background(), []string{"user:bob"}, taint, "proposed text")
	require.NoError(t, err, "data owner without session standing is the approver; the ask must build")
	require.NotNil(t, ask)
	assert.Equal(t, "issue:ENG-1#owner",
		extractString(ask.Payload, "approver"),
		"payload approver must route delivery to the data owner-set")
}

// TestBuildLeakageApprovalAsk_UnownedData_FailsClosed asserts the leakage
// builder refuses to emit an ask when a tainted resource has no owners.
func TestBuildLeakageApprovalAsk_UnownedData_FailsClosed(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		// issue:ENG-1#owner deliberately absent ⇒ no owners.
	})

	taint := []infoleakagetaint.TaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}}
	ask, err := l.buildLeakageApprovalAsk(context.Background(), []string{"user:bob"}, taint, "proposed text")
	require.Error(t, err, "unowned tainted data must fail-closed")
	assert.Nil(t, ask, "no ask should be built when no one has standing")
	assert.Contains(t, strings.ToLower(err.Error()), "no one has standing")
}

// TestBuildLeakageApprovalAsk_MultipleOwnerSets_DeliversToAll pins the
// multi-taint behavior: eligibility is the UNION across the distinct
// owner-sets (any single owner approves), and the payload carries EVERY
// owner-set so the channel kind fans the prompt out to all of them (the
// kind applies the delivery cap). The singular "approver" stays the first
// ref for older consumers.
func TestBuildLeakageApprovalAsk_MultipleOwnerSets_DeliversToAll(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		"issue:ENG-1#owner":                {"user:bob", "user:carol"},
		"issue:ENG-2#owner":                {}, // unowned sibling must not empty the union
	})

	taint := []infoleakagetaint.TaintRecord{
		{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"},
		{ResourceType: "issue", ResourceID: "ENG-2", Permission: "view"},
	}
	ask, err := l.buildLeakageApprovalAsk(context.Background(), []string{"user:dave"}, taint, "proposed text")
	require.NoError(t, err, "ENG-1 has owners; the ask must build")
	require.NotNil(t, ask)
	assert.Equal(t, "issue:ENG-1#owner",
		extractString(ask.Payload, "approver"),
		"singular approver stays the first owner-set ref")
	subs, ok := ask.Payload["approver_subjects"].([]string)
	require.True(t, ok, "payload approver_subjects must be a []string")
	assert.Equal(t, []string{"issue:ENG-1#owner", "issue:ENG-2#owner"}, subs,
		"payload approver_subjects must carry every distinct owner-set")
}

// A session-only resource type has NO owners in SpiceDB by design — nobody
// writes an owner tuple for every git remote a session might touch, which is
// exactly why the type declares session-only standing. Routing its approval to
// the resource owner-set therefore finds an empty pool and fails closed, and
// the tool becomes permanently unapprovable.
//
// Observed live on 2026-08-21: an approved 2-phase plan reached its push and
// died with `approval flow: no one has standing to approve tool "gitlike_git"
// (git_repo:https://github.com/... has no owners to route the approval to)` on
// a class whose status published git_repo standing=session-only. The plan-gate
// binding path already honours standing (host_approval.go); this builder did
// not consult it at all.
func TestBuildToolCallApprovalAsk_SessionOnlyStanding_RoutesToSessionApprovers(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "git_push"}}
	l.ResourceStandings = map[string]ResourceStanding{"git_repo": {Standing: spiceboxv1alpha1.StandingSessionOnly}}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		// git_repo:...#owner deliberately absent: SpiceDB holds no upstream
		// truth for this type, which is what session-only declares.
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "git_repo",
			ResourceIDTemplate: "{remote}",
			Permission:         "push",
		},
	}
	ask, err := l.buildToolCallApprovalAsk(context.Background(), "git_push",
		map[string]any{"remote": "https://github.com/acme/widgets"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err,
		"a session-only type has no SpiceDB owners BY DESIGN; refusing to build the ask makes it permanently unapprovable")
	require.NotNil(t, ask)
	assert.Equal(t, "agentsession:default/tw1#approve",
		extractString(ask.Payload, "approver_subject"),
		"session-only routes to the session approve-set — the approver's decision on the card is the authority")
}

// The converse, so the fix above cannot be mistaken for "owner routing is
// optional": a type explicitly declared `required` keeps the pre-existing
// fail-closed behaviour when its resource has no owners. `required` is the
// opt-in that says SpiceDB IS authoritative for this type, so an empty
// owner-set there is a real "no one has standing", not a design property.
func TestBuildToolCallApprovalAsk_RequiredStanding_UnownedResource_StillFailsClosed(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	l.ResourceStandings = map[string]ResourceStanding{"github_repo": {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"}}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		// github_repo:spicedb#owner absent ⇒ no owners.
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
	_, err := l.buildToolCallApprovalAsk(context.Background(), "do_thing",
		map[string]any{"repo": "spicedb"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.Error(t, err, "`required` standing means SpiceDB is authoritative; an unowned resource must still fail closed")
	assert.Contains(t, err.Error(), "has no subjects to route the approval to")
}

// The DECLARATION is authoritative, not the incidental presence of tuples. A
// session-only type routes to the session's approvers even when the resource
// happens to have subjects on a relation named `owner`, because session-only
// says SpiceDB does not govern approval for this type — and a relation that
// exists is not the same as one that means something.
//
// This is the whole reason the permission is declared rather than assumed.
// git_repo declares an `owner` relation only so its checks are answerable;
// reading subjects off it and calling them approvers is exactly the mistake the
// hardcoded "#owner" made.
func TestBuildToolCallApprovalAsk_SessionOnly_IgnoresIncidentalOwnerTuples(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "git_push"}}
	l.ResourceStandings = map[string]ResourceStanding{"git_repo": {Standing: spiceboxv1alpha1.StandingSessionOnly}}
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve":               {"user:alice"},
		"git_repo:https://github.com/acme/widgets#owner": {"user:bob"},
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "git_repo",
			ResourceIDTemplate: "{remote}",
			Permission:         "push",
		},
	}
	ask, err := l.buildToolCallApprovalAsk(context.Background(), "git_push",
		map[string]any{"remote": "https://github.com/acme/widgets"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)
	assert.Equal(t, "agentsession:default/tw1#approve",
		extractString(ask.Payload, "approver_subject"),
		"session-only was declared, so the session's approvers decide regardless of what tuples exist")
}

// An UNDECLARED type is refused outright. Neither guess is safe — session-only
// would widen who may approve, required would dead-end the tool forever — so
// the builder refuses and names the type the author has to classify.
func TestBuildToolCallApprovalAsk_UndeclaredResourceType_IsRefused(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	l.ResourceStandings = map[string]ResourceStanding{} // nothing declared
	l.SpiceDBLookupSubjects = lookupBy(map[string][]string{
		"agentsession:default/tw1#approve": {"user:alice"},
		"github_repo:spicedb#owner":        {"user:bob"},
	})

	perm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
	_, err := l.buildToolCallApprovalAsk(context.Background(), "do_thing",
		map[string]any{"repo": "spicedb"}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.Error(t, err, "an unclassified type has no safe answer, even with owners present")
	assert.Contains(t, err.Error(), "declares no standing")
	assert.Contains(t, err.Error(), "github_repo")
}

// boundPermissionsFor decides what a slot grant is written FOR, and getting it
// wrong is invisible until the very next call escalates again.
//
// A grant is keyed (instance, permission). Binding the slot's declared
// permission when the approval named a different one writes a grant the
// approved call can never spend: observed live 2026-08-21 as an amendment
// adding `read` to a git_repo slot declared for `push`, approved by a human
// over and over because the binding never covered what they clicked.
func TestBoundPermissionsFor(t *testing.T) {
	handle := func(perm, typ string) string {
		h, err := permsurface.NewPermHandle(perm, typ)
		require.NoError(t, err)
		return h.String()
	}
	cases := []struct {
		name     string
		pg       *pendingPlanGate
		declared []string
		want     []string
	}{
		{
			name:     "an amendment binds the permission IT named, not the slot's",
			pg:       &pendingPlanGate{handle: handle("read", "git_repo")},
			declared: []string{"push", "read"},
			want:     []string{"read"},
		},
		{
			name: "a plan ask binds every ceiling permission for this type — all of them were on the card",
			pg: &pendingPlanGate{ceiling: []string{
				handle("push", "git_repo"), handle("read", "git_repo"),
			}},
			declared: []string{"push", "read"},
			want:     []string{"push", "read"},
		},
		{
			name: "ceiling entries for OTHER types are ignored",
			pg: &pendingPlanGate{ceiling: []string{
				handle("read", "git_repo"), handle("write", "github_repo"),
			}},
			declared: []string{"push", "read"},
			want:     []string{"read"},
		},
		{
			name:     "an amendment for a DIFFERENT type falls through to the declared permission",
			pg:       &pendingPlanGate{handle: handle("read", "github_repo")},
			declared: []string{"push", "read"},
			want:     []string{"push"},
		},
		{
			name:     "nothing named falls back to the declared permission, preserving old behaviour",
			pg:       &pendingPlanGate{},
			declared: []string{"push", "read"},
			want:     []string{"push"},
		},
		{
			name:     "a nil gate is not a panic",
			pg:       nil,
			declared: []string{"push", "read"},
			want:     []string{"push"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, boundPermissionsFor(tc.pg, plangateaudit.Content{}, "git_repo", tc.declared))
		})
	}
}

// A permission the slot never DECLARED is not bound, however it was named.
//
// It has no slot_grant_<permission> relation in the composed schema, so
// including it fails the entire grant write at SpiceDB — costing the approval
// even the permissions that were declared. Caught by
// plangate-card-names-constant-instance, whose push stopped being granted the
// moment a plan approval started binding its whole ceiling.
func TestBoundPermissionsFor_UndeclaredPermissionIsNotBound(t *testing.T) {
	handle := func(perm, typ string) string {
		h, err := permsurface.NewPermHandle(perm, typ)
		require.NoError(t, err)
		return h.String()
	}
	pg := &pendingPlanGate{ceiling: []string{
		handle("push", "git_repo"), handle("read", "git_repo"),
	}}
	got := boundPermissionsFor(pg, plangateaudit.Content{}, "git_repo", []string{"push"})
	assert.Equal(t, []string{"push"}, got,
		"read was in the ceiling but the slot never declared it, so binding it would fail the whole write")
}

// A WHOLE-PLAN approval binds each phase's OWN permissions.
//
// One click covers every phase, and narrowToApproved runs once per covered
// record — but the permissions came from the pending ask (the phase that
// raised the card) rather than from the record being bound, so every phase
// got phase 1's permission set.
//
// Observed live 2026-08-23: a 2-phase plan approved in one click wrote
// slot_grant_fetch and slot_grant_read on the repository and nothing else.
// Phase 2's push and write had no grant, so both external calls escalated and
// the user approved three more cards for a plan they had already approved.
// The instances were bound per record all along; only the permissions were not.
func TestBoundPermissionsFor_WholePlanBindsEachPhasesOwnPermissions(t *testing.T) {
	handle := func(perm, typ string) string {
		h, err := permsurface.NewPermHandle(perm, typ)
		require.NoError(t, err)
		return h.String()
	}
	// The card was raised by phase 1, so the pending ask carries phase 1's
	// ceiling — for every record it goes on to bind.
	pg := &pendingPlanGate{
		phaseKey: "phase-1",
		ceiling:  []string{handle("fetch", "git_repo"), handle("read", "git_repo")},
	}
	declared := []string{"push", "read", "write", "fetch"}

	phase1 := plangateaudit.Content{
		PhaseKey: "phase-1",
		Ceiling:  []string{handle("fetch", "git_repo"), handle("read", "git_repo")},
	}
	assert.Equal(t, []string{"fetch", "read"}, boundPermissionsFor(pg, phase1, "git_repo", declared),
		"the raising phase binds what it named")

	phase2 := plangateaudit.Content{
		PhaseKey: "phase-2",
		Ceiling:  []string{handle("push", "git_repo"), handle("write", "git_repo")},
	}
	assert.Equal(t, []string{"push", "write"}, boundPermissionsFor(pg, phase2, "git_repo", declared),
		"phase 2 must bind ITS OWN push and write, not phase 1's fetch and read")
}

// An amendment adds one permission to ONE phase, and must not spray it across
// every other phase the same approval covered.
func TestBoundPermissionsFor_AmendmentBindsOnlyThePhaseItAmends(t *testing.T) {
	handle := func(perm, typ string) string {
		h, err := permsurface.NewPermHandle(perm, typ)
		require.NoError(t, err)
		return h.String()
	}
	pg := &pendingPlanGate{phaseKey: "phase-2", handle: handle("read", "git_repo")}
	declared := []string{"push", "read", "write"}

	amended := plangateaudit.Content{
		PhaseKey: "phase-2",
		Ceiling:  []string{handle("push", "git_repo")},
	}
	assert.Equal(t, []string{"read"}, boundPermissionsFor(pg, amended, "git_repo", declared),
		"the amended phase binds the permission the amendment named")

	other := plangateaudit.Content{
		PhaseKey: "phase-1",
		Ceiling:  []string{handle("write", "git_repo")},
	}
	assert.Equal(t, []string{"write"}, boundPermissionsFor(pg, other, "git_repo", declared),
		"a phase the amendment did not touch binds its own ceiling, not the added handle")
}

// A grant is written for the instance each permission ACTUALLY names.
//
// git.yaml keys git_repo read/write on the literal "workspace" — the session's
// own checked-out copy — while fetch/push key on the remote URL the agent
// declares as a slot. One phase reaches BOTH, and the card says so: it lists
// the constant instance alongside the declared one and states "Every resource
// above is named, so this approval covers them."
//
// The binding did not. It paired every ceiling permission with the DECLARED
// instance only, which got both halves wrong at once:
//
//   - workspace received no grant at all, so the first read of the checked-out
//     files raised an amendment for a resource the card had already named.
//     Observed live 2026-08-23.
//   - the remote URL received slot_grant_write, a grant no check can ever
//     spend, because write resolves to workspace and never to the URL.
func TestPlanGateBindings_EachPermissionBindsTheInstanceItResolvesTo(t *testing.T) {
	surface := []permsurface.Descriptor{
		permDescConst(t, "read", "git_repo", "workspace"),
		permDescConst(t, "write", "git_repo", "workspace"),
		permDescConst(t, "fetch", "git_repo", ""),
		permDescConst(t, "push", "git_repo", ""),
	}
	handle := func(perm string) string {
		h, err := permsurface.NewPermHandle(perm, "git_repo")
		require.NoError(t, err)
		return h.String()
	}
	rec := plangateaudit.Content{
		PhaseKey: "phase-2",
		Ceiling:  []string{handle("write"), handle("push")},
	}
	granted := []plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/demo-org/demo-repo"}}

	// The approver holds every declared permission on both instances (the
	// session-only/authority case); planGateBindings filters bindings by this
	// held set, so this test of the instance-pairing logic grants all of it.
	allGitRepo := map[string]struct{}{"push": {}, "read": {}, "write": {}, "fetch": {}}
	held := map[string]map[string]struct{}{
		"git_repo\x00https://github.com/demo-org/demo-repo": allGitRepo,
		"git_repo\x00workspace":                             allGitRepo,
	}
	got := planGateBindings(surface, nil, rec, granted,
		map[string][]string{"git_repo": {"push", "read", "write", "fetch"}}, held, nil, nil, nil)

	want := map[string]string{
		"write": "workspace",
		"push":  "https://github.com/demo-org/demo-repo",
	}
	assert.Len(t, got, 2, "one binding per ceiling permission, no more")
	for _, b := range got {
		assert.Equal(t, want[b.Permission], b.ResourceID.String(),
			"permission %q must bind the instance it resolves to", b.Permission)
	}
}

// A constant instance is bound ONCE however many declared slots the phase
// names — it does not depend on them, so pairing it with each would write the
// same grant repeatedly.
func TestPlanGateBindings_ConstantInstanceIsBoundOnce(t *testing.T) {
	surface := []permsurface.Descriptor{permDescConst(t, "read", "git_repo", "workspace")}
	h, err := permsurface.NewPermHandle("read", "git_repo")
	require.NoError(t, err)

	rec := plangateaudit.Content{Ceiling: []string{h.String()}}
	granted := []plangateaudit.SlotRef{
		{Type: "git_repo", ID: "https://github.com/demo-org/repo-one"},
		{Type: "git_repo", ID: "https://github.com/demo-org/repo-two"},
	}

	held := map[string]map[string]struct{}{"git_repo\x00workspace": {"read": {}}}
	got := planGateBindings(surface, nil, rec, granted, map[string][]string{"git_repo": {"read"}}, held, nil, nil, nil)
	assert.Len(t, got, 1, "workspace is one instance, not one per declared repo")
	assert.Equal(t, "workspace", got[0].ResourceID.String())
}

// permDescConst builds a surface descriptor with an optional constant id, so a
// test can state which permissions take their instance from the call and which
// name the same object every time.
func permDescConst(t *testing.T, perm, resourceType, constantID string) permsurface.Descriptor {
	t.Helper()
	h, err := permsurface.NewPermHandle(perm, resourceType)
	require.NoError(t, err)
	return permsurface.Descriptor{
		Handle:             h,
		Permission:         perm,
		ResourceType:       resourceType,
		ConstantResourceID: constantID,
	}
}

// planGateBindings must NOT write a grant for a permission the approver was not
// verified to hold on that instance — even when the phase ceiling names it.
//
// The over-grant: a StandingRequired slot declares primary `read` plus
// additional `push`; the phase ceiling holds both on repo X; the approver holds
// `read` on X but not `push` and clicks Approve. Before the fix, boundPermissionsFor
// added every ceiling permission and planGateBindings wrote both slot_grant_read
// AND slot_grant_push, so the agent could push to a repo the approving human
// cannot. The held-set filter drops the push binding.
func TestPlanGateBindings_DropsAPermissionTheApproverDoesNotHold(t *testing.T) {
	surface := []permsurface.Descriptor{
		permDescConst(t, "read", "git_repo", ""),
		permDescConst(t, "push", "git_repo", ""),
	}
	handle := func(perm string) string {
		h, err := permsurface.NewPermHandle(perm, "git_repo")
		require.NoError(t, err)
		return h.String()
	}
	rec := plangateaudit.Content{
		PhaseKey: "phase-1",
		Ceiling:  []string{handle("read"), handle("push")},
	}
	const repo = "https://github.com/acme/app"
	granted := []plangateaudit.SlotRef{{Type: "git_repo", ID: repo}}

	// The approver was verified to hold ONLY read on this instance.
	held := map[string]map[string]struct{}{
		"git_repo\x00" + repo: {"read": {}},
	}

	got := planGateBindings(surface, nil, rec, granted,
		map[string][]string{"git_repo": {"read", "push"}}, held, nil, nil, nil)

	perms := map[string]bool{}
	for _, b := range got {
		perms[b.Permission] = true
	}
	assert.True(t, perms["read"], "read is held and must be bound")
	assert.False(t, perms["push"], "push is NOT held and must not be bound — this is the over-grant fix")
}

// A CONSTANT instance (a check resolvable with no tool args, e.g.
// git_repo:workspace) is session-only and must bind on the approver's say-so —
// the per-permission held filter must NOT drop it. It is absent from the
// delegable/held map (the declared ref is keyed by the phase's named instance,
// the URL), so filtering it forced a second per-call approval for a resource the
// phase card had already named and promised to cover.
func TestPlanGateBindings_ConstantInstanceBindsEvenWhenNotInTheHeldMap(t *testing.T) {
	surface := []permsurface.Descriptor{
		permDescConst(t, "read", "git_repo", "workspace"), // constant → session-only
		permDescConst(t, "push", "git_repo", ""),          // declared instance
	}
	handle := func(perm string) string {
		h, err := permsurface.NewPermHandle(perm, "git_repo")
		require.NoError(t, err)
		return h.String()
	}
	rec := plangateaudit.Content{
		PhaseKey: "phase-1",
		Ceiling:  []string{handle("read"), handle("push")},
	}
	const repo = "https://github.com/acme/app"
	granted := []plangateaudit.SlotRef{{Type: "git_repo", ID: repo}}

	// The held map covers ONLY the declared instance (push on the URL). It has
	// no entry for the constant workspace — exactly the shape that dropped it.
	held := map[string]map[string]struct{}{"git_repo\x00" + repo: {"push": {}}}

	got := planGateBindings(surface, nil, rec, granted,
		map[string][]string{"git_repo": {"read", "push"}}, held, nil, nil, nil)

	byInstance := map[string]string{} // resourceID -> permission
	for _, b := range got {
		byInstance[b.ResourceID.String()] = b.Permission
	}
	assert.Equal(t, "read", byInstance["workspace"],
		"the constant workspace instance must bind read even though it is not in the held map")
	assert.Equal(t, "push", byInstance[repo],
		"the declared instance binds the held permission")
}

// A constant instance skips the held-set filter ONLY when its type is
// session-only. Nothing structurally forbids a StandingRequired type from
// carrying a ConstantResourceID (standing and the constant come from separate
// sources), and skipping the filter for one would let an approver's click grant
// reach on a shared resource they hold nothing on — the exact over-grant the
// per-permission filter closes for declared instances. A StandingRequired-type
// constant must still be filtered against the approver's held set.
func TestPlanGateBindings_StandingRequiredConstantIsStillFiltered(t *testing.T) {
	surface := []permsurface.Descriptor{permDescConst(t, "admin", "shared_thing", "the-one-instance")}
	h, err := permsurface.NewPermHandle("admin", "shared_thing")
	require.NoError(t, err)
	rec := plangateaudit.Content{PhaseKey: "phase-1", Ceiling: []string{h.String()}}
	granted := []plangateaudit.SlotRef{{Type: "shared_thing", ID: "the-one-instance"}}

	// The approver holds NOTHING on the shared resource: an empty held map.
	held := map[string]map[string]struct{}{}
	standing := map[string]string{"shared_thing": spiceboxv1alpha1.StandingRequired}

	got := planGateBindings(surface, nil, rec, granted,
		map[string][]string{"shared_thing": {"admin"}}, held, standing, nil, nil)

	assert.Empty(t, got,
		"a StandingRequired constant the approver holds nothing on must not be bound — the constant skip is for session-only types only")

	// And with the type session-only, the same constant DOES bind on the
	// approver's say-so (the legitimate constant path stays intact).
	sessionOnly := map[string]string{"shared_thing": spiceboxv1alpha1.StandingSessionOnly}
	got2 := planGateBindings(surface, nil, rec, granted,
		map[string][]string{"shared_thing": {"admin"}}, held, sessionOnly, nil, nil)
	assert.Len(t, got2, 1, "a session-only constant binds on the approval alone")
}
