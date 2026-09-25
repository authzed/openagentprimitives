package runner

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func brokenRule() error {
	return &pipeline.DefinitionError{
		Subject: `tool "crm_search_records" on MCP server "crm-tools"`,
		Locus:   "constraints[1]",
		Detail:  `timestamp(f.value) >= now() - duration("744h")`,
		Err:     errors.New("type conversion error from 'string' to 'google.protobuf.Timestamp'"),
	}
}

// The wire status is what carries the distinction across the process boundary.
// A deny caused by a broken rule must not arrive at the browser wearing the
// same status as a deny caused by the viewer's permissions — everything
// downstream reads the status, and nothing downstream can recover the
// difference once it is gone.
func TestAppToolResponseForSeparatesBrokenDefinitionsFromDenials(t *testing.T) {
	cases := []struct {
		name       string
		oc         containedOutcome
		wantStatus string
	}{
		{
			name:       "pre-hook deny from a broken rule: misconfigured",
			oc:         containedOutcome{Phase: containPreDeny, Reason: "mcp trust: validator error", Definition: brokenRule()},
			wantStatus: channelevents.AppToolCallStatusMisconfigured,
		},
		{
			name:       "post-hook deny from a broken rule: misconfigured",
			oc:         containedOutcome{Phase: containPostDeny, Reason: "mcp trust: validator error", Definition: brokenRule()},
			wantStatus: channelevents.AppToolCallStatusMisconfigured,
		},
		{
			name:       "pre-hook deny from a policy refusal: denied",
			oc:         containedOutcome{Phase: containPreDeny, Reason: "permission denied"},
			wantStatus: channelevents.AppToolCallStatusDenied,
		},
		{
			name:       "post-hook deny from a policy refusal: denied",
			oc:         containedOutcome{Phase: containPostDeny, Reason: "leaked a restricted field"},
			wantStatus: channelevents.AppToolCallStatusDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := appToolResponseFor(tool.Result{}, tc.oc)
			assert.Equal(t, tc.wantStatus, resp.Status)
			assert.NotEmpty(t, resp.Message, "the diagnostic text must survive for the log")
			// ViewerMessage stays unset on every deny arm: the runner-side text
			// here is the containment pipeline's own, written for an operator.
			// A browser-facing caller reads this field, finds it empty, and
			// substitutes copy it authored itself.
			assert.Empty(t, resp.ViewerMessage, "runner-side deny text is never viewer copy")
		})
	}
}

// The monitoring report is the half of this that reaches someone who can act.
// It must carry the locus and the authored expression — a report saying only
// "a tool was denied" sends an operator hunting through every rule in the
// spec, which is the situation the report exists to end.
func TestDefinitionReportCarriesTheLocusAndExpression(t *testing.T) {
	summary := definitionSummary("crm_search_records", brokenRule())
	assert.Contains(t, summary, "constraints[1]", "the locus is the whole point of the report")
	assert.Contains(t, summary, "crm_search_records")
	assert.Contains(t, summary, "every call to this tool is refused",
		"an operator must know the tool is unusable, not merely strict")

	hint := definitionHint(brokenRule())
	assert.Contains(t, hint, `timestamp(f.value)`, "the expression is what they search the spec for")
	assert.Contains(t, hint, "type conversion error", "the underlying cause must survive")
}

// A definition fault that is not a CEL constraint — a CEL environment that
// will not build, say — still has to report. It has no position to name, and
// degrading to a vaguer summary is right; going silent is not.
func TestDefinitionReportHandlesAFaultWithNoLocus(t *testing.T) {
	err := &pipeline.DefinitionError{Subject: `tool "x"`, Err: errors.New("cel env: bad extension")}
	assert.Contains(t, definitionSummary("x", err), "cannot be evaluated")
	assert.Contains(t, definitionHint(err), "bad extension")

	// And an error that never went through the pipeline's shape at all still
	// yields its own text rather than an empty hint.
	assert.Contains(t, definitionHint(errors.New("raw failure")), "raw failure")
}

// A broken rule fires on every call that touches its tool. A dashboard with
// several bound sections trips it several times before the viewer has done
// anything, and an operator channel that repeats one permanent defect is a
// channel people mute.
func TestDefinitionFaultIsReportedOncePerDistinctFault(t *testing.T) {
	l := &Loop{}
	first := brokenRule().Error()

	assert.False(t, l.seenDefinitionError(first), "the first sighting must report")
	assert.True(t, l.seenDefinitionError(first), "a repeat of the same fault must not")
	assert.True(t, l.seenDefinitionError(first), "and must keep not reporting")

	// Keyed on the fault, not the tool: one tool can hold several broken
	// rules, and an operator who fixes the first must hear about the second.
	second := (&pipeline.DefinitionError{
		Subject: `tool "crm_search_records" on MCP server "crm-tools"`,
		Locus:   "constraints[2]",
		Err:     errors.New("no such field"),
	}).Error()
	assert.False(t, l.seenDefinitionError(second), "a different fault on the same tool must report")
}

// reportDefinitionError runs on a fail-closed deny path, where the cost of a
// panic is the whole turn. It must tolerate the shapes it can genuinely be
// handed: an outcome that is an ordinary refusal, and a Loop with no bus.
func TestReportDefinitionErrorIsSafeOnTheDenyPath(t *testing.T) {
	l := &Loop{}
	sess := &tool.SessionContext{Namespace: "team", Name: "sess-1"}

	require.NotPanics(t, func() {
		// An ordinary refusal: nothing to report, and nothing published.
		l.reportDefinitionError(sess, "crm_search_records", pipeline.Outcome{Verdict: pipeline.Deny, Reason: "permission denied"})
		// A real fault with no publish handle wired (kubectl-driven sessions,
		// tests): logged, not published, never fatal.
		l.reportDefinitionError(sess, "crm_search_records", pipeline.Outcome{Verdict: pipeline.Deny, Definition: brokenRule()})
		// A nil session, which sessionRefOf exists to absorb.
		l.reportDefinitionError(nil, "crm_search_records", pipeline.Outcome{Verdict: pipeline.Deny, Definition: brokenRule()})
	})
}

// The event that goes on the wire must satisfy the monitoring contract, or
// PublishMonitoring rejects it at the boundary and the report is lost with
// only a log line to say so.
func TestDefinitionMonitoringEventIsValid(t *testing.T) {
	l := &Loop{}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   MonitoringCategoryDefinition,
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     l.definitionSourceRef("team", "sess-1", "crm_search_records"),
		Condition:  "ToolDefinition",
		Reason:     "UnevaluatableRule",
		Summary:    definitionSummary("crm_search_records", brokenRule()),
		Hint:       definitionHint(brokenRule()),
	}
	require.NoError(t, ev.Validate(), "the monitoring boundary must accept this event")

	// With no origin lookup wired the source falls back to the session, which
	// is always resolvable — a report with an unfillable Source would fail
	// Validate and vanish.
	assert.Equal(t, "AgentSession", ev.Source.Kind)
	assert.Equal(t, "sess-1", ev.Source.Name)
}
