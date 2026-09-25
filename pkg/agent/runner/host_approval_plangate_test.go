package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func planGateHost(t *testing.T, published *channelevents.Envelope) *runnerHost {
	t.Helper()
	l := &Loop{
		Status:      LocalStatusPatcher(),
		Approval:    approval.New(),
		ChannelKind: "slack",
		AgentName:   "demo-agent",
		// The plan-gate recorder scopes writes by SessionKey, not by the host's
		// own session ref — leaving it empty files every record under "/".
		SessionKey:   memory.NamespacedName{Namespace: "ns", Name: "s"},
		PlanGateMode: "enforcing",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@example.com"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			*published = env
			return nil
		},
	}
	return newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
}

// planPhaseDigest stands in for the frozen plan's digest. The gate stamps the
// real one; what matters here is that the recorded decision names it.
const planPhaseDigest = "sha256:demo-plan-digest"

func planPhaseAsk() pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind:    "plan_phase",
		Summary: `The agent is asking to run "Recon", which only reads.`,
		Payload: map[string]any{
			"phase":      0,
			"planDigest": planPhaseDigest,
			"severity":   "routine",
			// The reach the card put in front of the human. The decision is
			// recorded against THIS set, not against whatever the plan declares
			// by the time the answer arrives.
			"ceiling":  []string{"perm:read:tracker_issue"},
			"maxCount": 1,
			"card":     plangate.Card{Lead: "Plan approval", What: "perm:read:tracker_issue"},
		},
	}
}

func planAmendmentAsk() pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind:    "plan_amendment",
		Summary: "The agent is asking to add Write tracker issue to its approved plan.",
		Payload: map[string]any{
			"handle":     "perm:write:tracker_issue",
			"phase":      0,
			"planDigest": planPhaseDigest,
			"severity":   "elevated",
			"ceiling":    []string{"perm:read:tracker_issue", "perm:write:tracker_issue"},
			"maxCount":   1,
			"card":       plangate.Card{Lead: "⚠️ Plan amendment", Why: "I need to edit the ticket"},
		},
	}
}

// The gap this test was written to expose: the gate raises these two kinds, and
// PublishApproval's switch had no case for either — so under enforcing the ask
// errored, the executor turned that into a Halt, and the session DIED instead
// of asking anyone.
//
// Unit tests on the hook could not see it: they assert the Decision the hook
// returns, never whether the host can publish it.
func TestPublishApproval_handlesPlanGateKinds(t *testing.T) {
	cases := []struct {
		name string
		ask  pipeline.ApprovalAsk
	}{
		{"plan_phase", planPhaseAsk()},
		{"plan_amendment", planAmendmentAsk()},
	}
	for _, tc := range cases {
		t.Run(tc.name+": publishes rather than erroring", func(t *testing.T) {
			var published channelevents.Envelope
			h := planGateHost(t, &published)

			reqID, err := h.PublishApproval(context.Background(), tc.ask)
			require.NoError(t, err, "an unhandled kind halts the session instead of asking")
			require.NotEmpty(t, reqID)
		})
	}
}

// The published card must carry the COMPUTED summary as its lead. The agent's
// own words ride in the card body as its claim — the same trust split the whole
// feature maintains.
func TestPublishApproval_planPhaseCarriesTheComputedSummary(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published)

	reqID, err := h.PublishApproval(context.Background(), planPhaseAsk())
	require.NoError(t, err)

	pa, ok := h.pending().m[reqID]
	require.True(t, ok, "the request must be registered so a decision can resolve it")
	require.NotNil(t, pa.onPublish)
	require.NoError(t, pa.onPublish(context.Background()))

	payload := decodeInteractionRequest(t, published)
	assert.Equal(t, categories.PlanPhase, payload.Category)
	assert.Contains(t, payload.Lead, "Recon", "the lead states which phase")
	assert.NotEmpty(t, payload.Actions, "an approval the human cannot act on is not an approval")
}

// An amendment is a widening, so the severity signal still has to survive all
// the way to the channel — it just no longer does so by recolouring Approve.
// An approval is a question, not a failure, at every severity: Approve stays
// primary and Deny is never danger-styled, for an Elevated card and for a
// Severe one.
func TestPublishApproval_planAmendmentButtonsAreNeverRed(t *testing.T) {
	cases := []struct {
		name     string
		severity string
	}{
		{"elevated amendment", "elevated"},
		{"severe amendment", "severe"},
	}
	for _, tc := range cases {
		t.Run(tc.name+": approve=primary, deny!=danger", func(t *testing.T) {
			var published channelevents.Envelope
			h := planGateHost(t, &published)

			ask := planAmendmentAsk()
			ask.Payload["severity"] = tc.severity

			reqID, err := h.PublishApproval(context.Background(), ask)
			require.NoError(t, err)
			pa := h.pending().m[reqID]
			require.NoError(t, pa.onPublish(context.Background()))

			payload := decodeInteractionRequest(t, published)
			assert.Equal(t, categories.PlanAmendment, payload.Category)

			var approveStyle, denyStyle channelevents.ActionStyle
			for _, a := range payload.Actions {
				switch a.ID {
				case "approve":
					approveStyle = a.Style
				case "deny":
					denyStyle = a.Style
				}
			}
			assert.Equal(t, channelevents.ActionStylePrimary, approveStyle,
				"an approval is a question, not a failure: elevation is carried by the card's amber, not by the confirm button")
			assert.NotEqual(t, channelevents.ActionStyleDanger, denyStyle,
				"a red Deny trains the reflex that refusing is the dangerous act")
		})
	}
}

// planGateRecords reads back everything the gate recorded for the test session.
func planGateRecords(t *testing.T, mem *memory.Local) []plangateaudit.Content {
	t.Helper()
	recs, err := plangateaudit.List(
		memory.WithSystemApproval(context.Background(), "plan_gate"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err)
	return recs
}

// eventsOf lists the recorded event names, for a readable failure message.
func eventsOf(recs []plangateaudit.Content) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Event)
	}
	return out
}

