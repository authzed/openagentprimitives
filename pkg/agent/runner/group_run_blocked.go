package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// expiredApproval names an approval a group run raised that reached its
// deadline with no decision.
type expiredApproval struct {
	RequestID string
	Kind      string
}

// approvalKindLabels names each host-driven approval kind as the run's result
// summary describes it.
var approvalKindLabels = map[string]string{
	"tool_call":                   "tool permission",
	"leakage_share":               "information-sharing",
	"content_inspection":          "content inspection",
	"plan_phase":                  "plan phase",
	"plan_amendment":              "plan amendment",
	categories.PreconditionWaiver: "precondition waiver",
	"preference_save":             "preference save",
}

// noteGroupRunApprovalExpired records that an approval this group run raised
// expired undecided. Only the first is kept: one is enough to end the run. A
// session that is not a group goal run keeps today's behavior (the deny is
// handed to the model, which carries on), so this is a no-op there. Expiry
// wins over a decision that arrives after the runner stopped waiting: the
// click lands on a request already resolved as expired and changes nothing.
// An earlier result the model itself reported wins over the runner's blocked
// proposal: the goal server keeps the first proposal, refuses the conflicting
// one, and concludeGroupRunBlocked logs that refusal and concludes anyway.
func (l *Loop) noteGroupRunApprovalExpired(requestID, kind string) {
	if !l.GroupGoalRun {
		return
	}
	l.groupRunExpiredMu.Lock()
	defer l.groupRunExpiredMu.Unlock()
	if l.groupRunExpired == nil {
		l.groupRunExpired = &expiredApproval{RequestID: requestID, Kind: kind}
	}
}

func (l *Loop) takeGroupRunApprovalExpired() (expiredApproval, bool) {
	l.groupRunExpiredMu.Lock()
	defer l.groupRunExpiredMu.Unlock()
	if l.groupRunExpired == nil {
		return expiredApproval{}, false
	}
	e := *l.groupRunExpired
	l.groupRunExpired = nil
	return e, true
}

// groupRunBlockedProposal is the result a group run reports when an approval
// it raised expired: blocked, naming the unanswered approval. Its RequestID is
// derived from the approval so a retried report is the identical proposal.
func groupRunBlockedProposal(e expiredApproval, now time.Time) goals.RunProposal {
	label, ok := approvalKindLabels[e.Kind]
	if !ok {
		label = e.Kind
	}
	id := "approval-expired-" + e.RequestID
	if len(id) > 128 {
		id = id[:128]
	}
	return goals.RunProposal{
		RequestID: id,
		Status:    "blocked",
		Summary: fmt.Sprintf("Blocked: the %s approval this run asked for (request %s) received no decision before its deadline, so the run stopped.",
			label, e.RequestID),
		Evidence:    []string{"approval request " + e.RequestID},
		SubmittedAt: now.UTC(),
	}
}

// concludeGroupRunBlocked ends a group goal run whose approval expired
// undecided: it reports the run's result as blocked, then concludes the
// session through the same Idle park agent_work_complete takes for a
// channel-attached session. The goal dispatcher reads that Idle plus the
// reported result as the run having ended (exitReason), so the occurrence
// carries the blocked report.
//
// A failed report is logged and the session still concludes: the run then
// ends with no reported result, which the dispatcher records as a failed run
// rather than leaving it waiting out its duration bound.
func (l *Loop) concludeGroupRunBlocked(ctx context.Context, e expiredApproval) error {
	session := l.SessionKey.Namespace + "/" + l.SessionKey.Name
	proposal := groupRunBlockedProposal(e, time.Now())
	switch {
	case l.GroupRunReport == nil:
		slog.Default().Info("group goal run: approval expired undecided but no result reporter is wired; concluding without a report",
			"session", session, "requestID", e.RequestID, "kind", e.Kind)
	default:
		if err := l.GroupRunReport(ctx, proposal); err != nil {
			slog.Default().Info("group goal run: reporting blocked after an expired approval failed; concluding anyway",
				"session", session, "requestID", e.RequestID, "kind", e.Kind, "err", err.Error())
		}
	}
	slog.Default().Info("group goal run: approval expired undecided; run concluded as blocked",
		"session", session, "requestID", e.RequestID, "kind", e.Kind)
	l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
	l.emitLifecycleEvent(ctx, lifecyclecore.AgentWorkComplete{Kubectl: false})
	l.fireSessionEnd(ctx, "idle")
	return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete)
}
