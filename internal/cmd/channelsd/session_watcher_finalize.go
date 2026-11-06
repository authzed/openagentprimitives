package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

// terminalFinalizeWindow bounds which terminal sessions finalizeTerminal will
// touch: only those that ended this recently. Without it a channelsd restart
// would issue a clear for every Succeeded/Failed session the cluster retains
// (hundreds, for weeks), all for indicators long since gone.
const terminalFinalizeWindow = time.Hour

// terminalFinalizeMaxAttempts bounds how many ticks finalizeTerminal retries a
// failing cleanup. A failure that persists (the channel was archived, the bot
// removed: channel_not_found) would otherwise be retried every 5s for the whole
// terminalFinalizeWindow — ~720 Slack calls per session, feeding the rate limit
// live sessions share. Five covers a transient ratelimited or network error.
const terminalFinalizeMaxAttempts = 5

// terminalFinalizeState is finalizeTerminal's per-session progress: whether
// the indicator clear already went out (so a retry does not re-send it),
// how many attempts failed, and whether the session is done with — finalized,
// or given up on.
type terminalFinalizeState struct {
	cleared  bool
	failures int
	done     bool
}

// capacityHoldNoticeAnnotation records the CapacityHold LastTransitionTime of
// the hold episode whose "queued" notice was already posted, so a channelsd
// restart mid-hold does not post it again. A new episode (a later hold, with a
// later transition time) gets its own notice.
const capacityHoldNoticeAnnotation = "agentprimitives.authzed.com/capacity-hold-notice"

// finalizeTerminal tears down every "still working" indicator a session left in
// its channel when it reached a terminal phase, once per session.
//
// The runner closes these out itself on a normal exit. A session the OPERATOR
// ended — failed at boot (a capacity timeout, a bundle that never came up), a
// crashed or evicted runner, an expiry — never runs that code, and before this
// the watcher only dropped its own tracking (Forget), sending nothing: the
// "⏳ Waiting for capacity" / "is starting…" / "⚠️ Taking longer" caption, and
// any plan card's in-progress hourglass, stayed up on a dead session.
//
// Two steps, both through the resolved Sender:
//   - the watchdog's Clear: the empty-Notification sentinel every kind reads as
//     "dismiss my indicator", plus the stall-streak reset; and
//   - the kind's optional channelkinds.SessionFinalizer, for indicators only the
//     kind can reach (Slack's plan cards).
//
// A failure of either is logged and NOT memoized, so the next tick retries —
// the same discipline maybeClearIdle uses — up to terminalFinalizeMaxAttempts,
// after which it gives up (logged). A clear that already went out is not
// re-sent on a retry. The capacity-notice marker is
// dropped as well: its own clear branch only runs while Pending/Running, so a
// session that fails mid-episode would otherwise strand it.
func (w *sessionWatcher) finalizeTerminal(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	uid := string(sess.UID)
	w.mu.Lock()
	st := w.terminalFinalized[uid]
	delete(w.schedNoticePosted, uid)
	w.mu.Unlock()
	if st.done {
		return
	}
	if fin := sess.Status.FinishedAt; fin != nil && time.Since(fin.Time) > terminalFinalizeWindow {
		w.setTerminalFinalized(uid, terminalFinalizeState{done: true})
		return
	}
	key := sess.Namespace + "/" + sess.Name
	// fail records a failed attempt; past the budget the session is given up
	// on, with the indicator possibly still up — logged, so it is findable.
	fail := func(what string, err error) {
		st.failures++
		if st.failures >= terminalFinalizeMaxAttempts {
			st.done = true
			logger.Info("terminal cleanup: giving up; the session's working indicator may stay up",
				"session", key, "phase", sess.Status.Phase, "step", what,
				"attempts", st.failures, "err", err.Error())
		} else {
			logger.Info("terminal cleanup failed; retrying next tick",
				"session", key, "phase", sess.Status.Phase, "step", what,
				"attempt", st.failures, "err", err.Error())
		}
		w.setTerminalFinalized(uid, st)
	}
	if !st.cleared {
		if err := w.wd.Clear(ctx, sess.Namespace, sess.Name); err != nil {
			fail("clear status indicator", err)
			return
		}
		st.cleared = true
	}
	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		fail("resolve sender", err)
		return
	}
	if fin, ok := sender.(channelkinds.SessionFinalizer); ok {
		if err := fin.FinalizeSession(ctx, channelkinds.SessionInfo{
			Namespace:   sess.Namespace,
			Name:        sess.Name,
			Channel:     spiceboxv1alpha1.OutboundBinding(sess),
			Annotations: sess.Annotations,
		}, sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed); err != nil {
			fail("finalize session indicators", err)
			return
		}
	}
	st.done = true
	w.setTerminalFinalized(uid, st)
}

func (w *sessionWatcher) setTerminalFinalized(uid string, st terminalFinalizeState) {
	w.mu.Lock()
	w.terminalFinalized[uid] = st
	w.mu.Unlock()
}

// resetTerminalFinalized forgets uid's finalize progress: the session is live
// again, so its next ending must be finalized afresh.
func (w *sessionWatcher) resetTerminalFinalized(uid string) {
	w.mu.Lock()
	delete(w.terminalFinalized, uid)
	w.mu.Unlock()
}

// maybePostCapacityHoldNotice posts the "queued for cluster capacity" notice
// once per hold episode while the operator holds sess for capacity
// (CapacityHold=True; see pkg/controllers/agentsession/capacity_hold.go).
//
// It is a durable in-thread notice rather than a status caption on purpose. A
// hold can last an hour, which outlives any transient caption; and a
// triggered session (a reviewbot review is github-in / slack-out) has no
// thread to hang a caption on until something is RELAYED, whereas a notice on
// the publish path goes through the outbound relay, which opens the thread
// first. That is what makes a held review visible at all instead of the
// channel simply staying quiet for an hour.
func (w *sessionWatcher) maybePostCapacityHoldNotice(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	text, held := sessionnotice.CapacityHold(sess)
	if !held || w.publish == nil {
		return
	}
	cond := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCapacityHold)
	if cond == nil {
		return
	}
	episode := cond.LastTransitionTime.UTC().Format(time.RFC3339)
	uid := string(sess.UID)
	w.mu.Lock()
	seen := w.holdNoticeReported[uid] == episode
	w.mu.Unlock()
	if seen {
		return
	}
	if sess.Annotations[capacityHoldNoticeAnnotation] == episode {
		w.mu.Lock()
		w.holdNoticeReported[uid] = episode
		w.mu.Unlock()
		return
	}
	key := sess.Namespace + "/" + sess.Name
	n := notice.New(categories.SessionQueued, notice.Args{
		Lead:     "Queued for cluster capacity",
		Body:     text,
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
	ref := fmt.Sprintf("notice-%s-%s-%d", categories.SessionQueued, uid, cond.LastTransitionTime.Unix())
	if err := w.publishNotice(sess, ref, n); err != nil {
		logger.Error(err, "capacity-hold notice: publish", "session", key)
		return
	}
	w.mu.Lock()
	w.holdNoticeReported[uid] = episode
	w.mu.Unlock()
	if err := w.stampAnnotation(ctx, sess, capacityHoldNoticeAnnotation, episode); err != nil {
		logger.Error(err, "capacity-hold notice: persist reported annotation", "session", key)
	}
}