// decideOnce drives one publish/await round trip, delivering `approved` as the
// human's answer, and returns what the gate recorded.
func decideOnce(
	t *testing.T, ask pipeline.ApprovalAsk, approved bool,
) (*memory.Local, []plangateaudit.Content) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	var published channelevents.Envelope
	h := planGateHost(t, &published)
	h.l.Mem = mem

	reqID, err := h.PublishApproval(context.Background(), ask)
	require.NoError(t, err)

	// Deliver the decision once Await has registered the request.
	go func() {
		for i := 0; i < 200; i++ {
			if h.l.Approval.PendingForSession("ns/s") > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		h.l.Approval.DeliverDecision(reqID, approval.Decision{
			Approved: approved, ApproverID: "user:owner@example.com",
		})
	}()

	gotApproved, _, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	require.False(t, timedOut)
	require.Equal(t, approved, gotApproved)

	return mem, planGateRecords(t, mem)
}

// The gate's whole premise is that approval state lives in the audit log, so a
// restart or an idle re-hydrate cannot lose a decision a human already made.
// Nothing wrote that record: PublishApproval grew plan_phase/plan_amendment
// cases, AwaitDecision's post-decision switch did not.
//
// The symptom is not subtle. phaseNeedsApproval folds the log on EVERY
// permissioned call, so with no phase_approved record a five-call phase
// publishes five identical cards, and the approval never survives a restart.
func TestAwaitDecision_planPhaseApprovalIsRecorded(t *testing.T) {
	_, recs := decideOnce(t, planPhaseAsk(), true)

	var approvals []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventPhaseApproved {
			approvals = append(approvals, r)
		}
	}
	require.Len(t, approvals, 1,
		"a human's yes must land in the log or every later call re-asks; got %v", eventsOf(recs))

	got := approvals[0]
	require.NotNil(t, got.PhaseIndex)
	assert.Equal(t, int32(0), *got.PhaseIndex)
	assert.Equal(t, planPhaseDigest, got.PlanDigest,
		"the record must name the plan it cleared, or the fold discards it")
	assert.Equal(t, []string{"perm:read:tracker_issue"}, got.Ceiling,
		"the record carries the APPROVED SUBSET; reconstructing it from the "+
			"declaration would read a held-back handle back as granted")
}

// A refusal has to be durable for the same reason an approval does — more so:
// the gate documents sticky-deny ("re-asking would let an agent grind a human
// into reversing a decision"), and with no denied record IsDenied never fires,
// so the agent could retry until the human relented.
func TestAwaitDecision_planPhaseDenialIsRecorded(t *testing.T) {
	_, recs := decideOnce(t, planPhaseAsk(), false)

	var denials []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventDenied {
			denials = append(denials, r)
		}
	}
	require.Len(t, denials, 1,
		"a human's no must outlive the call it was made against; got %v", eventsOf(recs))

	got := denials[0]
	assert.Equal(t, plangateaudit.OutcomeDenied, got.Outcome)
	require.NotNil(t, got.PhaseIndex)
	assert.Equal(t, int32(0), *got.PhaseIndex)
	assert.Equal(t, []string{"perm:read:tracker_issue"}, got.Ceiling,
		"a denial must be self-contained: the fold matches it by intersection "+
			"against any later plan, so it cannot point at a plan it no longer holds")
}

// decodeInteractionRequest unwraps the published envelope's payload.
func decodeInteractionRequest(t *testing.T, env channelevents.Envelope) channelevents.InteractionRequestPayload {
	t.Helper()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	return pl
}

// slotAsk requests two slot types, only one of which the approver will hold.
func slotAsk() pipeline.ApprovalAsk {
	a := planPhaseAsk()
	a.Payload["slots"] = []string{"crm_contact", "crm_company"}
	return a
}

// slotStanding grants the named slot types and refuses every other.
func slotStanding(granted ...string) func(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
	ok := map[string]struct{}{}
	for _, g := range granted {
		ok[g] = struct{}{}
	}
	return func(_ context.Context, resType, _ string, _ identity.CanonicalUserID) (bool, error) {
		_, hit := ok[resType]
		return hit, nil
	}
}

// Delegation, not elevation — the slot half. A session owner may approve a plan
// but only the portion they themselves hold, so the slot requests recorded as
// GRANTED are the ones the person who clicked can actually speak for. The rest
// stay held for their resource owner and escalate JIT when the agent reaches
// them.
//
// Bounded at decision time rather than on the card, which is what makes partial
// approval work on every transport: there is no subset for the human to compose
// and no multi-select to render, so a channel that can only show approve/deny
// still produces a correctly-bounded grant.
func TestAwaitDecision_theRecordedSlotsAreOnlyThoseTheApproverHolds(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{
		"crm_contact": "contact_access",
		"crm_company": "company_access",
	}
	h.l.PlanGateSlotStanding = map[string]string{
		"crm_contact": spiceboxv1alpha1.StandingRequired,
		"crm_company": spiceboxv1alpha1.StandingRequired,
	}
	h.l.SpiceDBHasAnyOfType = slotStanding("crm_contact")

	recs := driveDecision(t, h, mem, slotAsk(), true)

	got := onlyEventOf(t, recs, plangateaudit.EventPhaseApproved)
	assert.Equal(t, []string{"crm_contact"}, got.Slots,
		"crm_company is not theirs to delegate; it waits for its owner")
}

// Fail closed. A standing lookup that ERRORS must not read as "they hold
// everything" — that would turn a SpiceDB blip into a grant nobody authorized.
// Nothing is recorded, so the phase stays unapproved and simply asks again.
func TestAwaitDecision_aFailedSlotStandingLookupRecordsNoApproval(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_contact": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_contact": spiceboxv1alpha1.StandingRequired}
	h.l.SpiceDBHasAnyOfType = func(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
		return false, errors.New("spicedb unreachable")
	}

	recs := driveDecision(t, h, mem, slotAsk(), true)

	for _, r := range recs {
		assert.NotEqual(t, plangateaudit.EventPhaseApproved, r.Event,
			"an undeterminable subset must grant nothing rather than everything")
	}
}

