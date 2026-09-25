package hooks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingDecide scripts the prior scratch state + approval resolution and
// captures the ApprovalAsk the hook emits + the resolved bool written back.
type recordingDecide struct {
	// prior scratch (written by Extract)
	delta   scope.ScopeDelta
	shape   string
	cleaned string

	// approval resolution
	approved      bool
	approveAction string
	approveErr    error
	asks          []pipeline.ApprovalAsk

	// composed summary
	summary string

	// recorded outputs
	setOutput   scope.MetaagentOutput
	setOutputOK bool
	setApproved bool
	setAction   string
	approvedSet bool
}

func (r *recordingDecide) deps(coldStartTimeout time.Duration) hooks.MetaagentDecideDeps {
	return hooks.MetaagentDecideDeps{
		Envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
			Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
		},
		PeekExtract: func() (scope.ScopeDelta, string, string) { return r.delta, r.shape, r.cleaned },
		Compose: func(context.Context, scope.ScopeDelta, []scope.SkippedItem, []scope.CaveatItem) (string, string, string) {
			return r.summary, "", ""
		},
		RequestApproval: func(_ context.Context, ask pipeline.ApprovalAsk) (bool, string, error) {
			r.asks = append(r.asks, ask)
			return r.approved, r.approveAction, r.approveErr
		},
		SetDecide:        func(out scope.MetaagentOutput) { r.setOutput, r.setOutputOK = out, true },
		SetApproved:      func(b bool, action string) { r.setApproved, r.setAction, r.approvedSet = b, action, true },
		ColdStartTimeout: coldStartTimeout,
	}
}

func decideInput(kind string, autoApply bool) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.MetaagentDecide,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "a"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Metaagent: &pipeline.MetaagentInfo{Kind: kind, Requester: "user:alice", Text: "do thing", AutoApply: autoApply, InboxIdx: 0},
	}
}

// TestMetaagentDecide_ColdStart_AsksWithColdStartFlag verifies the cold_start
// ApprovalAsk carries Kind "cold_start" (so the Host stamps coldStart:true →
// 5-button render) and the cleaned task + inboxIdx in the payload.
func TestMetaagentDecide_ColdStart_AsksWithColdStartFlag(t *testing.T) {
	rec := &recordingDecide{
		delta:         scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		shape:         "narrow",
		cleaned:       "summarize L-140",
		approved:      true,
		approveAction: "approve_cleaned",
		summary:       "Deny linear search.",
	}
	h := hooks.NewMetaagentDecide(rec.deps(30 * time.Minute))
	dec := h.Eval(context.Background(), decideInput("cold_start", false))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "Decide returns Allow so Apply always runs (branches on PeekApproved)")
	require.Len(t, rec.asks, 1, "non-autoApply asks for approval")
	ask := rec.asks[0]
	assert.Equal(t, "cold_start", ask.Kind, "cold_start kind drives the 5-button render")
	assert.Equal(t, "summarize L-140", ask.Payload["cleanedTask"])
	assert.Equal(t, true, ask.Payload["coldStart"])
	assert.Equal(t, "user:alice", ask.Payload["requester"])
	assert.Equal(t, 30*time.Minute, ask.Timeout, "cold_start uses the configured approval timeout")
	assert.True(t, rec.approvedSet)
	assert.True(t, rec.setApproved, "approve recorded into scratch")
	assert.Equal(t, "approve_cleaned", rec.setAction, "cold-start 5-way action recorded into scratch")
	assert.True(t, rec.setOutputOK, "composed output recorded into scratch")
}

