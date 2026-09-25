package workshop

// testwatch.go enforces Workshop.spec.testWatch: the person's own test of a
// class they built here, started by them from the browser as a root session
// in the workshop namespace. The builder is never blocked in a tool call
// while it waits, so the only way to tell it anything is
// pkg/controllers/inboxwake — append one fixed line to its transcript and
// force it awake. Five events, each recorded in status.testWatch so it is
// delivered once (paused and resumed repeat, once per pause and once per
// return from one): started, paused, resumed, ended, timedOut.
//
// The record in status, not an in-memory table, is what makes this survive an
// operator restart and a builder that went to sleep mid-test — which is
// exactly what a blocking tool call could not do.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/inboxwake"
)

const (
	testEventStarted  = "started"
	testEventPaused   = "paused"
	testEventResumed  = "resumed"
	testEventEnded    = "ended"
	testEventTimedOut = "timedOut"

	// The six lines the builder actually reads. They say what a PERSON did,
	// in the words the builder can act on — no phase names, no CRD nouns, no
	// mention of a watch's plumbing — because they land in the transcript
	// indistinguishable from something the person typed. Each is delivered
	// with the moment it happened appended, "<line> (at <RFC3339 UTC>)", by
	// the deliver closure below; the sentence itself is what stays verbatim.
	// Exported: the test skill
	// (pkg/platform/builderbundle/src/skills/builder-test/SKILL.md) quotes all
	// six verbatim where it tells the builder what it will be told, and
	// skillbody_test.go asserts that against these constants rather than a
	// second hand-copied set of literals.
	TestLineStarted  = "The person started testing the agent."
	TestLinePaused   = "The person paused the test."
	TestLineResumed  = "The person resumed the test."
	TestLineEnded    = "The test ended."
	TestLineStopped  = "The test stopped with an error."
	TestLineTimedOut = "The test watch timed out."
)

// testWatchPollInterval caps how long a LIVE watch waits between passes.
//
// The deadline alone would be enough if every AgentSession event arrived: the
// watch is event-driven, and the deadline requeue exists only to deliver the
// timeout. But an event that is dropped or misrouted — a cache resync gap, a
// mapper that could not read the namespace — would then leave paused/ended
// undelivered for the whole remaining watch, which a person staring at a
// silent builder reads as the feature being broken rather than late. The cap
// bounds that to half a minute. It costs one cached reconcile per 30s per
// workshop with a live watch, and nothing at all once the watch is over.
const testWatchPollInterval = 30 * time.Second

