package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// waiverLoop builds a Loop wired for a precondition_waiver round trip: the raise
// half (buildPreconditionWaiverAsk) and the publish half
// (buildPreconditionWaiverPending, reached via PublishApproval) both run against
// ONE set of ResourceStandings + SpiceDBLookupSubjects, so the test can assert
// they resolve the SAME clicker. github_repo has `required` standing (owner) in
// testResourceStandings.
func waiverLoop(t *testing.T, lookup func(ctx context.Context, resource, permission string) ([]string, error), published *channelevents.Envelope) *Loop {
	t.Helper()
	return &Loop{
		Mem:                   memory.NewLocal(inmem.NewBackend()),
		ResourceStandings:     testResourceStandings(),
		SessionKey:            memory.NamespacedName{Namespace: "ns", Name: "s"},
		Status:                LocalStatusPatcher(),
		Approval:              approval.New(),
		ChannelKind:           "slack",
		PlanGateSlotTypes:     []string{"github_repo"}, // a DECLARED slot ⇒ NoSlotGrant=false
		SpiceDBLookupSubjects: lookup,
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			*published = env
			return nil
		},
		Tools: []tool.Tool{&csFakeTool{name: "inspect"}},
	}
}

// waiverPerm is the External github_repo check the gated call runs under.
func waiverPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       "github_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
}

func waiverInput() pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "inspect", UseID: "tu-1"},
	}
}

