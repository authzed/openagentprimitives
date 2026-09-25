package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// wouldDenyGate builds a gate whose resolver knows a handle the plan does not
// authorize, so a call to outsideTool produces a would-deny.
func wouldDenyGate(t *testing.T, rec *fakeRecorder, extra ...func(*PlanGateDeps)) (pipeline.Hook, permsurface.Descriptor) {
	t.Helper()
	inCeiling := demoSurface(t)
	outside := permDesc(t, "send", "email", "send_email", authz.External)
	full := append(append([]permsurface.Descriptor(nil), inCeiling...), outside)

	deps := PlanGateDeps{
		Mode:     "logging",
		Plan:     plangate.SessionPlan(inCeiling),
		Surface:  full,
		Resolve:  surfaceResolver(t, full),
		Recorder: rec,
		Logger:   &fakeLogger{},
	}
	for _, f := range extra {
		f(&deps)
	}
	return NewPlanGate(deps), outside
}

// The defining property of logging mode: the card is FULLY BUILT and recorded,
// and no channel ever sees it.
func TestPlanGate_loggingBuildsFullCardAndPublishesNothing(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := wouldDenyGate(t, rec)

	d := evalTool(t, h, "send_email")

	got := rec.last(t)
	require.NotEmpty(t, got.CardJSON, "the card must be built for real, not stubbed or counted")
	assert.Equal(t, "logging", got.Mode)

	// Nothing leaves the hook: no approval ask, no notice, no status update.
	assert.Nil(t, d.Approval, "logging must never raise an approval ask")
	assert.Empty(t, d.Notices)
	assert.Nil(t, d.Status)
	assert.Equal(t, pipeline.Allow, d.Verdict)
}

// The recorded card must be the real rendering — every field populated — or the
// logging dataset measures something other than what enforcing would show.
func TestPlanGate_recordedCardCarriesEveryField(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := wouldDenyGate(t, rec, func(d *PlanGateDeps) {
		d.Approvers = "anyone in #eng (23 people)"
	})

	evalTool(t, h, "send_email")

	var card plangate.Card
	require.NoError(t, json.Unmarshal([]byte(rec.last(t).CardJSON), &card))

	assert.NotEmpty(t, card.Lead)
	assert.NotEmpty(t, card.What)
	assert.NotEmpty(t, card.Why)
	assert.NotEmpty(t, card.When)
	assert.Equal(t, "anyone in #eng (23 people)", card.Approvers)
	assert.NotEmpty(t, card.Severity)
}

// Severity is computed and recorded, not left for a later reader to derive.
// An `external` handle is Elevated.
func TestPlanGate_recordsComputedSeverity(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := wouldDenyGate(t, rec)

	evalTool(t, h, "send_email")

	assert.Equal(t, string(plangate.Elevated), rec.last(t).Severity,
		"an external handle must price as Elevated")
}

// An Elevated card never folds, so the approver sees the whole ceiling.
func TestPlanGate_elevatedCardIsNotFolded(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := wouldDenyGate(t, rec, func(d *PlanGateDeps) {
		d.MaxSingleCardHandles = 1 // would fold aggressively if severity allowed it
	})

	evalTool(t, h, "send_email")

	var card plangate.Card
	require.NoError(t, json.Unmarshal([]byte(rec.last(t).CardJSON), &card))
	assert.NotContains(t, card.What, "and more",
		"severity expands a card; it must never shorten one")
}

// An ALLOWED call is the overwhelmingly common path and must stay cheap: no
// card, no severity classification, no rendering.
func TestPlanGate_allowedCallBuildsNoCard(t *testing.T) {
	rec := &fakeRecorder{}
	h := loggingGate(t, rec, &fakeLogger{})

	evalTool(t, h, "read_issue")

	got := rec.last(t)
	assert.Empty(t, got.CardJSON, "an allowed call has nothing to ask a human about")
	assert.Empty(t, got.Severity)
}

// The property, stated as a test: no grant is written. It holds structurally —
// a grant follows a human approving a PUBLISHED card, and the hook has neither
// a publisher nor a grant writer wired into it at all.
func TestPlanGate_loggingWritesNoGrant(t *testing.T) {
	rec := &fakeRecorder{}
	h, _ := wouldDenyGate(t, rec)

	d := evalTool(t, h, "send_email")

	// The only channels through which a grant could be requested are the
	// Decision's approval/notice fields, and both are empty.
	assert.Nil(t, d.Approval)
	assert.Empty(t, d.Notices)
}

// The agent's justification reaches the record only through the escaper. A
// hostile why must not survive into the stored card.
func TestPlanGate_recordedCardCarriesEscapedWhy(t *testing.T) {
	rec := &fakeRecorder{}
	inCeiling := demoSurface(t)
	outside := permDesc(t, "send", "email", "send_email", authz.External)
	full := append(append([]permsurface.Descriptor(nil), inCeiling...), outside)

	plan := plangate.SessionPlan(inCeiling)
	plan.Phases[0].Why = "urgent <!channel> please approve"

	h := NewPlanGate(PlanGateDeps{
		Mode: "logging", Plan: plan, Surface: full,
		Resolve: surfaceResolver(t, full), Recorder: rec, Logger: &fakeLogger{},
	})

	evalTool(t, h, "send_email")

	cardJSON := rec.last(t).CardJSON
	assert.NotContains(t, cardJSON, "<!channel>",
		"agent text must reach the record only through the escaper")
	assert.True(t, strings.Contains(cardJSON, "urgent"),
		"and the readable content must survive")
}