// Without a standing lookup wired there is nothing to intersect against, and
// the behaviour is the pre-existing one: the requested slots are recorded as
// asked. Safe because a slot request grants no instance any access on its own —
// the per-instance Check still runs at OrderToolCallAuthz, after the gate.
func TestAwaitDecision_withoutSlotStandingTheRequestIsRecordedAsAsked(t *testing.T) {
	h, mem := slotDecisionHost(t)

	recs := driveDecision(t, h, mem, slotAsk(), true)

	got := onlyEventOf(t, recs, plangateaudit.EventPhaseApproved)
	assert.Equal(t, []string{"crm_contact", "crm_company"}, got.Slots)
}

func slotDecisionHost(t *testing.T) (*runnerHost, *memory.Local) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	var published channelevents.Envelope
	h := planGateHost(t, &published)
	h.l.Mem = mem
	return h, mem
}

// onlyEventOf returns the single record of the named event, failing otherwise.
func onlyEventOf(t *testing.T, recs []plangateaudit.Content, event string) plangateaudit.Content {
	t.Helper()
	var out []plangateaudit.Content
	for _, r := range recs {
		if r.Event == event {
			out = append(out, r)
		}
	}
	require.Len(t, out, 1, "expected exactly one %q record; got events %v", event, eventsOf(recs))
	return out[0]
}

// driveDecision publishes the ask, delivers the decision, and returns the log.
func driveDecision(
	t *testing.T, h *runnerHost, mem *memory.Local, ask pipeline.ApprovalAsk, approved bool,
) []plangateaudit.Content {
	t.Helper()
	return driveDecisionAs(t, h, mem, ask, approved, channelevents.ExternalIdentity{
		Kind: identity.KindIdP, ExternalID: "owner@example.com", Email: "owner@example.com",
	})
}

