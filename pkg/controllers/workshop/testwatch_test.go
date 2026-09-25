package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// watchFixture is one Ready workshop plus everything a test needs to drive
// spec.testWatch: the clock the Reconciler reads, the memory facade
// inboxwake.Notify appends the builder's lines to, and the nudges it
// published. A struct rather than the multi-value return the rest of this
// package uses, because six correlated values read better named than
// positional.
type watchFixture struct {
	c     client.Client
	r     *workshop.Reconciler
	ws    *spiceboxv1alpha1.Workshop
	sess  *spiceboxv1alpha1.AgentSession
	mem   memory.Memory
	nudge []string // "<ns>/<name>" per published wake
	now   time.Time
}

// readyWorkshop provisions one workshop all the way to Ready, so every test
// below starts where the test watch actually runs: on the steady-state
// reconcile of an already-provisioned workshop.
func readyWorkshop(t *testing.T) *watchFixture {
	t.Helper()
	sess := builderSession("builder-1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	ws := sanctionedWorkshop(sess)
	c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws)

	f := &watchFixture{c: c, r: r, ws: ws, sess: sess, now: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
	// Declared as the interface and assigned once from a real value —
	// CLAUDE.md's typed-nil rule, the same shape the operator wiring uses.
	var mem memory.Memory = memory.NewLocal(inmem.NewBackend())
	f.mem = mem
	r.ParentMemory = mem
	r.PublishInteraction = func(_ context.Context, ns, name string, _ channelevents.Envelope) error {
		f.nudge = append(f.nudge, ns+"/"+name)
		return nil
	}
	r.Now = func() time.Time { return f.now }

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "provisioning the workshop must succeed before any test watch runs")
	require.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, getWorkshop(t, c, ws).Status.Phase)

	// The builder is live and working: Running, with nothing awaited, which is
	// where it spends most of a test it is watching. Every case below
	// therefore delivers its lines to a builder in the middle of its own turn,
	// which is the state the whole "inbox" role exists to make safe.
	f.setBuilderMidTurn(t)
	return f
}

// setBuilderMidTurn puts the builder in the middle of its own turn — Running
// with nothing awaited — so its runner is holding a transcript index it is
// about to write at.
func (f *watchFixture) setBuilderMidTurn(t *testing.T) {
	t.Helper()
	f.setBuilderStatus(t, spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning})
}

// holdReleaseForInstall records an install request still waiting on an admin,
// which is what keeps a workshop whose builder session has finished from being
// released (releaseIfFinished, close.go) before the watch ever runs.
func (f *watchFixture) holdReleaseForInstall(t *testing.T) {
	t.Helper()
	cur := getWorkshop(t, f.c, f.ws)
	cur.Status.Install = &spiceboxv1alpha1.WorkshopInstallStatus{Phase: spiceboxv1alpha1.WorkshopInstallPhaseRequested}
	require.NoError(t, f.c.Status().Update(context.Background(), cur), "recording an open install request")
}

func (f *watchFixture) setBuilderStatus(t *testing.T, status spiceboxv1alpha1.AgentSessionStatus) {
	t.Helper()
	var cur spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(context.Background(),
		types.NamespacedName{Namespace: f.sess.Namespace, Name: f.sess.Name}, &cur), "get the builder session")
	cur.Status = status
	require.NoError(t, f.c.Update(context.Background(), &cur), "setting the builder session status")
}

// namespace is the provisioned workshop namespace — where a person's own test
// session lives.
func (f *watchFixture) namespace() string {
	return spiceboxv1alpha1.WorkshopNamespaceName(f.sess.UID)
}

// lines returns every line delivered to the BUILDER session's transcript so
// far, in order. This is the only channel the builder has while it is not
// blocked in a tool call, so it is what each case asserts on.
func (f *watchFixture) lines(t *testing.T) []string {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	turns, err := turn.ReadAll(ctx, f.mem, memory.Scope{Kind: "session", ID: f.sess.Namespace + "/" + f.sess.Name})
	require.NoError(t, err, "reading the builder transcript")
	var out []string
	for _, tn := range turns {
		require.NotEmpty(t, tn.Content, "a delivered turn must carry content")
		out = append(out, tn.Content[0].Text)
	}
	return out
}