// reconcileTestWatch runs once per reconcile of a Ready workshop. It returns
// the requeue the watch itself needs (testWatchPollInterval, or the time left
// until the deadline when that is nearer; zero once the watch is over) and
// whether it changed status, so the caller can fold both into the single
// status write and result it already composes.
//
// A delivery failure is never returned: inboxwake logs it, and the caller's
// own provisioning work must not be undone because a notification could not
// be delivered. A line that never landed is left unrecorded, so the poll pass
// tries it again. Only a failed read of the workshop namespace's sessions is
// an error — that one leaves the watch unable to say what it observed, so the
// next reconcile must retry it.
func (r *Reconciler) reconcileTestWatch(ctx context.Context, ws *spiceboxv1alpha1.Workshop) (ctrl.Result, bool, error) {
	want := ws.Spec.TestWatch
	if want == nil {
		return ctrl.Result{}, false, nil
	}
	changed := false

	// A watch is identified by (class, startedAt). A different pair is a
	// different test — the person pressed "try it as yourself" again — so the
	// record starts over rather than inheriting the previous test's session
	// and delivered set.
	st := ws.Status.TestWatch
	if st == nil || st.Class != want.Class || !st.StartedAt.Time.Equal(want.StartedAt.Time) {
		st = &spiceboxv1alpha1.WorkshopTestWatchStatus{Class: want.Class, StartedAt: want.StartedAt}
		ws.Status.TestWatch = st
		changed = true
	}

	// A watch that already reported its timeout is LIVE AGAIN when its
	// deadline has moved ahead of now. That is the "Keep watching" case: the
	// person's test is still running, so watch_test kept this watch's identity
	// and extended only its deadline (handleWatchTest,
	// pkg/tools/workshopmcp/tools_trytest.go) — which means the check above
	// does not start the record over, and the timedOut marker would otherwise
	// hold the short-circuit below shut for the rest of the test's life.
	//
	// The moved deadline IS the evidence, and needs nothing recorded: the
	// timeout is delivered only once now has REACHED the deadline, so the only
	// thing that can put the deadline back in the future is a new lease. Only
	// the timedOut marker goes; started/paused history stays, so the builder
	// is never told a second time that the test began.
	if slices.Contains(st.Delivered, testEventTimedOut) && r.now().Before(want.Deadline.Time) {
		st.Delivered = slices.DeleteFunc(st.Delivered, func(e string) bool { return e == testEventTimedOut })
		changed = true
	}

	if slices.Contains(st.Delivered, testEventEnded) || slices.Contains(st.Delivered, testEventTimedOut) {
		// The watch is over. Nothing more is delivered, and the workshop's own
		// requeue (the max-age sweeper's) stands unshortened.
		return ctrl.Result{}, changed, nil
	}

	// deliver attempts one line and reports whether the builder can be
	// considered told. What decides that is whether the LINE landed, not
	// whether the whole notification succeeded. Three outcomes record the
	// event: a delivered line; a line that landed behind a wake that failed
	// (inboxwake.WakeError — the sentence is in the builder's transcript and
	// re-attempting would put it there twice, so it is recorded and the
	// builder reads it whenever anything next wakes it); and a builder that
	// has finished and can never read one (inboxwake.ErrSessionOver —
	// recording that is what stops the watch re-attempting a line whose
	// answer will never change). Only a line that never landed records
	// NOTHING, so the poll pass below tries it again; inboxwake has already
	// logged it with the session it concerned.
	deliver := func(event, line string) bool {
		// The moment rides on the line, because nothing else can carry it: the
		// builder has no clock, and a page it repaints from a line with no time
		// on it can only show the time it last learned — which is how a paused
		// test came to say it paused at the moment the test STARTED. The
		// sentence itself stays verbatim; the moment follows it in parentheses
		// and is what the status line's `since` is set from.
		stamped := fmt.Sprintf("%s (at %s)", line, r.now().UTC().Format(time.RFC3339))
		err := inboxwake.Notify(ctx, r.Client, r.reader(), r.ParentMemory, r.PublishInteraction, r.now,
			ws.Spec.Session.Namespace, ws.Spec.Session.Name, stamped)
		var wakeErr *inboxwake.WakeError
		switch {
		case err == nil, errors.Is(err, inboxwake.ErrSessionOver):
		case errors.As(err, &wakeErr):
			log.FromContext(ctx).Info("test watch: this line reached the builder's transcript but the wake did not fire; it is recorded, since re-attempting would deliver it twice",
				"workshop", ws.Namespace+"/"+ws.Name,
				"session", ws.Spec.Session.Namespace+"/"+ws.Spec.Session.Name,
				"event", event, "err", err.Error())
		default:
			log.FromContext(ctx).Info("test watch: this line did not reach the builder; it stays unrecorded and is tried again",
				"workshop", ws.Namespace+"/"+ws.Name,
				"session", ws.Spec.Session.Namespace+"/"+ws.Spec.Session.Name,
				"event", event, "err", err.Error())
			return false
		}
		st.Delivered = append(st.Delivered, event)
		changed = true
		return true
	}

	sess, err := r.qualifyingTestSession(ctx, ws, want, st)
	if err != nil {
		return ctrl.Result{}, changed, err
	}
	// Whether the builder has been told the test began. Nothing else is
	// delivered ahead of that line, and until a session qualifies no such
	// line is owed at all — a watch that times out having never seen a test
	// start reports the timeout on its own.
	started := true
	if sess != nil {
		// Recording the session is what pins the watch to THIS test, and it is
		// idempotent, so it happens whether or not the line lands. The started
		// event keys off the delivered set instead — a session recorded behind
		// an undelivered line must still get its "started" on a later pass.
		if st.Session == "" {
			st.Session = sess.Name
			changed = true
		}
		// The started line goes first and gates the rest: a phase change told
		// ahead of it would have the builder hearing that the test paused
		// before it ever heard the test began.
		started = slices.Contains(st.Delivered, testEventStarted) || deliver(testEventStarted, TestLineStarted)
		// Only a CHANGE of phase the watch has an EVENT for is something to
		// say. A phase with no case below — Pending, while a woken session's
		// pod comes back — delivers nothing and is not recorded either, so the
		// change it sits between is still a change when it arrives: a person
		// who pauses, resumes and pauses again paused twice.
		if phase := sess.Status.Phase; started && phase != st.LastPhase {
			told := true
			switch phase {
			case spiceboxv1alpha1.AgentSessionPhaseIdle:
				told = deliver(testEventPaused, TestLinePaused)
			case spiceboxv1alpha1.AgentSessionPhaseRunning:
				// Only a PAUSE has something to come back from, and the
				// recorded phase is Idle only once the paused line landed. The
				// Running a test is first seen in is the test starting, which
				// the started line above already said.
				if st.LastPhase == spiceboxv1alpha1.AgentSessionPhaseIdle {
					told = deliver(testEventResumed, TestLineResumed)
				}
			case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
				told = deliver(testEventEnded, TestLineEnded)
			case spiceboxv1alpha1.AgentSessionPhaseFailed:
				// The same terminal event as a finished test — the builder is
				// told once that the test is over — with the line that says
				// which way it went.
				told = deliver(testEventEnded, TestLineStopped)
			default:
				// An intermediate phase is not an event, and recording one hides
				// the event it sits between. A woken session goes Idle →
				// Pending → Running, so a Pending recorded as the observed
				// phase leaves the return from a pause looking like a change
				// from Pending rather than from Idle — the resumed line is then
				// owed forever and never delivered, and the page says "paused"
				// through a live reply. Only the phases named above advance the
				// record.
				told = false
			}
			// The phase advances only when its line landed. Recording it
			// behind a line that did not is the subtle way a notice is lost:
			// the next pass sees no change and never tries again.
			if told {
				st.LastPhase = phase
				changed = true
			}
		}
		if slices.Contains(st.Delivered, testEventEnded) {
			return ctrl.Result{}, changed, nil
		}
	}

	// The deadline is checked last, so a test that ended in the same reconcile
	// the deadline passed is reported as ended, not as timed out. It waits on
	// the started line the same way the phase arm does: the timeout is the
	// event that ENDS the watch, so delivering it ahead of a started line
	// that has not landed yet is how the builder is left never hearing that
	// the test began at all.
	now := r.now()
	if !now.Before(want.Deadline.Time) {
		if started && deliver(testEventTimedOut, TestLineTimedOut) {
			return ctrl.Result{}, changed, nil
		}
		// Undelivered and unrecorded: come back at the poll cap and try it
		// again, since the deadline itself can no longer schedule anything.
		return ctrl.Result{RequeueAfter: testWatchPollInterval}, changed, nil
	}
	return ctrl.Result{RequeueAfter: min(testWatchPollInterval, want.Deadline.Time.Sub(now))}, changed, nil
}