// TestPreconditionWaiver_RaiseAndPublishAgreeOnApprovers is Step 5's two-process
// agreement check. A precondition_waiver card is resolved in TWO processes — the
// raise half (buildPreconditionWaiverAsk, which fail-closed-checks the pool) and
// the publish half (buildPreconditionWaiverPending, which stamps the delivery
// Audience). If they resolved different approver sets the card would raise to one
// audience and then be refused for its own clicker, so the test drives the full
// raise→publish round trip and asserts the published Audience is the set the raise
// half validated against — in BOTH branches the split takes.
//
// The RefusalMessage is the gate-authored body: it rides denial.Message and must
// land on the card as a field, never as the agent's justification.
func TestPreconditionWaiver_RaiseAndPublishAgreeOnApprovers(t *testing.T) {
	const refusal = "waiver-test refusal: this commit's head lives on a fork, so checking it out runs untrusted code"

	cases := []struct {
		name string
		// approvers is denial.Approvers (the refusing rule's declared set). Empty
		// exercises the default-to-standing branch; a non-empty custom set
		// exercises the override branch — and, because the custom set routes to a
		// DIFFERENT subject than the resource owner, proves the publish half
		// consults precondition_approvers rather than re-deriving from the resource.
		approvers []string
		lookup    map[string][]string
		wantSub   string
		why       string
		// wantResourcesStamped is whether the CLICK-gate input (pl.Resources) names
		// the refused resource. Stamped ONLY for the unset-approvers / owned-resource
		// case, where delivery and the click gate both key on #owner; left EMPTY when
		// the rule declared its own approvers, so CheckApproverAuthorized folds to the
		// session-approve gate the card was delivered to (host_approval.go's condition
		// `len(preApprovers) == 0 && len(resourceOwnerSets) > 0`). See
		// TestPreconditionWaiver_ClickGateAuthorizesTheDeliveredApprover for the gate
		// this shape feeds.
		wantResourcesStamped bool
	}{
		{
			name:      "unset approvers: both halves route to the resource owner",
			approvers: nil,
			lookup: map[string][]string{
				"agentsession:ns/s#approve": {"user:alice"},
				"github_repo:spicedb#owner": {"user:bob"},
			},
			wantSub:              "user:bob",
			why:                  "default-to-standing: approverSetFor yields github_repo:spicedb#owner in both halves",
			wantResourcesStamped: true,
		},
		{
			name:      "declared approvers: both halves route to the rule's set, NOT the resource owner",
			approvers: []string{"risk_council:fork#member"},
			lookup: map[string][]string{
				"agentsession:ns/s#approve": {"user:alice"},
				"github_repo:spicedb#owner": {"user:bob"},   // the owner — must NOT be who it routes to
				"risk_council:fork#member":  {"user:carol"}, // the declared risk-answerer
			},
			wantSub:              "user:carol",
			why:                  "a publish half that re-derived from the resource would route to bob; agreement means it honors precondition_approvers like the raise half",
			wantResourcesStamped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var published channelevents.Envelope
			l := waiverLoop(t, lookupBy(tc.lookup), &published)

			denial := &authz.PreconditionDenial{
				Verdict:   precondition.Refused,
				Message:   refusal,
				Approvers: tc.approvers,
			}

			// Raise half.
			ask, err := l.buildPreconditionWaiverAsk(context.Background(), waiverInput(),
				map[string]any{"repo": "spicedb"}, waiverPerm(), denial)
			require.NoError(t, err, "a refused precondition with a resolvable pool must build the waiver ask")
			require.NotNil(t, ask)
			assert.Equal(t, categories.PreconditionWaiver, ask.Kind,
				"RULING P3-1: the ask Kind is the shared constant, or the PublishApproval switch never matches it")
			assert.Equal(t, refusal, ask.Summary, "the ask Summary is the gate-authored RefusalMessage, verbatim")

			// Publish half, reached exactly as production does — through the
			// PublishApproval switch's new case.
			h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
			reqID, err := h.PublishApproval(context.Background(), *ask)
			require.NoError(t, err, "PublishApproval must accept the precondition_waiver kind (P3-1: not a fail-closed unknown kind)")
			require.NotEmpty(t, reqID)

			pa, ok := h.pending().m[reqID]
			require.True(t, ok, "the waiver must register a pending approval under reqID")
			require.NoError(t, pa.onPublish(context.Background()))

			require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
			var pl channelevents.InteractionRequestPayload
			require.NoError(t, json.Unmarshal(published.Payload, &pl))

			assert.Equal(t, categories.PreconditionWaiver, pl.Category,
				"the payload Category is the shared constant (P3-1)")
			assert.Equal(t, channelevents.AudienceApprovers, pl.Audience.Scope)

			// THE agreement assertion: the published Audience is the set the raise
			// half validated against — same subject in both branches of the split.
			require.Len(t, pl.Audience.Approvers, 1, tc.why)
			assert.Equal(t, tc.wantSub, pl.Audience.Approvers[0].Subject.String(), tc.why)
			assert.Empty(t, pl.Audience.Approvers[0].ExternalID.String(),
				"the canonical id rides Subject, never ExternalID (silent-delivery bug guard)")

			// Resources is the DecideResourceOwners server-side CLICK-gate input, and
			// it depends on WHICH pool the waiver routed to. When the rule declared its
			// own approvers, Resources is left EMPTY so the click gate folds to the
			// session-approve set the card was delivered to; stamping the refused
			// resource there would demand #owner of a resource nobody owns and deny the
			// delivered approver at click time. When approvers are unset and the type
			// has owner standing, Resources names the refused resource so delivery and
			// the click gate agree on #owner.
			if tc.wantResourcesStamped {
				require.Len(t, pl.Resources, 1, "unset approvers + owned resource stamps the owner click-gate")
				assert.Equal(t, channelevents.InteractionResourceRef{Type: "github_repo", ID: "spicedb", Permission: "owner"}, pl.Resources[0])
			} else {
				assert.Empty(t, pl.Resources, "a declared-approver waiver leaves Resources empty so the click gate folds to the session approve-set")
			}

			// Details = the grant-write inputs Task 4's waiver handler reads back to
			// bind the slot grant that IS the waiver.
			require.NotEmpty(t, pl.Details)
			var det channelevents.ToolApprovalDetails
			require.NoError(t, json.Unmarshal(pl.Details, &det))
			assert.Equal(t, "github_repo", det.ResourceType)
			assert.Equal(t, "spicedb", det.ResourceID)
			assert.Equal(t, "write", det.Permission)
			assert.Equal(t, "inspect", det.ToolName)
			assert.False(t, det.NoSlotGrant, "github_repo is a declared slot, so the grant must be attempted")

			// The RefusalMessage is a card FIELD (trusted, gate-authored body), never
			// the agent's justification.
			var found bool
			for _, f := range pl.Fields {
				if f.Value == refusal {
					found = true
				}
			}
			assert.True(t, found, "the gate's refusal must be shown on the card as the reason a human is being asked to waive it")
		})
	}
}