// setWatch writes spec.testWatch, exactly as the sidecar's tool would.
func (f *watchFixture) setWatch(t *testing.T, class string, startedAt, deadline time.Time) {
	t.Helper()
	cur := getWorkshop(t, f.c, f.ws)
	cur.Spec.TestWatch = &spiceboxv1alpha1.WorkshopTestWatch{
		Class:     class,
		StartedAt: metav1.NewTime(startedAt),
		Deadline:  metav1.NewTime(deadline),
	}
	require.NoError(t, f.c.Update(context.Background(), cur), "writing spec.testWatch")
}

func (f *watchFixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := reconcileWorkshop(t, f.r, f.ws)
	require.NoError(t, err, "a steady-state reconcile of a Ready workshop must not error")
	return res
}

func (f *watchFixture) status(t *testing.T) *spiceboxv1alpha1.WorkshopTestWatchStatus {
	t.Helper()
	return getWorkshop(t, f.c, f.ws).Status.TestWatch
}

// testSession creates one AgentSession in the workshop namespace, as the
// browser would when a person presses "try it as yourself". An empty starter
// means the session carries no started-by annotation at all — the
// unattributed session a kubectl-driven create produces.
func (f *watchFixture) testSession(t *testing.T, name, class, starter string, created time.Time, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	annotations := map[string]string{}
	if starter != "" {
		annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = "user:" + starter
	}
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         f.namespace(),
			CreationTimestamp: metav1.NewTime(created),
			Annotations:       annotations,
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: class},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
	require.NoError(t, f.c.Create(context.Background(), s), "creating the test session")
	var back spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(context.Background(), types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &back))
	require.False(t, back.CreationTimestamp.IsZero(), "the fixture depends on the test session keeping its creation timestamp")
	return &back
}

func (f *watchFixture) setPhase(t *testing.T, s *spiceboxv1alpha1.AgentSession, phase string) {
	t.Helper()
	var cur spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(context.Background(), types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &cur))
	cur.Status.Phase = phase
	require.NoError(t, f.c.Update(context.Background(), &cur), "setting the test session phase")
}

// workshopSweepRequeue is the requeue a Ready workshop asks for on its own
// account (the max-age sweeper's hourly floor). A watch that still has a
// deadline ahead shortens it; a finished watch leaves it alone, which is what
// "no requeue from the watch" means at this call site.
const workshopSweepRequeue = time.Hour

// at is one delivered line as the builder reads it: the verbatim sentence
// followed by the moment it happened. The builder has no clock of its own —
// nothing it can read says what time anything was — so the moment on the line
// is the only thing that can reach the page's status line, and every
// expectation below is written against the reconciler's own clock at the pass
// that delivered the line.
func at(ts time.Time, line string) string {
	return line + " (at " + ts.UTC().Format(time.RFC3339) + ")"
}

// A live watch polls at its cap rather than sleeping to the deadline, so a
// dropped AgentSession event costs at most half a minute; when the deadline is
// nearer than the cap, the deadline wins so the timeout is not delivered late.
func TestTestWatch_NoSessionYet_RequeuesAtThePollCap(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))

	res := f.reconcile(t)

	assert.Equal(t, 30*time.Second, res.RequeueAfter,
		"a far deadline must not stop the watch from polling")
	assert.Empty(t, f.lines(t), "nothing has happened yet, so the builder is told nothing")
	st := f.status(t)
	require.NotNil(t, st, "the watch must be recorded even before a session qualifies")
	assert.Equal(t, "demo-agent", st.Class)
	assert.True(t, st.StartedAt.Time.Equal(f.now), "status echoes the spec's startedAt")
	assert.Empty(t, st.Session)
	assert.Empty(t, st.Delivered)

	f.setWatch(t, "demo-agent", f.now, f.now.Add(10*time.Second))
	assert.Equal(t, 10*time.Second, f.reconcile(t).RequeueAfter,
		"a deadline nearer than the cap wins, so the timeout lands on time")
}

