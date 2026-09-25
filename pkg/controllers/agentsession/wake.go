// pkg/controllers/agentsession/wake.go
//
// shouldWake detects when a parked AgentSession has received a new wake-up
// request via the wake-requested-at annotation.
package agentsession

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// shouldWake returns true when the AgentSession is parked with its runner pod
// gone (WakeEligible) and carries an unconsumed wake request (WakePending).
//
// Idle: channel-attached sessions park here between conversations; a fresh
// inbound message — or the exiting runner's own post-Idle inbox re-check —
// writes the annotation.
//
// AwaitingRetry: the runner exited on a Provider.Send error; the user clicking
// the Retry button is what writes the annotation. Both transition to Pending
// and respawn the runner via the same path.
//
// Both halves are shared predicates (pkg/apis/v1alpha1) because channelsd and
// the runner must agree with this controller on when a wake is worth writing.
func shouldWake(sess *spiceboxv1alpha1.AgentSession) bool {
	return spiceboxv1alpha1.WakeEligible(sess) && spiceboxv1alpha1.WakePending(sess)
}

// shouldServeUI returns true when the AgentSession is parked with its runner
// pod gone (WakeEligible — the same precondition a conversation wake has) and
// carries an unconsumed UI-serve request.
//
// The pod it leads to is NOT the pod shouldWake leads to. This one runs no
// agent loop: no LLM turn, no phase change, no terminal status. Keeping the
// two predicates separate over separate annotations is what stops a dashboard
// being answered by a conversation — the failure that made opening a page
// spend a turn and, observed live, leave a session Failed.
func shouldServeUI(sess *spiceboxv1alpha1.AgentSession) bool {
	return spiceboxv1alpha1.WakeEligible(sess) && spiceboxv1alpha1.UIServePending(sess)
}