// driveDecisionAs delivers the decision as a NAMED person, carrying the
// structured identity a channel really sends rather than a display string.
//
// The distinction is the whole of the canonicalization bug: production supplies
// `DecidedBy`, an ExternalIdentity, and every fixture used to hand-write an
// "ApproverID" in a shape (`user:owner@example.com`) that no channel produces —
// neither a canonical id nor a raw external one. A test cannot catch an
// encoding error against an input that was never encoded.
func driveDecisionAs(
	t *testing.T, h *runnerHost, mem *memory.Local, ask pipeline.ApprovalAsk,
	approved bool, decidedBy channelevents.ExternalIdentity,
) []plangateaudit.Content {
	t.Helper()
	reqID, err := h.PublishApproval(context.Background(), ask)
	require.NoError(t, err)

	go func() {
		for i := 0; i < 300; i++ {
			if h.l.Approval.PendingForSession("ns/s") > 0 {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		h.l.Approval.DeliverDecision(reqID, approval.Decision{
			Approved:   approved,
			ApproverID: decidedBy.ExternalID.String(),
			Approver:   decidedBy.Principal(),
		})
	}()

	_, _, _, err = h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	return planGateRecords(t, mem)
}

// The card was built, recorded, and never shown. buildPlanGatePending set only
// Lead — no Body, no Fields — so everything the card carries reached the
// `card_built` audit record and stopped there. The human deciding saw one
// sentence: which phase, and a comma-joined list of handles.
//
// That is the whole approval surface the design is about. A tool-call approval
// has rendered Fields the entire time (toolApprovalFields), so the omission
// looked like parity and read as intentional.
func TestPublishApproval_planPhaseRendersTheCardAndNotJustTheLead(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published)

	ask := planPhaseAsk()
	// The REAL type the gate puts in the payload. The map form a fixture can
	// reach for is not what production produces, and a reader that only
	// understands the map would pass here and render nothing in the cluster.
	ask.Payload["card"] = plangate.Card{
		Lead: "Plan approval",
		What: "perm:fetch:git_repo\n\nAlso asks to reach:\n  git_repo — https://github.com/acme/app",
		Why:  "the ticket names this repo",
		When: "phase 1 of 3",
	}

	reqID, err := h.PublishApproval(context.Background(), ask)
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	payload := decodeInteractionRequest(t, published)
	require.NotEmpty(t, payload.Fields,
		"a card nobody renders is a card nobody reads; the decision was being made on the lead alone")

	byLabel := map[string]string{}
	for _, f := range payload.Fields {
		byLabel[f.Label] = f.Value
	}

	what, hasWhat := byLabel["What"]
	require.True(t, hasWhat, "the computed description is the half the approver is meant to trust")
	assert.Contains(t, what, "https://github.com/acme/app",
		"the concrete resource is the reason one approval can cover the phase")

	// The agent's words must be present AND visibly attributed. Rendering them
	// unlabelled would put a prompt-injected agent's prose next to the system's
	// own description with nothing to tell them apart.
	var whyLabel, whyValue string
	for l, v := range byLabel {
		if strings.Contains(v, "the ticket names this repo") {
			whyLabel, whyValue = l, v
		}
	}
	require.NotEmpty(t, whyValue, "the agent's justification is worth showing")
	assert.NotEqual(t, "What", whyLabel, "it must not be rendered as the trusted half")
	assert.Regexp(t, `(?i)agent`, whyLabel,
		"the label has to say whose claim this is, or the two halves are indistinguishable")
}

// planScopedAsk is the ask a plan-scoped card raises: one decision that clears
// several phases, each carrying its own grant.
func planScopedAsk() pipeline.ApprovalAsk {
	a := planPhaseAsk()
	a.CoalesceKey = "plan:" + planPhaseDigest
	a.Payload["covered"] = []plangateaudit.Content{
		{PhaseIndex: new(int32(0)), PhaseKey: "key-recon", Ceiling: []string{"perm:fetch:git_repo"},
			Slots: []string{"crm_contact"}, MaxCount: 1},
		{PhaseIndex: new(int32(1)), PhaseKey: "key-edit", Ceiling: []string{"perm:read:git_repo"},
			Slots: []string{"crm_contact"}, MaxCount: 2, Requires: []int{0}},
		{PhaseIndex: new(int32(2)), PhaseKey: "key-ship", Ceiling: []string{"perm:push:git_repo"},
			Slots: []string{"crm_company"}, MaxCount: 1, Requires: []int{1}},
	}
	return a
}

func approvalsIn(recs []plangateaudit.Content) []plangateaudit.Content {
	var out []plangateaudit.Content
	for _, r := range recs {
		if r.Event == plangateaudit.EventPhaseApproved {
			out = append(out, r)
		}
	}
	return out
}

// One yes, one record PER PHASE it cleared.
//
// The fold keys approval on a phase's authority, so a plan-scoped card that
// wrote a single record would clear exactly one phase — the human answers once
// and is asked again at the next phase boundary, which is the serial prompting
// this whole move exists to end.
func TestAwaitDecision_planScopedApprovalIsRecordedPerCoveredPhase(t *testing.T) {
	_, recs := decideOnce(t, planScopedAsk(), true)

	approvals := approvalsIn(recs)
	require.Len(t, approvals, 3,
		"one record per phase the card cleared; got events %v", eventsOf(recs))

	byKey := map[string]plangateaudit.Content{}
	for _, r := range approvals {
		byKey[r.PhaseKey] = r
	}
	for _, want := range []struct {
		key     string
		index   int32
		ceiling []string
		max     int
	}{
		{"key-recon", 0, []string{"perm:fetch:git_repo"}, 1},
		{"key-edit", 1, []string{"perm:read:git_repo"}, 2},
		{"key-ship", 2, []string{"perm:push:git_repo"}, 1},
	} {
		got, ok := byKey[want.key]
		require.True(t, ok, "phase %d was shown on the card and must be cleared by it", want.index)
		require.NotNil(t, got.PhaseIndex)
		assert.Equal(t, want.index, *got.PhaseIndex)
		assert.Equal(t, want.ceiling, got.Ceiling,
			"each phase is granted ITS OWN reach; one shared ceiling would grant every "+
				"phase the push")
		assert.Equal(t, want.max, got.MaxCount)
		assert.Equal(t, planPhaseDigest, got.PlanDigest)
	}
}

// Delegation applies per phase, not once for the plan. A card covering three
// phases that request different resource types must record each phase's grant
// bounded by what this approver can speak for — a union recorded on every phase
// would hand each of them the others' slots.
func TestAwaitDecision_planScopedSlotsAreBoundedPerPhase(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{
		"crm_contact": "contact_access",
		"crm_company": "company_access",
	}
	h.l.PlanGateSlotStanding = map[string]string{
		"crm_contact": spiceboxv1alpha1.StandingRequired,
		"crm_company": spiceboxv1alpha1.StandingRequired,
	}
	h.l.SpiceDBHasAnyOfType = slotStanding("crm_contact")

	recs := driveDecision(t, h, mem, planScopedAsk(), true)

	byKey := map[string]plangateaudit.Content{}
	for _, r := range approvalsIn(recs) {
		byKey[r.PhaseKey] = r
	}
	require.Len(t, byKey, 3)
	assert.Equal(t, []string{"crm_contact"}, byKey["key-recon"].Slots)
	assert.Equal(t, []string{"crm_contact"}, byKey["key-edit"].Slots)
	assert.Empty(t, byKey["key-ship"].Slots,
		"crm_company is not this approver's to delegate; the ship phase is cleared "+
			"to run and still has to escalate when it reaches the company")
}

// A no is still ONE denial. Approving covers every phase; refusing refuses the
// call in front of the human, and the agent's route out is to re-plan — writing
// three denials would key a refusal to two phases nobody had reached.
func TestAwaitDecision_planScopedDenialIsRecordedOnce(t *testing.T) {
	_, recs := decideOnce(t, planScopedAsk(), false)

	assert.Empty(t, approvalsIn(recs), "a no grants nothing")
	var denials int
	for _, r := range recs {
		if r.Event == plangateaudit.EventDenied {
			denials++
		}
	}
	assert.Equal(t, 1, denials, "got events %v", eventsOf(recs))
}

// THE canonicalization bug, caught live: an approval was thrown away because
// SpiceDB was asked about `user:admin@ap.local`, which is not a legal object id.
//
// `identity.CanonicalFromTrusted(..., "test fixture")` is a TYPE, so writing it around a raw string
// is a conversion, not an encoding — it launders an unencoded email into the
// type whose whole purpose is to make that a compile error. Decision.ApproverID
// is `DecidedBy.ExternalID.String()`, a raw external id that is NEVER canonical
// and never carries a "user:" prefix, so nothing downstream corrected it.
//
// The fake here asserts on the value rather than ignoring it, which is the gap
// that let this ship: every existing slot test passes with any encoding at all.
func TestAwaitDecision_theStandingLookupAsksAboutTheCANONICALApprover(t *testing.T) {
	want, err := identity.EmailReference("admin@ap.local").Canonical()
	require.NoError(t, err)

	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_contact": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_contact": spiceboxv1alpha1.StandingRequired}

	var asked []identity.CanonicalUserID
	h.l.SpiceDBHasAnyOfType = func(
		_ context.Context, _, _ string, id identity.CanonicalUserID,
	) (bool, error) {
		asked = append(asked, id)
		return true, nil
	}

	ask := planPhaseAsk()
	ask.Payload["slots"] = []string{"crm_contact"}
	driveDecisionAs(t, h, mem, ask, true, channelevents.ExternalIdentity{
		Kind: identity.KindIdP, ExternalID: "admin@ap.local", Email: "admin@ap.local",
	})

	require.NotEmpty(t, asked, "the standing lookup must run at all")
	assert.Equal(t, want, asked[0],
		"a raw email reaches SpiceDB as an illegal object id, the lookup errors, "+
			"and the approval is discarded with the approver told nothing")
}

