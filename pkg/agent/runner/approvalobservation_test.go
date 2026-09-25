package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fixtureSessNS/fixtureSessName are the session coordinates every fixture in
// this file uses, so a test's SpiceDBLookupSubjects map key
// ("agentsession:<ns>/<name>#approve") and the ask's sess_ns/sess_name always
// agree without each test re-deriving the string.
const (
	fixtureSessNS   = "default"
	fixtureSessName = "demo-sess"
)

// newApprovalObserverFixture builds a runnerHost wired the way
// buildToolCallPending + AwaitDecision need to run for real: a SpiceDB
// approver lookup answering approverSubs for the fixture session's approve
// set, a no-op interaction publisher (so PublishApproval succeeds), and a
// real approval.Orchestrator (so AwaitDecision can block and be delivered
// to). obs is wired as host.approvalEvent; nil is a valid input (the
// "every other surface leaves it nil" case).
func newApprovalObserverFixture(t *testing.T, approverSubs []string, viewer string, obs func(approvalObservation)) *runnerHost {
	t.Helper()
	l := &Loop{
		Approval: approval.New(),
		SpiceDBLookupSubjects: lookupBy(map[string][]string{
			"agentsession:" + fixtureSessNS + "/" + fixtureSessName + "#approve": approverSubs,
		}),
		InteractionRequestPublish: func(context.Context, string, string, channelevents.Envelope) error {
			return nil
		},
	}
	host := newRunnerHost(l, hostSession{Namespace: fixtureSessNS, Name: fixtureSessName})
	host.approvalEvent = obs
	return host
}

// toolCallAskFor builds a "tool_call" pipeline.ApprovalAsk with every field
// buildToolCallPending reads populated, mirroring the shape
// buildToolCallApprovalAsk (pipeline_wiring.go) produces for a proxy-exec
// call: StateImpact=External so toolCallPostApprove's post-approval re-check
// short-circuits (no SpiceDB Engine/Cli needed in this fixture) and the test
// stays focused on the observation seam, not authz re-verification. viewer is
// the BARE canonical subject (no "user:" prefix) — empty for the "no proxy-exec
// viewer" case.
func toolCallAskFor(t *testing.T, viewer string) pipeline.ApprovalAsk {
	t.Helper()
	var subjects []string
	if viewer != "" {
		subjects = []string{viewer}
	}
	payload := ToolCallApprovalPayload{
		SessNS:        fixtureSessNS,
		SessName:      fixtureSessName,
		ToolName:      "do_thing",
		Permission:    "write",
		StateImpact:   string(authz.External),
		Perm:          authz.Permission{StateImpact: authz.External},
		Justification: "because",
		UseID:         "tu-1",
		Subject:       viewer,
		Subjects:      subjects,
	}
	return pipeline.ApprovalAsk{
		Kind:    "tool_call",
		Summary: "do a thing",
		Timeout: time.Minute,
		Payload: payload.ToMap(),
	}
}

// publishAndApprove drives one tool_call approval through PublishApproval +
// AwaitDecision to completion, returning the request ID. When timeout is
// true, AwaitDecision is given a short deadline and never delivered — it
// resolves via context deadline. Otherwise it waits for the orchestrator to
// register the pending await (mirroring host_approval_proxyexec_test.go's
// awaitDecisionProbe pattern) and delivers approve/deny explicitly.
func publishAndApprove(t *testing.T, h *runnerHost, approve, timeout bool) string {
	t.Helper()
	reqID, err := h.PublishApproval(context.Background(), toolCallAskFor(t, "viewer-1"))
	require.NoError(t, err, "PublishApproval must succeed for this fixture")

	awaitTimeout := time.Minute
	if timeout {
		awaitTimeout = 20 * time.Millisecond
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, awaitErr := h.AwaitDecision(context.Background(), reqID, awaitTimeout)
		assert.NoError(t, awaitErr, "AwaitDecision must not error for this fixture")
	}()

	if !timeout {
		sessionRef := h.sess.Namespace + "/" + h.sess.Name
		deadline := time.Now().Add(2 * time.Second)
		for h.l.Approval.PendingForSession(sessionRef) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for AwaitDecision to register the pending approval")
			}
			time.Sleep(time.Millisecond)
		}
		h.l.Approval.DeliverDecision(reqID, approval.Decision{Approved: approve, ApproverID: "tester"})
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for AwaitDecision to return")
	}
	return reqID
}

