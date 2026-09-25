//go:build e2e

package threadrun

import (
	"testing"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// planGateDenialStreakThresholdFor enables the forensic-hold tripper
// (pkg/authz/plangate/hold) for the one bundle that exercises it end to end
// (testdata/plangate-denial-streak-hold).
//
// The threshold is an OPERATOR-WIDE flag in production
// (internal/cmd/operator/main.go's --plangate-denial-streak-threshold), never
// a per-AgentClass setting, so bt.Bundle's declarative schema has nowhere to
// carry it — adding a field there would be new exported surface serving
// exactly one scenario. Recognizing the bundle by name mirrors how
// waitForComposedSlotSchema and loadManifests already derive per-bundle
// behavior from fields bt.Bundle DOES carry; this is the one case where no
// such field exists, so the name stands in for it.
func planGateDenialStreakThresholdFor(b bt.Bundle) int {
	if b.Name == "plangate-denial-streak-hold" {
		return 3
	}
	return 0
}

// wantSessionPhaseFor is checkSessionPhase's companion: the AgentSession
// phase a bundle's run must end in, keyed by name for the same reason
// planGateDenialStreakThresholdFor is — bt.Assertions has no
// "session.phase" field (adding one would be exported surface no other
// scenario in the suite would use), and the forensic-hold bundle is the
// first whose point IS the session's own final phase rather than a tool
// result or an audit record.
func wantSessionPhaseFor(b bt.Bundle) string {
	switch b.Name {
	case "plangate-denial-streak-hold":
		return spiceboxv1alpha1.AgentSessionPhaseHeld
	case "guest-start-denied":
		// The deny's whole claim is that the session ENDS without ever
		// running: no runner, no model call, no reply. Failed (reason
		// StartDenied, driven off channelsd's StartFailure signal) is the
		// only observable that separates "denied" from "ran anyway" — and
		// the wait doubles as this replyless bundle's barrier.
		return spiceboxv1alpha1.AgentSessionPhaseFailed
	case credentialHaltBundle:
		// The credential halt's whole claim is that the SESSION ends. Every
		// other observable — the tool result, the audit event — is the same
		// whether the guard halted or merely denied, so the phase is the only
		// place the difference shows.
		return spiceboxv1alpha1.AgentSessionPhaseFailed
	case "completion-requirement-satisfied-by-attachment":
		// Not a claim about the phase — a BARRIER, and the only one this
		// scenario can get. checkTranscriptLength is what actually catches a
		// gate that refuses a round that delivered, and a model-call count
		// read while the round is still running proves nothing in either
		// direction: short of four it fails a correct tree, and landing on
		// four while the fifth call is still in flight passes a broken one.
		// The driver's only wait is ExpectAgentReply, which returns at this
		// transcript's SECOND step of four, so the run is unfinished when the
		// assertions begin — six runs of the healthy tree without this barrier
		// produced one that counted three.
		//
		// The sibling refusal bundle gets its barrier free from
		// checkNoticesPublished, which polls for the bypass notice; this one
		// publishes no notice, by design — an agent that delivered its work is
		// not asked to justify anything. Idle is where a turn that ended
		// normally settles: the runner writes it on the way out, awaiting the
		// next user message.
		return spiceboxv1alpha1.AgentSessionPhaseIdle
	case "agentui-builder-page":
		// This turn's whole point is that the model answers the person by
		// repainting the PAGE (update_view), not by replying in chat — its
		// last authored step is a bare EndTurn, never respond_to_user — so the
		// bundle carries no assert.agentReplyContains and the turn loop's only
		// wait (ExpectAgentReply) never runs. Idle is where a turn that ends
		// normally settles once the runner is done with it, the same barrier
		// completion-requirement-satisfied-by-attachment above uses for the
		// same reason: without it the state checks below would read the run
		// mid-flight, before its last update_view call had even landed.
		return spiceboxv1alpha1.AgentSessionPhaseIdle
	}
	return ""
}

// checkSessionPhase asserts the AgentSession's final status.phase for
// bundles that need it (wantSessionPhaseFor), and is a no-op for every other
// bundle in the suite. Waits rather than checking immediately: the phase
// transition this exists to observe is driven by a watch-triggered reconcile
// (pkg/controllers/agentsession/hold.go), asynchronous with whatever request
// in the transcript triggered it (the plan-gate denial that trips the
// tripper).
func checkSessionPhase(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	want := wantSessionPhaseFor(b)
	if want == "" {
		return
	}
	// The session has to EXIST before its phase can be read, and a bundle whose
	// run ends without an agent reply gets no wait from the turn loop above —
	// SendUserMessage returns as soon as the message is published, and
	// AgentReplyContains is what normally blocks. A scenario whose whole point
	// is that the session STOPS has no reply to wait on, so it would otherwise
	// read the phase before the pipeline had created anything.
	waitForSessionExists(t, h, 30*time.Second)
	ns, name := h.SessionRef()
	h.WaitForSessionPhase(ns, name, want, 30*time.Second)
}