// A discarded approval must SAY SO. This is the no-silent-errors rule at its
// sharpest: the failure is on a thing a human is actively waiting for, and the
// design's stated recovery — "leave it to be re-asked" — is only recovery if
// the re-ask can succeed. When it cannot, the human approves, sees the same
// card, approves again, and the system looks like it is working.
func TestAwaitDecision_aDiscardedApprovalTellsTheApprover(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	var published []channelevents.Envelope
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	h.l.InteractionRequestPublish = func(_ context.Context, _, _ string, env channelevents.Envelope) error {
		published = append(published, env)
		return nil
	}
	h.l.PlanGateSlotPermissions = map[string]string{"crm_contact": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_contact": spiceboxv1alpha1.StandingRequired}
	h.l.SpiceDBHasAnyOfType = func(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
		return false, errors.New("permission service unreachable")
	}

	recs := driveDecision(t, h, mem, slotAsk(), true)

	require.Empty(t, approvalsIn(recs), "an undeterminable subset must grant nothing")

	var told bool
	for _, env := range published {
		pl := decodeInteractionRequest(t, env)
		if pl.Category == categories.InternalError {
			told = true
			assert.NotEmpty(t, pl.Lead, "a notice with no text tells nobody anything")
			// House style for a degraded-tone notice, and the substance of the
			// complaint: saying "something went wrong" and stopping leaves the
			// reader holding an unanswered card.
			assert.NotEmpty(t, pl.NextStep,
				"a notice that does not say what to do next is the silent failure with extra words")
			for _, leak := range []string{"SpiceDB", "slot_grant", "kubectl", "relation", "perm:"} {
				assert.NotContains(t, pl.Lead, leak,
					"a chat surface is not an operator console")
			}
		}
	}
	assert.True(t, told,
		"the approver clicked Approve and nothing happened; saying nothing turns a "+
			"silent error into a silent loop")
}

// The channel must receive STRUCTURE, not prose.
//
// planGateFields flattened the whole card into four Label/Value pairs whose
// Value was one pre-rendered multi-line string, so every surface could only
// print it back. Webchat — which has a design system, a severity signal and
// room to lay out a tree — rendered the same grey run-on text a plain-text
// surface gets, because prose is all it was given.
//
// InteractionField already documents the shape for this on Mentions: carry the
// structure to the consumer that can act on it, and keep Value as the display
// fallback for surfaces that cannot.
func TestPublishApproval_planGateCardTravelsAsStructure(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published)

	ask := planPhaseAsk()
	ask.Payload["card"] = plangate.Card{
		Lead: "⚠️ Plan approval",
		What: "Phase 1:\n  Fetch git_repo\nPhase 2:\n  Push git_repo  (leaves this session)",
		Phases: []plangate.CardPhase{
			{Title: "Phase 1", Permissions: []plangate.CardLine{{Text: "Fetch git_repo"}},
				Resources: []plangate.CardLine{{Text: "git_repo", Detail: "https://github.com/demo-org/demo-repo"}}},
			{Title: "Phase 2", Permissions: []plangate.CardLine{{Text: "Push git_repo", External: true}},
				Resources: []plangate.CardLine{{Text: "git_repo", Detail: "https://github.com/demo-org/demo-repo"}}},
		},
		Coverage: "Approving covers every phase above.",
		Severity: "elevated",
	}

	reqID, err := h.PublishApproval(context.Background(), ask)
	require.NoError(t, err)
	require.NoError(t, h.pending().m[reqID].onPublish(context.Background()))

	pl := decodeInteractionRequest(t, published)

	var what *channelevents.InteractionField
	for i := range pl.Fields {
		if pl.Fields[i].Label == "What" {
			what = &pl.Fields[i]
		}
	}
	require.NotNil(t, what, "the computed half must still be present")
	require.Len(t, what.Items, 3, "one item per phase, so a surface can lay them out, plus coverage")

	assert.Equal(t, "Phase 1", what.Items[0].Text)
	require.NotEmpty(t, what.Items[1].Items, "a phase's lines are its children, not its prose")
	assert.True(t, hasExternalItem(what.Items[1].Items),
		"the irreversible step must be flagged structurally, not only spelled out in text")

	// What one click buys is a claim about the whole card, so it travels as its
	// own line rather than trailing the last phase.
	assert.Contains(t, what.Items[2].Text, "covers every phase")
	assert.Equal(t, channelevents.ToneMuted, what.Items[2].Tone)

	assert.NotEmpty(t, what.Value,
		"Value stays the always-present fallback for surfaces that render no structure")
}

func hasExternalItem(items []channelevents.InteractionItem) bool {
	for _, it := range items {
		if it.Tone == channelevents.ToneExternal {
			return true
		}
	}
	return false
}

// The plan gate's own approval must narrow, not merely grant. A human shown
// "reaches crm_company — 4210" and clicking approve has said which company;
// recording that as a grant alone leaves every other company permitted by
// anything that happens to allow it.
func TestAwaitDecision_planGateApprovalNarrowsScope(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}

	ask := planPhaseAsk()
	ask.Payload["slots"] = []string{"crm_company"}
	ask.Payload["slotValues"] = map[string]string{"crm_company": "4210"}

	driveDecision(t, h, mem, ask, true)

	assert.Equal(t, []string{"4210"}, scopedIDs(t, mem, "crm_company"),
		"approving this company must exclude every other one, not merely permit this one")
}

// The feature's central claim, proven at the level a human's click actually
// exercises: session-only drops exactly one pre-check (did the approver
// already hold it) and changes nothing downstream. The grant this produces
// must still be WRITTEN and still narrow to the named instance, the same as
// the required path above — otherwise "we dropped one lookup" would quietly
// have become "we turned enforcement off". Both lookup hooks fail the test if
// called, so a regression that re-adds the lookup for a session-only type
// fails here too, not just in the ApproverCanDelegateSlots-level tests.
func TestAwaitDecision_sessionOnlyStillBindsTheNamedInstance(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_company": spiceboxv1alpha1.StandingSessionOnly}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("a session-only type must not be asked about")
		return false, nil
	}
	h.l.SpiceDBHasAnyOfType = func(context.Context, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("a session-only type must not be asked about")
		return false, nil
	}

	ask := planPhaseAsk()
	ask.Payload["slots"] = []string{"crm_company"}
	ask.Payload["slotValues"] = map[string]string{"crm_company": "4210"}

	recs := driveDecision(t, h, mem, ask, true)

	assert.Equal(t, []string{"4210"}, scopedIDs(t, mem, "crm_company"),
		"session-only still narrows the written grant to the named instance — no lookup, "+
			"but the grant, its expiry, its session scope, and the tool-call Check that resolves "+
			"against it are all unchanged")

	got := onlyEventOf(t, recs, plangateaudit.EventPhaseApproved)
	assert.Equal(t, []string{"crm_company"}, got.Slots,
		"the slot is recorded granted: the approver's decision is the whole authority for a "+
			"session-only type")
}

