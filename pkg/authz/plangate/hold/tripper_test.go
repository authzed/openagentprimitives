package hold

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

func TestDenialStreak_tripsAtThreshold(t *testing.T) {
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied},
		{Outcome: plangateaudit.OutcomeDenied},
		{Outcome: plangateaudit.OutcomeDenied},
	}
	assert.Equal(t, 3, consecutiveDenials(records))
}

func TestDenialStreak_allowResetsTheRun(t *testing.T) {
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied},
		{Outcome: plangateaudit.OutcomeDenied},
		{Outcome: plangateaudit.OutcomeAllow},
		{Outcome: plangateaudit.OutcomeDenied},
	}
	assert.Equal(t, 1, consecutiveDenials(records),
		"an allowed call means the agent found its way back inside the ceiling")
}

func TestDenialStreak_wouldDenyCounts(t *testing.T) {
	// Logging mode records would_deny rather than denied. The tripper must see
	// the same behaviour in both modes, or it only ever fires where enforcement
	// is already on.
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeWouldDeny},
		{Outcome: plangateaudit.OutcomeWouldDeny},
	}
	assert.Equal(t, 2, consecutiveDenials(records))
}

// TestConsecutiveDenialsAcrossClosure_DistinctTimestampsMergeInOrder pins the
// non-tied case: records from different sessions, sorted oldest-first by At
// with no collisions, behave exactly like consecutiveDenials over one log.
func TestConsecutiveDenialsAcrossClosure_DistinctTimestampsMergeInOrder(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied, At: base},
		{Outcome: plangateaudit.OutcomeDenied, At: base.Add(time.Second)},
		{Outcome: plangateaudit.OutcomeDenied, At: base.Add(2 * time.Second)},
	}
	assert.Equal(t, 3, consecutiveDenialsAcrossClosure(records))
}

// TestConsecutiveDenialsAcrossClosure_AllowAtTheEndResetsTheRun mirrors
// TestDenialStreak_allowResetsTheRun for the merged, multi-session case.
func TestConsecutiveDenialsAcrossClosure_AllowAtTheEndResetsTheRun(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied, At: base},
		{Outcome: plangateaudit.OutcomeDenied, At: base.Add(time.Second)},
		{Outcome: plangateaudit.OutcomeAllow, At: base.Add(2 * time.Second)},
	}
	assert.Equal(t, 0, consecutiveDenialsAcrossClosure(records))
}

// TestConsecutiveDenialsAcrossClosure_TiedAllowBreaksRatherThanCredits pins
// the ordering rule this function's doc comment states: two sessions'
// records land at the SAME instant (a tie), one a denial and one an allow.
// Their true relative order can't be recovered, so the tie must resolve
// toward NOT tripping — the denial in the tied group is not credited, exactly
// as if the allow were the newer of the two.
func TestConsecutiveDenialsAcrossClosure_TiedAllowBreaksRatherThanCredits(t *testing.T) {
	tie := time.Unix(1700000000, 0).UTC()
	older := []plangateaudit.Content{
		// An older, unambiguous denial run that a wrongly-resolved tie could
		// wrongly extend into.
		{Outcome: plangateaudit.OutcomeDenied, At: tie.Add(-2 * time.Second)},
		{Outcome: plangateaudit.OutcomeDenied, At: tie.Add(-time.Second)},
	}
	tied := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied, At: tie},
		{Outcome: plangateaudit.OutcomeAllow, At: tie},
	}
	records := append(append([]plangateaudit.Content{}, older...), tied...)

	assert.Equal(t, 0, consecutiveDenialsAcrossClosure(records),
		"a tied allow must break the run for the whole group, not just for records ordered after it")
}

// TestConsecutiveDenialsAcrossClosure_TiedDenialsWithNoAllowAllCount pins the
// other half of the tie rule: a tied group with no allow in it is NOT
// ambiguous (every governed outcome in it is a denial either way), so it
// simply extends the run rather than being treated with the same caution as
// a tie containing an allow.
func TestConsecutiveDenialsAcrossClosure_TiedDenialsWithNoAllowAllCount(t *testing.T) {
	tie := time.Unix(1700000000, 0).UTC()
	records := []plangateaudit.Content{
		{Outcome: plangateaudit.OutcomeDenied, At: tie},
		{Outcome: plangateaudit.OutcomeWouldDeny, At: tie},
	}
	assert.Equal(t, 2, consecutiveDenialsAcrossClosure(records))
}