func TestTestWatch_StartedThenPausedThenEnded_DeliversEachOnce(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	f.reconcile(t)
	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t))
	st := f.status(t)
	require.NotNil(t, st)
	assert.Equal(t, "try-1", st.Session)
	assert.Equal(t, []string{"started"}, st.Delivered)
	assert.Equal(t, []string{"default/builder-1"}, f.nudge, "the builder is woken, not the test session")

	// Nothing changed: the builder must not be told the same thing twice.
	f.reconcile(t)
	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t))

	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseIdle)
	f.reconcile(t)
	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The person paused the test."),
	}, f.lines(t))

	// Coming back is its own event, and a second pause is a second thing that
	// happened, so both are delivered again.
	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseIdle)
	f.reconcile(t)
	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The person paused the test."),
		at(f.now, "The person resumed the test."),
		at(f.now, "The person paused the test."),
	}, f.lines(t))

	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	res := f.reconcile(t)
	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The person paused the test."),
		at(f.now, "The person resumed the test."),
		at(f.now, "The person paused the test."),
		at(f.now, "The test ended."),
	}, f.lines(t))
	st = f.status(t)
	require.NotNil(t, st)
	require.NotEmpty(t, st.Delivered)
	assert.Equal(t, "ended", st.Delivered[len(st.Delivered)-1])
	assert.Equal(t, workshopSweepRequeue, res.RequeueAfter, "a finished watch stops shortening the workshop's own requeue")

	// The watch is over: a later reconcile delivers nothing more.
	f.reconcile(t)
	assert.Len(t, f.lines(t), 5)
}

func TestTestWatch_FailedSessionDeliversStoppedLine(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)

	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseFailed)
	f.reconcile(t)

	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The test stopped with an error."),
	}, f.lines(t))
	st := f.status(t)
	require.NotNil(t, st)
	assert.Equal(t, []string{"started", "ended"}, st.Delivered,
		"a stopped test is the same terminal event as a finished one; only the line differs")
}

func TestTestWatch_IgnoresSessionsThatDoNotQualify(t *testing.T) {
	t.Run("wrong class, another person's session, or one predating the watch: no lines", func(t *testing.T) {
		f := readyWorkshop(t)
		f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
		f.testSession(t, "other-class", "another-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.testSession(t, "other-starter", "demo-agent", "someone-else", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.testSession(t, "too-early", "demo-agent", "c4nonical", f.now.Add(-time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

		f.reconcile(t)

		assert.Empty(t, f.lines(t), "none of these is the person's own test of the watched class")
		st := f.status(t)
		require.NotNil(t, st)
		assert.Empty(t, st.Session)
		assert.Empty(t, st.Delivered)
	})

	// spec.starterCanonical is +optional and StartedByCanonical returns "" for
	// an unattributed session, so both sides can be empty. Comparing them
	// equal would hand the builder a session nobody started.
	t.Run("no recorded starter and an unattributed session: fail closed, no lines", func(t *testing.T) {
		f := readyWorkshop(t)
		cur := getWorkshop(t, f.c, f.ws)
		cur.Spec.StarterCanonical = ""
		require.NoError(t, f.c.Update(context.Background(), cur), "clearing spec.starterCanonical")

		f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
		f.testSession(t, "unattributed", "demo-agent", "", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

		f.reconcile(t)

		assert.Empty(t, f.lines(t), "a session nobody is attributed with is not the person's test")
		st := f.status(t)
		require.NotNil(t, st)
		assert.Empty(t, st.Session)
		assert.Empty(t, st.Delivered)
	})
}

func TestTestWatch_TimesOutWithNoSession(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now.Add(-time.Hour), f.now.Add(-time.Minute))

	res := f.reconcile(t)

	assert.Equal(t, []string{at(f.now, "The test watch timed out.")}, f.lines(t))
	st := f.status(t)
	require.NotNil(t, st)
	assert.Equal(t, []string{"timedOut"}, st.Delivered)
	assert.Equal(t, workshopSweepRequeue, res.RequeueAfter, "a timed-out watch stops shortening the workshop's own requeue")

	f.reconcile(t)
	assert.Len(t, f.lines(t), 1, "the timeout is delivered exactly once")
}

func TestTestWatch_TimesOutWhileRunning(t *testing.T) {
	f := readyWorkshop(t)
	began := f.now
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	require.Equal(t, []string{at(began, "The person started testing the agent.")}, f.lines(t))

	f.now = f.now.Add(31 * time.Minute)
	f.reconcile(t)

	assert.Equal(t, []string{
		at(began, "The person started testing the agent."),
		at(f.now, "The test watch timed out."),
	}, f.lines(t))
	st := f.status(t)
	require.NotNil(t, st)
	assert.Equal(t, []string{"started", "timedOut"}, st.Delivered)
}

func TestTestWatch_ReplacedWatchStartsFresh(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	require.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t))

	// A second "try it as yourself" replaces the watch; the first test's
	// session predates it and so is not the new test.
	later := f.now.Add(10 * time.Minute)
	f.setWatch(t, "demo-agent", later, later.Add(30*time.Minute))
	f.reconcile(t)

	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t), "the replaced watch delivers nothing on its own")
	st := f.status(t)
	require.NotNil(t, st)
	assert.True(t, st.StartedAt.Time.Equal(later), "status tracks the new watch")
	assert.Empty(t, st.Session, "the previous test is not this watch's session")
	assert.Empty(t, st.Delivered, "a replaced watch has delivered nothing yet")
}