// scopedIDs reads back the instances session_scope now records for one type.
//
// The whole observable half of a slot binding on the memory side, and shared
// because three tests make the same claim about different inputs; ScopeResource
// carries IDs as a []string, so a per-type read has to flatten them.
func scopedIDs(t *testing.T, mem *memory.Local, resourceType string) []string {
	t.Helper()
	got, _, err := sessionscope.Get(
		memory.WithSystemApproval(context.Background(), "test"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"})
	require.NoError(t, err, "reading session_scope")

	var ids []string
	for _, r := range got.Resources {
		if r.ResourceType == resourceType {
			ids = append(ids, r.IDs...)
		}
	}
	return ids
}

// installInstanceStanding wires BOTH standing lookups the way production does:
// per-instance affirmative for exactly the pairs named, and TYPE-level
// affirmative for any type the approver holds at least one instance of.
//
// Keeping the type-level lookup wired and TRUE is the point of the helper. That
// is precisely the answer the gate used to act on, so a fix that added the
// instance hook without actually consulting it would still satisfy every
// positive case and only fail where the two answers disagree.
func installInstanceStanding(t *testing.T, h *runnerHost, held ...plangateaudit.SlotRef) {
	t.Helper()
	instances := map[plangateaudit.SlotRef]struct{}{}
	types := map[string]struct{}{}
	for _, r := range held {
		instances[r] = struct{}{}
		types[r.Type] = struct{}{}
	}
	h.l.SpiceDBHasOnResource = func(
		_ context.Context, resType, resID, _ string, _ identity.CanonicalUserID,
	) (bool, error) {
		_, ok := instances[plangateaudit.SlotRef{Type: resType, ID: resID}]
		return ok, nil
	}
	h.l.SpiceDBHasAnyOfType = func(
		_ context.Context, resType, _ string, _ identity.CanonicalUserID,
	) (bool, error) {
		_, ok := types[resType]
		return ok, nil
	}
}

// Type-level standing must never become an instance-level grant.
//
// The approval writes a grant on the SPECIFIC instance the phase named, so
// answering "does this approver hold contact_access on ANY company" and then
// granting the one the phase happened to name hands the session live access to
// a record its approver has no standing on whatsoever. The approver here owns
// 4210 and only 4210; the phase names 4299, which belongs to somebody else.
//
// This was inert before the approval wrote a grant at all — the per-instance
// Check at OrderToolCallAuthz was the only thing that ever authorized an
// instance, and it still ran after the gate. Writing the grant is what turned a
// too-broad question into an escalation.
func TestAwaitDecision_standingOnOneInstanceDoesNotGrantAnother(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired}
	installInstanceStanding(t, h, plangateaudit.SlotRef{Type: "crm_company", ID: "4210"})

	ask := planPhaseAsk()
	ask.Payload["slots"] = []string{"crm_company"}
	ask.Payload["slotValues"] = map[string]string{"crm_company": "4299"}

	recs := driveDecision(t, h, mem, ask, true)

	assert.Empty(t, scopedIDs(t, mem, "crm_company"),
		"the approver holds nothing on 4299; approving a card that named it must bind nothing")

	got := onlyEventOf(t, recs, plangateaudit.EventPhaseApproved)
	assert.Empty(t, got.Slots,
		"a slot recorded as granted is read back by the fold as authority the approver conferred; "+
			"4299 was never theirs to confer")
}

// The same rule across a plan-scoped card, which is where it actually bites: one
// click covers several phases, and the phases name different instances of the
// same type. Standing on the first one must clear that phase and no other.
func TestAwaitDecision_planScopedApprovalBindsOnlyTheInstancesTheApproverHolds(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired}
	installInstanceStanding(t, h, plangateaudit.SlotRef{Type: "crm_company", ID: "4210"})

	// Covers two phases: one naming 4210, one naming 9931.
	recs := driveDecision(t, h, mem, planScopedAskWithDifferingInstances(), true)

	assert.Equal(t, []string{"4210"}, scopedIDs(t, mem, "crm_company"),
		"9931 is a different company with a different owner; one approver's standing on "+
			"4210 does not speak for it")

	byKey := map[string]plangateaudit.Content{}
	for _, r := range approvalsIn(recs) {
		byKey[r.PhaseKey] = r
	}
	require.Len(t, byKey, 2)
	assert.Equal(t, []string{"crm_company"}, byKey["key-first"].Slots)
	assert.Empty(t, byKey["key-second"].Slots,
		"the phase that named 9931 is cleared to RUN and still has to escalate when it "+
			"reaches the company")
	assert.Empty(t, byKey["key-second"].SlotRefs,
		"both spellings narrow together, or carry-over reads the held-back instance back "+
			"as granted")
}

// planScopedAskWithDifferingInstances raises a plan-scoped card covering two
// phases that both touch crm_company, but name two DIFFERENT instances — the
// real shape a plan takes when one phase reads one company record and a
// later phase acts on another (the git analogue: clone from upstream/repo,
// push to fork/repo). Each covered phase carries its OWN SlotRefs, exactly
// the way plangate.PhaseAuthorityRecord produces them in production — unlike
// planScopedAsk's fixtures above, which predate SlotRefs and so cannot
// exercise this.
func planScopedAskWithDifferingInstances() pipeline.ApprovalAsk {
	a := planPhaseAsk()
	a.CoalesceKey = "plan:" + planPhaseDigest
	a.Payload["covered"] = []plangateaudit.Content{
		{PhaseIndex: new(int32(0)), PhaseKey: "key-first", Ceiling: []string{"perm:read:crm_company"},
			Slots:    []string{"crm_company"},
			SlotRefs: []plangateaudit.SlotRef{{Type: "crm_company", ID: "4210"}},
			MaxCount: 1},
		{PhaseIndex: new(int32(1)), PhaseKey: "key-second", Ceiling: []string{"perm:read:crm_company"},
			Slots:    []string{"crm_company"},
			SlotRefs: []plangateaudit.SlotRef{{Type: "crm_company", ID: "9931"}},
			MaxCount: 1, Requires: []int{0}},
	}
	return a
}