func TestDenialStreak_belowThresholdDoesNotTrip(t *testing.T) {
	d := &DenialStreak{deps: DenialStreakDeps{Threshold: 5}}
	assert.False(t, d.shouldTrip(4))
	assert.True(t, d.shouldTrip(5))
	assert.True(t, d.shouldTrip(6))
}

func TestDenialStreak_zeroThresholdNeverTrips(t *testing.T) {
	// Zero means disabled, not "trip on everything" — an unset threshold must
	// never read as "freeze every session".
	d := &DenialStreak{deps: DenialStreakDeps{Threshold: 0}}
	assert.False(t, d.shouldTrip(100))
}

func TestDenialStreak_name(t *testing.T) {
	d := NewDenialStreak(DenialStreakDeps{Threshold: 5})
	require.NotNil(t, d)
	assert.Equal(t, "plangate-denial-streak", d.Name())
}

// newTripFixture seeds a fake client with objs and a fresh in-memory
// memory.Memory, and returns a DenialStreak wired to both plus threshold.
func newTripFixture(t *testing.T, threshold int, objs ...client.Object) (client.Client, memorypkg.Memory, *DenialStreak) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	mem := memorypkg.NewLocal(inmem.NewBackend())
	d := NewDenialStreak(DenialStreakDeps{Threshold: threshold, Mem: mem, Client: c})
	return c, mem, d
}

// seedDenials records n denied plan-gate entries for scope, oldest-first.
func seedDenials(t *testing.T, mem memorypkg.Memory, scope memorypkg.Scope, n int) {
	t.Helper()
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	for i := 0; i < n; i++ {
		require.NoError(t, plangateaudit.Record(ctx, mem, scope, plangateaudit.Content{
			Outcome: plangateaudit.OutcomeDenied,
		}))
	}
}

func TestDenialStreak_OnSignal_tripAtThresholdOwnsHoldByAgentSession(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns", UID: "demo-uid"},
	}
	c, mem, d := newTripFixture(t, 2, sess)
	scope := memorypkg.Scope{Kind: "session", ID: "demo-ns/demo-session"}
	seedDenials(t, mem, scope, 2)

	err := d.OnSignal(context.Background(), memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scope})
	require.NoError(t, err)

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("demo-ns")))
	require.Len(t, list.Items, 1, "the streak reached threshold, so exactly one hold must be created")

	got := list.Items[0]
	assert.Equal(t, "tripper/plangate-denial-streak", got.Spec.Source)
	assert.Equal(t, "demo-session", got.Spec.SessionRef.Name)
	assert.Equal(t, cosidecar.OwnerRef(sess), got.OwnerReferences,
		"a hold must own-reference the AgentSession it freezes, or a hold left behind by a deleted session would keep matching a same-named session recreated later")
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", got.OwnerReferences[0].Kind)
	assert.Equal(t, "demo-session", got.OwnerReferences[0].Name)
	assert.Equal(t, sess.UID, got.OwnerReferences[0].UID)
}

func TestDenialStreak_OnSignal_sessionGoneCreatesNoHold(t *testing.T) {
	// No AgentSession object seeded: the session is already gone by the time
	// the streak crosses threshold.
	c, mem, d := newTripFixture(t, 2)
	scope := memorypkg.Scope{Kind: "session", ID: "demo-ns/demo-session"}
	seedDenials(t, mem, scope, 2)

	err := d.OnSignal(context.Background(), memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scope})
	require.NoError(t, err, "a session gone before trip is a no-op, not a retryable error")

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("demo-ns")))
	assert.Empty(t, list.Items, "no SessionHold may be created for a session that no longer exists")
}

// --- Closure-aware streak: a delegating root's descendants share its streak ---
//
// closureNS is the namespace shared by every fixture below, matching the
// flat "demo-ns" the single-session fixtures above already use.
const closureNS = "demo-ns"