// qualifyingTestSession returns the session this watch is about: the one
// already recorded, or — before one is recorded — the earliest AgentSession in
// the workshop namespace of the watched class, started by the workshop's own
// owner, created at or after the watch began. nil when none qualifies yet.
//
// All three tests matter. The class keeps a session of some other class the
// person also built out of it; the starter keeps a session someone else
// started in the same namespace out of it; the creation time keeps the
// PREVIOUS test out of it, which is the whole reason a replaced watch carries
// a new startedAt.
//
// Both reads go through reader() (uncached when an APIReader is wired, this
// package's idiom), not r.Client: under --watch-namespaces the cached client
// is a multi-namespace cache that answers "unknown namespace" for a workshop
// namespace it was never told to watch, and every test would silently fail to
// qualify.
func (r *Reconciler) qualifyingTestSession(
	ctx context.Context,
	ws *spiceboxv1alpha1.Workshop,
	want *spiceboxv1alpha1.WorkshopTestWatch,
	st *spiceboxv1alpha1.WorkshopTestWatchStatus,
) (*spiceboxv1alpha1.AgentSession, error) {
	// spec.starterCanonical is +optional, and StartedByCanonical returns ""
	// for a session carrying no started-by annotation. An empty starter must
	// never compare equal to an empty/absent canonical — that would make a
	// session NOBODY is attributed with qualify as this person's test, and the
	// builder would be told about a run the person never started. Fail closed,
	// the same way every other site that compares against this field does.
	if ws.Spec.StarterCanonical == "" {
		log.FromContext(ctx).Info("test watch: no starter canonical recorded on the workshop; no session can be attributed to the person, so none qualifies as their test",
			"workshop", ws.Namespace+"/"+ws.Name)
		return nil, nil
	}

	reader := r.reader()
	if st.Session != "" {
		// Once a session is recorded the watch follows THAT session, never a
		// later one — a second session of the same class is not this test.
		var sess spiceboxv1alpha1.AgentSession
		key := types.NamespacedName{Namespace: ws.Status.Namespace, Name: st.Session}
		if err := reader.Get(ctx, key, &sess); err != nil {
			if apierrors.IsNotFound(err) {
				log.FromContext(ctx).Info("test watch: the recorded test session is gone; the watch now only runs to its deadline",
					"workshop", ws.Namespace+"/"+ws.Name, "session", key.String())
				return nil, nil
			}
			return nil, fmt.Errorf("get recorded test session %s: %w", key, err)
		}
		return &sess, nil
	}

	var list spiceboxv1alpha1.AgentSessionList
	if err := reader.List(ctx, &list, client.InNamespace(ws.Status.Namespace)); err != nil {
		return nil, fmt.Errorf("list sessions in workshop namespace %s: %w", ws.Status.Namespace, err)
	}
	var best *spiceboxv1alpha1.AgentSession
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.Class != want.Class {
			continue
		}
		if spiceboxv1alpha1.StartedByCanonical(s).String() != ws.Spec.StarterCanonical {
			continue
		}
		if s.CreationTimestamp.Time.Before(want.StartedAt.Time) {
			continue
		}
		if best == nil || s.CreationTimestamp.Time.Before(best.CreationTimestamp.Time) {
			best = s
		}
	}
	return best, nil
}