// TestTestWatch_ReArmedWatchOnAStillRunningTestStillDeliversEnded is the
// controller half of "Keep watching". The tool keeps (class, startedAt) while
// the recorded test still runs and moves only the deadline, so the identity
// check above does NOT start the record over — which leaves "timedOut" in the
// delivered set and the short-circuit holding the watch shut forever. A
// deadline back in the FUTURE is the evidence of a new lease: the timeout is
// only ever delivered once now has reached the deadline, so nothing but a
// re-arm can put it there.
func TestTestWatch_ReArmedWatchOnAStillRunningTestStillDeliversEnded(t *testing.T) {
	f := readyWorkshop(t)
	began := f.now
	f.setWatch(t, "demo-agent", began, began.Add(30*time.Minute))
	s := f.testSession(t, "try-1", "demo-agent", "c4nonical", began.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	require.Equal(t, []string{at(began, "The person started testing the agent.")}, f.lines(t))

	// The watch runs out while the person is still testing.
	f.now = began.Add(31 * time.Minute)
	f.reconcile(t)
	require.Equal(t, []string{
		at(began, "The person started testing the agent."),
		at(f.now, "The test watch timed out."),
	}, f.lines(t))
	require.Equal(t, []string{"started", "timedOut"}, f.status(t).Delivered)

	// "Keep watching": the same watch, a deadline further out.
	f.setWatch(t, "demo-agent", began, f.now.Add(30*time.Minute))
	res := f.reconcile(t)

	st := f.status(t)
	require.NotNil(t, st)
	assert.True(t, st.StartedAt.Time.Equal(began), "the re-armed watch is the same watch, not a replacement")
	assert.Equal(t, []string{"started"}, st.Delivered,
		"the timeout marker is dropped so the watch can speak again; the started history stays, so the builder is not told the test began twice")
	assert.Equal(t, "try-1", st.Session, "and it is still watching the same test")
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "a live watch polls again")
	assert.Len(t, f.lines(t), 2, "re-arming says nothing to the builder on its own")

	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	f.reconcile(t)

	assert.Equal(t, []string{
		at(began, "The person started testing the agent."),
		at(f.now, "The test watch timed out."),
		at(f.now, "The test ended."),
	}, f.lines(t), "the test the person kept running is reported when it ends")
	assert.Equal(t, []string{"started", "ended"}, f.status(t).Delivered)
}