// The defect this fix closes: narrowing keyed off a single active-phase
// slotValues map collapses two covered phases' distinct targets into one,
// so approving a card that clears "read 4210" and "read 9931" would leave
// only whichever phase's instance happened to be on the map — denying the
// other phase's already-approved call. Each covered record must narrow to
// ITS OWN instance, so both land in scope.
func TestAwaitDecision_planGateApprovalNarrowsScopeAcrossDifferingCoveredInstances(t *testing.T) {
	h, mem := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}

	driveDecision(t, h, mem, planScopedAskWithDifferingInstances(), true)

	assert.ElementsMatch(t, []string{"4210", "9931"}, scopedIDs(t, mem, "crm_company"),
		"two covered phases naming different instances of the same type must BOTH narrow into scope")
}

// The approval path must ask SpiceDB about the DERIVED id, not the raw URL the
// agent wrote in its plan. Before this, `git_repo:https://github.com/...`
// reached SpiceDB, which rejects it on the object-id charset regex — so the
// standing lookup errored and a human's approval was discarded.
func TestApproverCanDelegateSlots_AsksAboutTheDerivedID(t *testing.T) {
	h, _ := slotDecisionHost(t)
	var asked []string
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "push"}
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingRequired}
	h.l.PlanGateSlotTransforms = map[string][]string{
		"git_repo": {"normalize_url", "spicedb_escape"},
	}
	h.l.SpiceDBHasOnResource = func(_ context.Context, resType, resID, perm string, _ identity.CanonicalUserID) (bool, error) {
		asked = append(asked, resType+":"+resID+"#"+perm)
		return true, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Len(t, got, 1, "the approver holds push, so the slot is delegable")
	assert.Equal(t, []string{"git_repo:https=3A//github=2Ecom/acme/app#push"}, asked)
}

// A value that cannot be derived is held back rather than asked about raw.
//
// PlanGateSlotStanding is set to `required` here, or the type's default
// (session-only) would bypass the lookup entirely — the bypass runs BEFORE
// id-derivation is even attempted, so an undrivable value would be waved
// through rather than held back, and this test would stop exercising the
// thing it is named for.
func TestApproverCanDelegateSlots_UndrivableValueIsHeldBack(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "push"}
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingRequired}
	h.l.PlanGateSlotTransforms = map[string][]string{"git_repo": {"no_such_transform"}}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("SpiceDB must not be asked about a value that failed to derive")
		return false, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err, "one bad slot must not fail the whole approval")
	assert.Empty(t, got, "held back, not waved through")
}

// A session-only type binds on the approver's say-so: no lookup is made, and
// the slot is delegable even though nobody holds anything on the instance.
// SpiceDB holds no upstream truth for a type like this (its permissions live
// at the forge, not in SpiceDB), so there is no standing to check — the
// approver's decision on the card is the whole authority.
func TestApproverCanDelegateSlots_SessionOnlySkipsTheLookup(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "push"}
	h.l.PlanGateSlotTransforms = map[string][]string{"git_repo": {"normalize_url", "spicedb_escape"}}
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingSessionOnly}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("a session-only type must not be asked about")
		return false, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Len(t, got, 1, "the approval is the authority for a session-only type")
}

// The bypass must run BEFORE id-derivation, not after — SessionOnlySkipsTheLookup
// above uses a value that derives cleanly, so a bypass placed after derivation
// would still satisfy it. This case closes that gap: the transform chain here
// cannot parse the value at all, and the slot must still be admitted, because a
// session-only type needs no object id to ask SpiceDB about in the first place.
func TestApproverCanDelegateSlots_SessionOnlyBypassesEvenAnUndrivableValue(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "push"}
	h.l.PlanGateSlotTransforms = map[string][]string{"git_repo": {"no_such_transform"}}
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingSessionOnly}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("a session-only type must not be asked about")
		return false, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Len(t, got, 1, "an undrivable value must not block a slot that needs no lookup at all")
}

// A required type is unchanged: no standing, no delegation.
func TestApproverCanDelegateSlots_RequiredStillAsks(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
	h.l.PlanGateSlotStanding = map[string]string{"crm_company": spiceboxv1alpha1.StandingRequired}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		return false, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "crm_company", ID: "4210"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Empty(t, got, "no standing, no delegation")
}

// An unmapped type must not become required by accident: session-only is the
// default everywhere, including when AgentClass.status has not published yet
// (the AgentClass controller may not have caught up when a session starts) —
// treating that window as strict would make every slot unbindable until it
// does. The lookup hook fails the test if called at all, so this genuinely
// proves the hook is never consulted rather than merely returning the right
// answer despite being asked.
func TestApproverCanDelegateSlots_UnmappedTypeIsSessionOnly(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "push"}
	h.l.PlanGateSlotStanding = nil
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		t.Fatal("an unmapped type defaults to session-only and must not be asked about")
		return false, nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "acme/app"}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Len(t, got, 1, "an unmapped type defaults to session-only, so the approval is the authority")
}

// classWithGatedCompany builds an AgentClass declaring one crm_company slot. A
// non-empty cel gives it a precondition over the observed `is_public` fact; an
// empty cel gives it none, which is the regression-guard shape (an ordinary
// phase whose enforcement must be a no-op).
func classWithGatedCompany(cel string) *spiceboxv1alpha1.AgentClass {
	slot := spiceboxv1alpha1.AuthzSlot{
		ResourceType: "crm_company",
		Description:  "a CRM company the agent may reach",
		Permission:   "contact_access",
	}
	if cel != "" {
		slot.Requires = []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              cel,
			UndeterminedHint: "call crm_lookup to observe the company first",
			RefusalMessage:   "this company's records are restricted",
		}}
	}
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{slot}},
		},
	}
}