// TestPreconditionWaiver_UnownedResourceFailsClosed pins the raise half's
// fail-closed guard: a refused precondition whose resource has no owners (and no
// declared rule approvers) must NOT emit a waiver into the void — parity with
// buildToolCallApprovalAsk's "no one has standing" refusal.
func TestPreconditionWaiver_UnownedResourceFailsClosed(t *testing.T) {
	var published channelevents.Envelope
	l := waiverLoop(t, lookupBy(map[string][]string{
		"agentsession:ns/s#approve": {"user:alice"},
		// github_repo:spicedb#owner deliberately absent ⇒ no owners.
	}), &published)

	denial := &authz.PreconditionDenial{Verdict: precondition.Refused, Message: "refused"}
	ask, err := l.buildPreconditionWaiverAsk(context.Background(), waiverInput(),
		map[string]any{"repo": "spicedb"}, waiverPerm(), denial)
	require.Error(t, err, "an unowned refused resource must fail closed rather than prompt into the void")
	assert.Nil(t, ask)
	assert.Contains(t, err.Error(), "no one has standing")
}

// fakeApproverChecker is a runner-local authz.ApproverChecker that answers the
// two click-gate questions from DISTINCT sets, so the fold under test is
// observable: a session approver who owns nothing, and a resource owner who has
// no session standing, are two different subjects and cannot mask each other.
type fakeApproverChecker struct {
	approve map[string]bool // canonical id → holds agentsession#approve
	owner   map[string]bool // "canon|type|id" → holds #owner
}

func (f *fakeApproverChecker) CheckApprove(_ context.Context, _, _ string, canon identity.CanonicalUserID, _ bool) (bool, error) {
	return f.approve[canon.String()], nil
}

func (f *fakeApproverChecker) CheckOwnerOnResource(_ context.Context, rt, rid string, canon identity.CanonicalUserID, _ bool) (bool, error) {
	return f.owner[canon.String()+"|"+rt+"|"+rid], nil
}

// CheckOnResource satisfies the approver-gate interface. These fakes model
// resource types whose approverPermission is the default (owner), so every
// permission delegates to the owner answer.
func (f *fakeApproverChecker) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return f.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

// buildWaiverCard drives the full raise→publish round trip for a refused
// precondition and returns the published card. The card's Resources are the
// input the server-side click gate keys on, so this is the exact shape a
// clicker's authorization is decided against.
func buildWaiverCard(t *testing.T, lookup map[string][]string, denial *authz.PreconditionDenial) channelevents.InteractionRequestPayload {
	t.Helper()
	var published channelevents.Envelope
	l := waiverLoop(t, lookupBy(lookup), &published)

	ask, err := l.buildPreconditionWaiverAsk(context.Background(), waiverInput(),
		map[string]any{"repo": "spicedb"}, waiverPerm(), denial)
	require.NoError(t, err, "the raise half must build the waiver ask")
	require.NotNil(t, ask)

	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	reqID, err := h.PublishApproval(context.Background(), *ask)
	require.NoError(t, err, "the publish half must accept the precondition_waiver kind")
	pa, ok := h.pending().m[reqID]
	require.True(t, ok, "the waiver registers a pending approval under reqID")
	require.NoError(t, pa.onPublish(context.Background()))

	require.Equal(t, channelevents.KindInteractionRequest, published.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published.Payload, &pl))
	return pl
}

// clickGateResources mirrors pipeline.resolveDecisionResources: it maps the
// card's Resources onto the []authz.ApproverResourceRef CheckApproverAuthorized
// consumes. An empty card slice yields an empty gate slice, which is exactly the
// fold to the session-approve gate that is under test.
func clickGateResources(pl channelevents.InteractionRequestPayload) []authz.ApproverResourceRef {
	out := make([]authz.ApproverResourceRef, 0, len(pl.Resources))
	for _, r := range pl.Resources {
		out = append(out, authz.ApproverResourceRef{Type: r.Type, ID: r.ID})
	}
	return out
}