// closureSession builds an AgentSession in closureNS, parented and
// delegation-labelled the way buildChild (pkg/controllers/subagentrequest)
// stamps a real delegated child at creation time: a root carries no label at
// all (RootNameFor's rule — a session with no label IS a root) and every
// descendant carries LabelDelegationRoot set to the tree's root name. Mirrors
// pkg/controllers/sessionhold/cascade_test.go's treeSession fixture shape.
func closureSession(name, parentName, root string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: closureNS, Name: name},
	}
	if parentName != "" {
		s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: closureNS, Name: parentName}
	}
	if root != "" {
		s.Labels = map[string]string{spiceboxv1alpha1.LabelDelegationRoot: root}
	}
	return s
}

// scopeFor returns sess's plan_gate_audit memory scope.
func scopeFor(sess *spiceboxv1alpha1.AgentSession) memorypkg.Scope {
	return memorypkg.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
}

// seedRecordAt records one plan-gate entry for scope with an explicit outcome
// and timestamp — seedDenials' finer-grained sibling, needed once records
// must be orderable ACROSS scopes rather than merely appended within one.
func seedRecordAt(t *testing.T, mem memorypkg.Memory, scope memorypkg.Scope, outcome string, at time.Time) {
	t.Helper()
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	require.NoError(t, plangateaudit.Record(ctx, mem, scope, plangateaudit.Content{
		Outcome: outcome,
		At:      at,
	}))
}

// queryCountingMemory wraps a real Memory and counts calls to Query, so a
// test can assert exactly how many plan_gate_audit scope reads OnSignal
// performed. A non-delegated session's whole cost budget is one such read —
// only a call count proves the closure work was never reached, since the
// RESULT (no hold, either way) looks identical whether it was or not.
type queryCountingMemory struct {
	memorypkg.Memory
	queries int
}

func (m *queryCountingMemory) Query(ctx context.Context, q memorypkg.Query) (memorypkg.QueryResult, error) {
	m.queries++
	return m.Memory.Query(ctx, q)
}

// TestDenialStreak_OnSignal_Closure_SiblingDenialsTripTheRoot is the
// delegation-evasion regression test: denials spread one-each across three
// siblings must trip the ROOT's streak. A single-scope-only OnSignal reads
// only the signalling child's own log — which never accumulates enough
// denials of its own — so a parent could spread its denials across children
// and never trip a streak at any threshold.
func TestDenialStreak_OnSignal_Closure_SiblingDenialsTripTheRoot(t *testing.T) {
	root := closureSession("root-session", "", "")
	child1 := closureSession("child-a", "root-session", "root-session")
	child2 := closureSession("child-b", "root-session", "root-session")
	child3 := closureSession("child-c", "root-session", "root-session")

	c, mem, d := newTripFixture(t, 3, root, child1, child2, child3)

	base := time.Unix(1700000000, 0).UTC()
	seedRecordAt(t, mem, scopeFor(child1), plangateaudit.OutcomeDenied, base)
	seedRecordAt(t, mem, scopeFor(child2), plangateaudit.OutcomeDenied, base.Add(time.Second))
	seedRecordAt(t, mem, scopeFor(child3), plangateaudit.OutcomeDenied, base.Add(2*time.Second))

	// The signal fires on the scope that just received the newest denial, as a
	// real append-only Put's post-write dispatch would.
	sig := memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scopeFor(child3)}
	err := d.OnSignal(context.Background(), sig)
	require.NoError(t, err)

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(closureNS)))
	require.Len(t, list.Items, 1,
		"three siblings denied once each must trip the root's streak — today this creates nothing at any threshold, which is the vulnerability")

	got := list.Items[0]
	assert.Equal(t, "root-session", got.Spec.SessionRef.Name,
		"the hold must land on the ROOT, not the child whose signal fired — the SessionHold cascade distributes it back down to the children")
	assert.Contains(t, got.Spec.Reason, "delegation",
		"the reason must name the closure, not read like a single session's own streak")
}