// A re-armed watch that runs out AGAIN must say so again. The drop of the
// timedOut marker is what lets it, and it is conditioned on the deadline
// being back in the future — so this case pins both halves at once. Drop the
// marker unconditionally and the second timeout is re-delivered on every
// later pass; never drop it and the person who pressed "Keep watching" is
// told nothing when the extra time runs out either, which reads as the
// builder having quietly stopped watching.
func TestTestWatch_ReArmedWatchTimesOutAgainAtItsNewDeadlineExactlyOnce(t *testing.T) {
	f := readyWorkshop(t)
	began := f.now
	f.setWatch(t, "demo-agent", began, began.Add(30*time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", began.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	require.Equal(t, []string{at(began, "The person started testing the agent.")}, f.lines(t))

	// The first lease runs out while the person is still testing.
	f.now = began.Add(31 * time.Minute)
	firstTimeout := f.now
	f.reconcile(t)
	require.Equal(t, []string{
		at(began, "The person started testing the agent."),
		at(firstTimeout, "The test watch timed out."),
	}, f.lines(t))
	require.Equal(t, []string{"started", "timedOut"}, f.status(t).Delivered)

	// "Keep watching": the same watch, with a second lease.
	secondDeadline := firstTimeout.Add(30 * time.Minute)
	f.setWatch(t, "demo-agent", began, secondDeadline)
	res := f.reconcile(t)
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "the watch is live again, so it polls again")
	assert.Len(t, f.lines(t), 2, "re-arming says nothing on its own")

	// Most of the way through the second lease: still nothing, because the
	// timeout belongs to the deadline, not to the marker that was dropped.
	f.now = firstTimeout.Add(29 * time.Minute)
	f.reconcile(t)
	assert.Len(t, f.lines(t), 2, "the second timeout must not be delivered before the second deadline")

	f.now = secondDeadline.Add(time.Minute)
	f.reconcile(t)
	assert.Equal(t, []string{
		at(began, "The person started testing the agent."),
		at(firstTimeout, "The test watch timed out."),
		at(f.now, "The test watch timed out."),
	}, f.lines(t), "the extra time the person asked for has run out, and they are told so")
	assert.Equal(t, []string{"started", "timedOut"}, f.status(t).Delivered)

	// And once: the deadline is behind now, so nothing may re-drop the marker.
	f.now = f.now.Add(10 * time.Minute)
	f.reconcile(t)
	assert.Len(t, f.lines(t), 3, "the second timeout is delivered exactly once")
}

// A person who comes back to a paused test is the only thing that moves it
// from Idle to Running, and until this line existed the page's "Paused"
// status stayed up for the rest of the run. The pause is what there is to
// resume FROM, so the line is owed only after one was delivered — never on
// the Running a test is first seen in.
func TestTestWatch_PausedThenResumedIsDeliveredOnceEach(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	f.reconcile(t)
	require.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
		"the first Running is the test starting, not a resume")

	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseIdle)
	f.reconcile(t)
	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseRunning)
	f.reconcile(t)
	// Nothing changed since: neither line is owed a second time.
	f.reconcile(t)

	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The person paused the test."),
		at(f.now, "The person resumed the test."),
	}, f.lines(t))
	assert.Equal(t, []string{"started", "paused", "resumed"}, f.status(t).Delivered)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, f.status(t).LastPhase)
}