// recordCompanyFact writes the observed `is_public` fact the slot's precondition
// reads, keyed by the RAW company id — the same key LookupSubject derives at
// bind time, so the gate actually finds it.
func recordCompanyFact(t *testing.T, mem *memory.Local, rawID string, isPublic bool) {
	t.Helper()
	require.NoError(t, observedfact.Record(
		memory.WithSystemApproval(context.Background(), "test"),
		mem, memory.Scope{Kind: "session", ID: "ns/s"},
		factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: "crm_company", ResourceID: rawID}},
			Facts:    map[string]any{"is_public": isPublic},
			Source:   factcontent.Source{ToolName: "crm_lookup"},
		}))
}

// The confused-deputy gap this task closes, proven at the level a human's click
// exercises: approving a PLAN PHASE binds the phase's slot refs through
// authz.BindApproved, and that must NOT silently waive a slot precondition. A
// plan-phase approval consents to the phase; the risk a precondition guards is a
// SEPARATE consent, asked for only by the waiver card.
//
// The Refused row is the security claim: the phase names a company whose
// precondition Refuses, and approving the plan-gate card must bind nothing for
// it — the instance stays unbound and escalates to the waiver card at its next
// tool call. The Satisfied and no-precondition rows are the regression guard: a
// gate that is met, or a slot that declares none, must bind exactly as before,
// because wedging that path breaks EVERY approved phase.
//
// crm_company is session-only here (no PlanGateSlotStanding entry), so the
// approver's click is the whole delegation authority and the standing lookups
// stay out of it — the ONLY thing that can hold "4210" out of the binding is its
// precondition, which is exactly what the test means to isolate.
func TestAwaitDecision_planGateApprovalEnforcesSlotPreconditions(t *testing.T) {
	const isPublic = `facts.observed.is_public == true`

	cases := []struct {
		name string
		// cel is the slot's precondition, empty for a slot declaring none.
		cel string
		// seed records the gating fact, or is nil to leave it unobserved
		// (Undetermined). A *bool distinguishes "recorded false" from "unrecorded".
		fact      *bool
		wantBound bool
		why       string
	}{
		{
			name:      "Refused precondition: approving the phase binds nothing for the gated instance",
			cel:       isPublic,
			fact:      boolPtr(false),
			wantBound: false,
			why:       "a plan-phase approval must not silently waive a Refused precondition; the instance escalates to the waiver card",
		},
		{
			name:      "Satisfied precondition: the instance binds, as it must for the gate to be usable",
			cel:       isPublic,
			fact:      boolPtr(true),
			wantBound: true,
			why:       "a met precondition must not hold the approved instance out of its slot",
		},
		{
			name:      "Undetermined precondition (fact unobserved): the instance is held, not bound",
			cel:       isPublic,
			fact:      nil,
			wantBound: false,
			why:       "undetermined never binds; the fact is not observed yet, so approval binds nothing until it is",
		},
		{
			name:      "no precondition declared: the ordinary phase binds exactly as before",
			cel:       "",
			fact:      boolPtr(false), // present but irrelevant — an ungated slot ignores it
			wantBound: true,
			why:       "enforcing preconditions must be a no-op for a slot that declares none, or every approved phase wedges",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mem := slotDecisionHost(t)
			h.l.PlanGateSlotPermissions = map[string]string{"crm_company": "contact_access"}
			h.l.AgentClass = classWithGatedCompany(tc.cel)
			if tc.fact != nil {
				recordCompanyFact(t, mem, "4210", *tc.fact)
			}

			ask := planPhaseAsk()
			ask.Payload["slots"] = []string{"crm_company"}
			ask.Payload["slotValues"] = map[string]string{"crm_company": "4210"}

			driveDecision(t, h, mem, ask, true)

			if tc.wantBound {
				assert.Equal(t, []string{"4210"}, scopedIDs(t, mem, "crm_company"), tc.why)
			} else {
				assert.Empty(t, scopedIDs(t, mem, "crm_company"), tc.why)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// A StandingRequired slot may declare a PRIMARY permission plus ADDITIONAL ones
// (its EffectivePermissions set). The delegation gate must verify the approver
// holds EACH permission it is about to bind — not just the slot's primary — or
// an approver who holds `read` on a repo clicks Approve and the agent is granted
// `push` on it, a repo the approving human cannot push to. The gate's own doc
// says holding read "says nothing about a slot whose grant confers write."
func TestApproverCanDelegateSlots_ChecksEveryPermission_NotJustThePrimary(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "read"}                // primary
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"read", "push"}} // primary ∪ additional
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingRequired}
	h.l.PlanGateSlotTransforms = map[string][]string{"git_repo": {"normalize_url", "spicedb_escape"}}

	// The approver holds read but NOT push.
	h.l.SpiceDBHasOnResource = func(_ context.Context, _, _, perm string, _ identity.CanonicalUserID) (bool, error) {
		return perm == "read", nil
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app", Standing: spiceboxv1alpha1.StandingRequired}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	require.Len(t, got, 1, "the ref is delegable — the approver holds at least one of its permissions")

	var ref plangateaudit.SlotRef
	for r := range got {
		ref = r
	}
	held := got[ref]
	assert.Contains(t, held, "read", "read is held and delegable")
	assert.NotContains(t, held, "push", "push is NOT held and must not be delegable — this is the over-grant")
}

// If the approver holds NONE of a required slot's permissions, the ref is not
// delegable at all (empty held set → dropped).
func TestApproverCanDelegateSlots_HoldingNonePermissionsDropsTheRef(t *testing.T) {
	h, _ := slotDecisionHost(t)
	h.l.PlanGateSlotPermissions = map[string]string{"git_repo": "read"}
	h.l.PlanGateSlotPermissionSets = map[string][]string{"git_repo": {"read", "push"}}
	h.l.PlanGateSlotStanding = map[string]string{"git_repo": spiceboxv1alpha1.StandingRequired}
	h.l.PlanGateSlotTransforms = map[string][]string{"git_repo": {"normalize_url", "spicedb_escape"}}
	h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
		return false, nil // holds nothing
	}

	got, err := h.approverCanDelegateSlots(context.Background(),
		[]plangateaudit.SlotRef{{Type: "git_repo", ID: "https://github.com/acme/app", Standing: spiceboxv1alpha1.StandingRequired}},
		identity.EmailReference("approver@example.com"))
	require.NoError(t, err)
	assert.Empty(t, got, "no permission held → nothing to delegate")
}