// TestDenialStreak_OnSignal_Closure_AllowAnywhereResetsTheRun: an allow
// anywhere in the closure — chronologically after the denials — resets the
// run, exactly as an allow within one session's own log would. This
// preserves the gate's existing semantic: finding the way back inside the
// ceiling is the behaviour the gate is meant to produce.
func TestDenialStreak_OnSignal_Closure_AllowAnywhereResetsTheRun(t *testing.T) {
	root := closureSession("root-session", "", "")
	child1 := closureSession("child-a", "root-session", "root-session")
	child2 := closureSession("child-b", "root-session", "root-session")
	child3 := closureSession("child-c", "root-session", "root-session")
	child4 := closureSession("child-d", "root-session", "root-session")

	c, mem, d := newTripFixture(t, 3, root, child1, child2, child3, child4)

	base := time.Unix(1700000000, 0).UTC()
	seedRecordAt(t, mem, scopeFor(child1), plangateaudit.OutcomeDenied, base)
	seedRecordAt(t, mem, scopeFor(child2), plangateaudit.OutcomeDenied, base.Add(time.Second))
	seedRecordAt(t, mem, scopeFor(child3), plangateaudit.OutcomeDenied, base.Add(2*time.Second))
	seedRecordAt(t, mem, scopeFor(child4), plangateaudit.OutcomeAllow, base.Add(3*time.Second))

	sig := memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scopeFor(child4)}
	err := d.OnSignal(context.Background(), sig)
	require.NoError(t, err)

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(closureNS)))
	assert.Empty(t, list.Items,
		"an allow anywhere in the closure, chronologically after the denials, must reset the run")
}

// TestDenialStreak_OnSignal_Closure_ZeroThresholdStillDisabled: threshold
// zero must stay disabled across a closure too. A backstop that switches
// itself on the moment delegation appears would be a nasty surprise for
// every existing install that runs with it off.
func TestDenialStreak_OnSignal_Closure_ZeroThresholdStillDisabled(t *testing.T) {
	root := closureSession("root-session", "", "")
	child1 := closureSession("child-a", "root-session", "root-session")
	child2 := closureSession("child-b", "root-session", "root-session")
	child3 := closureSession("child-c", "root-session", "root-session")

	c, mem, d := newTripFixture(t, 0, root, child1, child2, child3)

	base := time.Unix(1700000000, 0).UTC()
	seedRecordAt(t, mem, scopeFor(child1), plangateaudit.OutcomeDenied, base)
	seedRecordAt(t, mem, scopeFor(child2), plangateaudit.OutcomeDenied, base.Add(time.Second))
	seedRecordAt(t, mem, scopeFor(child3), plangateaudit.OutcomeDenied, base.Add(2*time.Second))

	sig := memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scopeFor(child3)}
	err := d.OnSignal(context.Background(), sig)
	require.NoError(t, err)

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(closureNS)))
	assert.Empty(t, list.Items,
		"threshold zero must stay disabled across a closure, or delegation would turn on a backstop that was off for every existing install")
}

// TestDenialStreak_OnSignal_Closure_NoLineageStaysToOneScopeRead: a session
// with no lineage — no parent, no labelled descendants — must cost exactly
// ONE plan_gate_audit scope read. The closure walk (root resolution +
// ListClosure) is a K8s read, not a memory read, and is allowed to run every
// signal; what must NOT happen is the closure work turning into a SECOND (or
// Nth) memory read for the 99% of sessions that never delegate, since this
// handler fires after every successful append-only Put.
func TestDenialStreak_OnSignal_Closure_NoLineageStaysToOneScopeRead(t *testing.T) {
	sess := closureSession("solo-session", "", "")

	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	counted := &queryCountingMemory{Memory: memorypkg.NewLocal(inmem.NewBackend())}
	d := NewDenialStreak(DenialStreakDeps{Threshold: 5, Mem: counted, Client: c})

	scope := scopeFor(sess)
	seedDenials(t, counted, scope, 2)

	err := d.OnSignal(context.Background(), memorypkg.Signal{Kind: memorypkg.SignalEntryAppended, Scope: scope})
	require.NoError(t, err)

	assert.Equal(t, 1, counted.queries,
		"a session with no lineage must cost exactly one plan_gate_audit read per signal")
}