// lastResolved returns the last Resolved observation in got, failing the
// test if none was recorded.
func lastResolved(t *testing.T, got []approvalObservation) approvalObservation {
	t.Helper()
	for i := len(got) - 1; i >= 0; i-- {
		if got[i].Resolved {
			return got[i]
		}
	}
	t.Fatal("no Resolved observation was recorded")
	return approvalObservation{}
}

func TestApprovalObservationAddressedToViewer(t *testing.T) {
	cases := []struct {
		name          string
		approverSubs  []string // what resolveInteractionApprovers returns
		viewer        string   // the proxy-exec requester, BARE canonical form
		wantAddressed bool
	}{
		{
			name:          "the viewer is among the approvers -> addressed to them",
			approverSubs:  []string{"user:viewer-1", "user:owner-9"},
			viewer:        "viewer-1",
			wantAddressed: true,
		},
		{
			name:          "only someone else can approve -> not addressed to the viewer",
			approverSubs:  []string{"user:owner-9"},
			viewer:        "viewer-1",
			wantAddressed: false,
		},
		{
			name: "the comparison normalizes the SpiceDB prefix on BOTH sides",
			// The approver list carries identity.Subject ("user:<canonical>");
			// the ask payload's "subject" carries the BARE canonical form that
			// handleAppToolCallReq derived with strings.TrimPrefix. Comparing
			// them raw always yields false, which reads in production as
			// "chrome never auto-reveals" with nothing failing anywhere.
			approverSubs:  []string{"user:viewer-1"},
			viewer:        "viewer-1",
			wantAddressed: true,
		},
		{
			name:          "an empty viewer (a non-proxy-exec LLM call) is never addressed",
			approverSubs:  []string{"user:owner-9"},
			viewer:        "",
			wantAddressed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []approvalObservation
			h := newApprovalObserverFixture(t, tc.approverSubs, tc.viewer,
				func(ev approvalObservation) { got = append(got, ev) })

			_, err := h.PublishApproval(t.Context(), toolCallAskFor(t, tc.viewer))
			require.NoError(t, err, "PublishApproval must succeed for this fixture")

			require.Len(t, got, 1, "publishing an ask must produce exactly one observation")
			assert.True(t, got[0].Asked)
			assert.False(t, got[0].Resolved)
			assert.Equal(t, tc.wantAddressed, got[0].AddressedToViewer)
		})
	}
}

func TestApprovalObservationResolution(t *testing.T) {
	t.Run("a granted approval observes Resolved+Approved, which is when running begins", func(t *testing.T) {
		var got []approvalObservation
		h := newApprovalObserverFixture(t, []string{"user:owner-9"}, "viewer-1",
			func(ev approvalObservation) { got = append(got, ev) })
		reqID := publishAndApprove(t, h, true /* approve */, false /* timeout */)
		require.NotEmpty(t, reqID)

		resolved := lastResolved(t, got)
		assert.True(t, resolved.Approved)
		assert.False(t, resolved.TimedOut)
	})

	t.Run("a lapsed deadline observes TimedOut, which is what makes expired distinguishable from denied", func(t *testing.T) {
		var got []approvalObservation
		h := newApprovalObserverFixture(t, []string{"user:owner-9"}, "viewer-1",
			func(ev approvalObservation) { got = append(got, ev) })
		publishAndApprove(t, h, false, true /* timeout */)

		resolved := lastResolved(t, got)
		assert.False(t, resolved.Approved)
		assert.True(t, resolved.TimedOut,
			"without this, uiaction.StateFor can never return expired and the control never re-enables")
	})

	t.Run("a nil observer is a no-op, not a panic — every other surface leaves it nil", func(t *testing.T) {
		h := newApprovalObserverFixture(t, []string{"user:owner-9"}, "viewer-1", nil)
		_, err := h.PublishApproval(t.Context(), toolCallAskFor(t, "viewer-1"))
		assert.NoError(t, err)
	})
}