// TestPreconditionWaiver_ClickGateAuthorizesTheDeliveredApprover drives the
// REAL click gate (authz.CheckApproverAuthorized, the DecideResourceOwners fold
// pipeline/interaction_decision.go runs server-side) against the card the runner
// actually builds — the check the old raise-vs-publish agreement test could not
// make, because both of its populations were the same delivery set and it never
// touched authorization.
//
// The card is delivered to precondition.Approvers, but the CLICK is gated by
// CheckApproverAuthorized(pl.Resources): when the rule declared its own
// approvers the card leaves Resources EMPTY, so the gate folds to
// CheckApprove(agentsession#approve) — the delivered set — and the delivered
// approver is authorized. The bug this pins: stamping the refused resource there
// made the gate demand #owner of a userless session's UNOWNED fork, so the
// delivered session approver was denied and the waiver could never be granted.
//
// The two probe subjects are genuinely distinct — alice holds
// agentsession#approve but owns nothing; bob owns the fork but has no session
// standing — so a gate that consulted the wrong set flips the verdict and the
// test fails.
func TestPreconditionWaiver_ClickGateAuthorizesTheDeliveredApprover(t *testing.T) {
	ctx := context.Background()
	const refusal = "waiver-test refusal: this commit's head lives on a fork, so checking it out runs untrusted code"

	sessionApprover := identity.CanonicalFromTrusted("alice", "test fixture") // agentsession#approve, NOT an owner
	resourceOwner := identity.CanonicalFromTrusted("bob", "test fixture")     // owns the fork, NO session standing
	fake := &fakeApproverChecker{
		approve: map[string]bool{sessionApprover.String(): true},
		owner:   map[string]bool{resourceOwner.String() + "|github_repo|spicedb": true},
	}

	t.Run("declared approvers: Resources empty ⇒ click folds to the session-approve gate, delivered approver authorized", func(t *testing.T) {
		// `approvers: session` — demo-reviewbot's concrete case. The rule names the
		// session-approve set, the card is delivered to alice, and the click gate must
		// fold to CheckApprove — the SAME set — not to ownership of an unowned fork.
		pl := buildWaiverCard(t, map[string][]string{
			"agentsession:ns/s#approve": {"user:alice"},
			"github_repo:spicedb#owner": {"user:bob"},
		}, &authz.PreconditionDenial{
			Verdict:   precondition.Refused,
			Message:   refusal,
			Approvers: []string{"agentsession:ns/s#approve"},
		})
		// assert (not require): if a regression stamps Resources here, let the test
		// CONTINUE to the click-gate assertion below so the failure names the real
		// consequence — the delivered approver denied — not just the shape.
		assert.Empty(t, pl.Resources,
			"a declared-approver waiver leaves Resources empty so the click gate folds to the session approve-set")

		res := clickGateResources(pl)
		authorized, err := authz.CheckApproverAuthorized(ctx, fake, "ns", "s", res, sessionApprover)
		require.NoError(t, err)
		assert.True(t, authorized,
			"the delivered session approver's click folds to CheckApprove and is AUTHORIZED — the bound waiver handler (BindApproved) runs")

		// With Resources empty the fold consults CheckApprove only: a bare resource
		// owner who lacks session standing is NOT admitted through the session gate.
		ownerAuthorized, err := authz.CheckApproverAuthorized(ctx, fake, "ns", "s", res, resourceOwner)
		require.NoError(t, err)
		assert.False(t, ownerAuthorized,
			"Resources empty ⇒ ownership is never consulted; a non-approver owner is denied at the session gate")
	})

	t.Run("unset approvers: Resources stamped ⇒ click gates on resource ownership", func(t *testing.T) {
		// No declared approvers and github_repo has `required` (owner) standing, so
		// Resources IS stamped and the click gate demands #owner of the fork.
		pl := buildWaiverCard(t, map[string][]string{
			"agentsession:ns/s#approve": {"user:alice"},
			"github_repo:spicedb#owner": {"user:bob"},
		}, &authz.PreconditionDenial{
			Verdict: precondition.Refused,
			Message: refusal,
			// Approvers unset ⇒ default to the resource's owner standing.
		})
		require.Len(t, pl.Resources, 1, "unset approvers + owned resource stamps the owner click-gate")
		assert.Equal(t, channelevents.InteractionResourceRef{Type: "github_repo", ID: "spicedb", Permission: "owner"}, pl.Resources[0])

		res := clickGateResources(pl)
		ownerAuthorized, err := authz.CheckApproverAuthorized(ctx, fake, "ns", "s", res, resourceOwner)
		require.NoError(t, err)
		assert.True(t, ownerAuthorized,
			"the resource owner's click is authorized against the stamped Resources — the #owner gate")

		nonOwnerAuthorized, err := authz.CheckApproverAuthorized(ctx, fake, "ns", "s", res, sessionApprover)
		require.NoError(t, err)
		assert.False(t, nonOwnerAuthorized,
			"a session approver who does not own the resource is DENIED — Resources stamped means the owner gate, not the session gate")
	})
}