// --- trip: an existing unreleased hold must be reused, not multiplied ---
//
// The hold name embeds the streak value
// ("plangate-denial-streak-<session>-<streak>"), so a retry of trip at the
// SAME streak dedups via Create's AlreadyExists -- but a SECOND trip at a
// HIGHER streak value names a DIFFERENT object and sails straight past that
// dedup. Within one session this was near-harmless (the first Active stamp
// cancels the runner's context, so no further denials arrive), but across a
// delegation closure the hold lands on the root and the root's own
// cancellation does nothing to the children: they keep running until their
// own cascaded holds go Active, at least one reconcile hop away, and every
// denial in that window would otherwise mint another root hold.

// TestDenialStreak_trip_ExistingUnreleasedHoldSuppressesANewOne pins the
// whole-branch review's Finding 3: two trips at different streak values
// against a session that already has an unreleased hold must produce ONE
// hold, not two.
func TestDenialStreak_trip_ExistingUnreleasedHoldSuppressesANewOne(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns", UID: "demo-uid"},
	}
	c, _, d := newTripFixture(t, 2, sess)
	scope := memorypkg.Scope{Kind: "session", ID: "demo-ns/demo-session"}

	require.NoError(t, d.trip(context.Background(), scope, 2, nil, 1), "first trip, streak 2")
	require.NoError(t, d.trip(context.Background(), scope, 5, nil, 1), "second trip, streak 5 -- a DIFFERENT hold name")

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("demo-ns")))
	assert.Len(t, list.Items, 1,
		"a second trip at a higher streak value must not mint a second hold while the first is still unreleased")
}

// TestDenialStreak_trip_ReleasedHoldDoesNotSuppressANewOne proves the guard
// is an EXISTENCE check on an UNRELEASED hold, not a blanket "this session
// was ever held before": once a prior hold is released (the session resumed
// and was let go), a fresh trip must still be able to freeze it again.
func TestDenialStreak_trip_ReleasedHoldDoesNotSuppressANewOne(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns", UID: "demo-uid"},
	}
	released := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "plangate-denial-streak-demo-session-2", Namespace: "demo-ns"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "demo-session"},
			Source:     "tripper/plangate-denial-streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{Phase: spiceboxv1alpha1.SessionHoldPhaseReleased},
	}
	c, _, d := newTripFixture(t, 2, sess, released)
	scope := memorypkg.Scope{Kind: "session", ID: "demo-ns/demo-session"}

	require.NoError(t, d.trip(context.Background(), scope, 5, nil, 1))

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("demo-ns")))
	assert.Len(t, list.Items, 2,
		"a RELEASED prior hold must not suppress a legitimate new trip -- the session resumed and misbehaved again")
}

// TestDenialStreak_trip_ClosurePhaseSuffixSuppressed pins the doc sweep's
// tripper.go:335-336 minor: in the closure path, records is a MERGED
// multi-session timeline, so activePhaseIndex can return a CHILD's phase
// while the hold is created on the ROOT -- naming the wrong session's plan.
// closureSize > 1 must suppress the " in phase N" suffix entirely rather than
// print a number that does not describe the session the hold actually names.
func TestDenialStreak_trip_ClosurePhaseSuffixSuppressed(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "root-session", Namespace: "demo-ns", UID: "demo-uid"},
	}
	c, _, d := newTripFixture(t, 2, sess)
	scope := memorypkg.Scope{Kind: "session", ID: "demo-ns/root-session"}
	phase := int32(3)
	records := []plangateaudit.Content{{Outcome: plangateaudit.OutcomeDenied, PhaseIndex: &phase}}

	require.NoError(t, d.trip(context.Background(), scope, 4, records, 3))

	var list spiceboxv1alpha1.SessionHoldList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace("demo-ns")))
	require.Len(t, list.Items, 1)
	assert.NotContains(t, list.Items[0].Spec.Reason, "phase",
		"a closure-wide streak must not print a per-record phase index -- it may name a CHILD's phase while the hold is created on the root")
}