// A woken session does not go straight back to Running: it goes through
// Pending while its pod comes back, and that is what a person answering a
// paused test actually produces. Recording Pending as the observed phase
// hides the pause it follows — the resumed line is owed only while the
// recorded phase is still Idle — and the page then stayed "paused" through a
// live reply. Only the phases the watch has an event for advance the record.
func TestTestWatch_AnIntermediatePhaseIsNotAnEventAndHidesNeitherSide(t *testing.T) {
	t.Run("Running to Idle to Pending to Running: paused then resumed, once each", func(t *testing.T) {
		f := readyWorkshop(t)
		f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
		s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.reconcile(t)

		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseIdle)
		f.reconcile(t)
		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhasePending)
		f.reconcile(t)
		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.reconcile(t)

		assert.Equal(t, []string{
			at(f.now, "The person started testing the agent."),
			at(f.now, "The person paused the test."),
			at(f.now, "The person resumed the test."),
		}, f.lines(t), "the Pending between the pause and the return is not an event, and must not swallow the return")
		assert.Equal(t, []string{"started", "paused", "resumed"}, f.status(t).Delivered)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, f.status(t).LastPhase,
			"the record holds the phase of the last event, never the one in between")
	})

	t.Run("Running to Pending to Idle: paused, once", func(t *testing.T) {
		f := readyWorkshop(t)
		f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
		s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.reconcile(t)

		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhasePending)
		f.reconcile(t)
		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseIdle)
		f.reconcile(t)

		assert.Equal(t, []string{
			at(f.now, "The person started testing the agent."),
			at(f.now, "The person paused the test."),
		}, f.lines(t), "a Pending on the way into a pause leaves the pause itself a change, and it is delivered")
		assert.Equal(t, []string{"started", "paused"}, f.status(t).Delivered)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, f.status(t).LastPhase)
	})

	// The other end of the same rule, and the one a browser-started test
	// actually hits first: the session is created Pending while its pod is
	// scheduled, so Pending — not Running — is the phase the watch sees the
	// test IN. It is still the person's own test, so the started line is
	// owed; the Pending itself is not an event, so nothing else is said and
	// nothing is recorded as the observed phase. Record it and the Running
	// that follows is read as a change from Pending, which is how the first
	// real pause after it comes to be compared against the wrong phase.
	t.Run("the test is first seen Pending, then Running: started only, and the record is untouched", func(t *testing.T) {
		f := readyWorkshop(t)
		f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
		s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhasePending)

		f.reconcile(t)

		assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
			"the test is the person's own, so it began; the Pending it began in is not itself an event")
		st := f.status(t)
		require.NotNil(t, st)
		assert.Equal(t, "try-1", st.Session)
		assert.Equal(t, []string{"started"}, st.Delivered)
		assert.Empty(t, st.LastPhase, "a phase the watch has no event for must never become the observed one")

		f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseRunning)
		f.reconcile(t)

		assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
			"the first Running is the test starting, which the started line already said")
		assert.Equal(t, []string{"started"}, f.status(t).Delivered)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, f.status(t).LastPhase,
			"and the first phase with an event behind it is what the record finally holds")
	})
}

// The AgentSession watch has to reach the owning Workshop from BOTH kinds of
// session: the builder's own (whose name derives the Workshop's) and a test
// session in the workshop namespace (whose name derives nothing).
func TestMapSessionToWorkshop_ResolvesBothKindsOfSession(t *testing.T) {
	f := readyWorkshop(t)
	ctx := context.Background()
	wantOwner := reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: f.ws.Namespace, Name: f.ws.Name,
	}}

	assert.Equal(t, []reconcile.Request{wantOwner}, f.r.MapSessionToWorkshop(ctx, f.sess),
		"a builder session still maps to the workshop named for it")

	test := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now, spiceboxv1alpha1.AgentSessionPhaseRunning)
	assert.Equal(t, []reconcile.Request{wantOwner}, f.r.MapSessionToWorkshop(ctx, test),
		"a test session maps to the workshop that owns its namespace, not to one named for itself")

	assert.Nil(t, f.r.MapSessionToWorkshop(ctx, &corev1.Namespace{}), "only AgentSessions map")
}

// A builder in the middle of its own turn is told right away. The line takes
// the "inbox" role, which shares no (index, role) key with the turns the
// builder's runner writes, so a busy builder is no reason to hold anything
// back: both the started line and the phase change behind it land on the
// pass that observed them.
func TestTestWatch_MidTurnBuilderIsToldRightAway(t *testing.T) {
	f := readyWorkshop(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	s := f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	res := f.reconcile(t)

	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
		"a busy builder is told on the pass that saw the test start")
	assert.Equal(t, []string{"default/builder-1"}, f.nudge, "and is nudged awake")
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "the ordinary poll cap; nothing is being waited out")
	st := f.status(t)
	require.NotNil(t, st)
	assert.Equal(t, []string{"started"}, st.Delivered)

	// The person's test ends while the builder is still busy: same answer.
	f.setPhase(t, s, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	f.reconcile(t)

	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The test ended."),
	}, f.lines(t))
	assert.Equal(t, []string{"started", "ended"}, f.status(t).Delivered)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, f.status(t).LastPhase,
		"the observed phase advances with the line that landed")
}