// TestMetaagentDecide_MidSession_AsksWithoutColdStartFlag verifies the
// mid_session ApprovalAsk carries Kind "metaagent_scope" (3-button render: the
// Host omits coldStart) and the 24h cap timeout.
func TestMetaagentDecide_MidSession_AsksWithoutColdStartFlag(t *testing.T) {
	rec := &recordingDecide{
		delta:    scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}}},
		shape:    "widen",
		approved: false,
		summary:  "Add foo/bar.",
	}
	h := hooks.NewMetaagentDecide(rec.deps(30 * time.Minute))
	dec := h.Eval(context.Background(), decideInput("mid_session", false))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, rec.asks, 1)
	ask := rec.asks[0]
	assert.Equal(t, "metaagent_scope", ask.Kind, "mid_session kind drives the 3-button render")
	_, hasColdStart := ask.Payload["coldStart"]
	assert.False(t, hasColdStart, "mid_session ask must NOT carry coldStart in the hook payload")
	_, hasCleaned := ask.Payload["cleanedTask"]
	assert.False(t, hasCleaned, "mid_session ask must NOT carry cleanedTask")
	assert.Equal(t, hooks.MidSessionApprovalTimeout, ask.Timeout, "mid_session uses the 24h cap")
	assert.True(t, rec.approvedSet)
	assert.False(t, rec.setApproved, "deny recorded into scratch")
}

// TestMetaagentDecide_AutoApply_NoAsk verifies AutoApply applies without an ask
// and records approved=true.
func TestMetaagentDecide_AutoApply_NoAsk(t *testing.T) {
	rec := &recordingDecide{
		delta:   scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		shape:   "narrow",
		cleaned: "summarize L-140",
	}
	h := hooks.NewMetaagentDecide(rec.deps(0))
	dec := h.Eval(context.Background(), decideInput("cold_start", true /*autoApply*/))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, rec.asks, "autoApply must NOT ask for approval")
	assert.True(t, rec.approvedSet)
	assert.True(t, rec.setApproved, "autoApply records approved=true")
}

// TestMetaagentDecide_ApprovalError_DeniesSticky verifies a RequestApproval
// error resolves to a sticky no (approved=false) and Decide still returns Allow
// so Apply runs the deny→sticky conversion.
func TestMetaagentDecide_ApprovalError_DeniesSticky(t *testing.T) {
	rec := &recordingDecide{
		delta:      scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}}},
		shape:      "widen",
		approveErr: errors.New("orch timeout"),
		summary:    "Add foo/bar.",
	}
	h := hooks.NewMetaagentDecide(rec.deps(0))
	dec := h.Eval(context.Background(), decideInput("mid_session", false))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "await error → sticky no, NOT a Halt; Apply still runs")
	assert.True(t, rec.approvedSet)
	assert.False(t, rec.setApproved, "await error maps to a sticky no")
}

// TestMetaagentDecide_ComposesFromSchemaDeltaOnly verifies the composed summary
// is recorded into the scratch output (the approver-summary LLM sees only the
// classified delta — prompt-injection-safe).
func TestMetaagentDecide_ComposesFromSchemaDeltaOnly(t *testing.T) {
	rec := &recordingDecide{
		delta:    scope.ScopeDelta{Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}}},
		shape:    "widen",
		approved: true,
		summary:  "If approved, agent can access foo/bar.",
	}
	h := hooks.NewMetaagentDecide(rec.deps(0))
	_ = h.Eval(context.Background(), decideInput("mid_session", false))
	require.True(t, rec.setOutputOK)
	assert.Equal(t, "If approved, agent can access foo/bar.", rec.setOutput.ApproverSummary)
	require.Len(t, rec.asks, 1)
	assert.Equal(t, "If approved, agent can access foo/bar.", rec.asks[0].Payload["approverSummary"])
}

func TestMetaagentDecide_Points(t *testing.T) {
	h := hooks.NewMetaagentDecide(hooks.MetaagentDecideDeps{})
	assert.Equal(t, []pipeline.Point{pipeline.MetaagentDecide}, h.Points())
	assert.Equal(t, "metaagent_decide", h.Name())
}

var _ pipeline.Hook = hooks.NewMetaagentDecide(hooks.MetaagentDecideDeps{})