// A builder that has FINISHED will never come back to read anything, so a
// line for it is not re-attempted. The watch records the event and moves on
// rather than retrying for as long as the workshop lives.
//
// A finished builder's workshop is otherwise RELEASED before the watch runs
// (releaseIfFinished, close.go) — so the state this case lives in is the one
// that holds the release open: an install request still waiting on an admin.
// That is the only way a workshop outlives the builder that was reading it.
func TestTestWatch_FinishedBuilderDropsTheLineRatherThanRetryForever(t *testing.T) {
	f := readyWorkshop(t)
	f.setBuilderStatus(t, spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseFailed})
	f.holdReleaseForInstall(t)
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	res := f.reconcile(t)

	assert.Empty(t, f.lines(t), "a finished builder is appended nothing")
	assert.Empty(t, f.nudge, "and nobody is woken")
	assert.Equal(t, []string{"started"}, f.status(t).Delivered, "the event is recorded so the watch stops trying")
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "the ordinary poll cap")
}

// putFailsOnce is the memory facade with its first write knocked out, so a
// test can watch one delivery fail and the next one succeed. Reads pass
// through, so the transcript a test inspects is the real one.
type putFailsOnce struct {
	inner  memory.Memory
	failed bool
}

func (m *putFailsOnce) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if !m.failed {
		m.failed = true
		return memory.Entry{}, errors.New("the backend is down")
	}
	return m.inner.Put(ctx, e)
}

func (m *putFailsOnce) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return m.inner.Query(ctx, q)
}

func (m *putFailsOnce) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	return m.inner.Search(ctx, req)
}

func (m *putFailsOnce) SendSignal(ctx context.Context, sig memory.Signal) error {
	return m.inner.SendSignal(ctx, sig)
}

// A wake that failed behind a line that landed is not an undelivered line.
// The builder has the line in its transcript and reads it the next time
// anything wakes it; re-attempting would put the same sentence in front of it
// twice, which is why the event is recorded on this failure.
func TestTestWatch_LineThatLandedBehindAFailedWakeIsRecordedNotReAppended(t *testing.T) {
	f := readyWorkshop(t)
	failWake := true
	f.r.PublishInteraction = func(_ context.Context, ns, name string, _ channelevents.Envelope) error {
		f.nudge = append(f.nudge, ns+"/"+name)
		if failWake {
			failWake = false
			return errors.New("the bus is down")
		}
		return nil
	}
	f.setWatch(t, "demo-agent", f.now, f.now.Add(30*time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	f.reconcile(t)

	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
		"the line landed even though the nudge did not")
	assert.Equal(t, []string{"started"}, f.status(t).Delivered,
		"a line in the transcript is delivered, so the event is recorded")

	f.reconcile(t)

	assert.Equal(t, []string{at(f.now, "The person started testing the agent.")}, f.lines(t),
		"the builder must not be told the same thing twice")
}

// The started line gates the timeout the same way it gates a phase change. A
// watch that delivered its timeout while the started line was still owed
// would leave the builder hearing that a test it never heard begin has now
// run out of time.
func TestTestWatch_TimeoutWaitsForTheStartedLineToLand(t *testing.T) {
	f := readyWorkshop(t)
	var racey memory.Memory = &putFailsOnce{inner: f.mem}
	f.r.ParentMemory = racey
	// The deadline has already passed, so both lines are owed on the same pass.
	f.setWatch(t, "demo-agent", f.now.Add(-time.Hour), f.now.Add(-time.Minute))
	f.testSession(t, "try-1", "demo-agent", "c4nonical", f.now.Add(-30*time.Minute), spiceboxv1alpha1.AgentSessionPhaseRunning)

	res := f.reconcile(t)

	assert.Empty(t, f.lines(t), "the started line did not land, so nothing else is delivered ahead of it")
	assert.Empty(t, f.status(t).Delivered, "and nothing is recorded")
	assert.Equal(t, 30*time.Second, res.RequeueAfter,
		"the deadline can no longer schedule anything, so the poll cap brings the watch back")

	f.reconcile(t)

	assert.Equal(t, []string{
		at(f.now, "The person started testing the agent."),
		at(f.now, "The test watch timed out."),
	}, f.lines(t), "the retried started line goes first, and the timeout follows it")
	assert.Equal(t, []string{"started", "timedOut"}, f.status(t).Delivered)
}
