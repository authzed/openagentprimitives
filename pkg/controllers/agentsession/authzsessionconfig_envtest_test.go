//go:build integration

// pkg/controllers/agentsession/authzsessionconfig_envtest_test.go
//
// The ORDERING half of the operator-authored authz_session_config snapshot.
// The unit tests next door pin the derivation and the write; what only a full
// Reconcile can show is that the record is durable BEFORE the runner pod that
// reads it is created — the property the whole move exists to establish.
//
// Ordering is asserted by OBSERVING the two events as the reconcile performs
// them, not by inspecting their after-effects once it has returned. Reading the
// record and the pod afterwards cannot distinguish "the snapshot was written
// first" from "both happened in some order during the same pass", which is the
// entire claim: authzd fails a session closed on a missing snapshot
// (internal/cmd/authzd/cold_start_policy.go), so a snapshot written after the reader is
// running is not the same guarantee.
package agentsession_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
)

// The two events whose RELATIVE ORDER is the security property.
const (
	evSnapshot    = "authz_session_config snapshot"
	evRunnerStart = "RunnerFactory.Start"
)

// ascKind is the Kind name carried on the snapshot's Entry — the one write
// snapshotRecorder singles out of everything the reconciler puts.
var ascKind = asc.Kind{}.Name()

// eventLog is the shared, ordered record of those two events. Reconcile is
// single-goroutine, but the mutex keeps the log honest if a factory ever starts
// its work asynchronously.
type eventLog struct {
	mu  sync.Mutex
	seq []string
}

func (l *eventLog) add(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq = append(l.seq, ev)
}

func (l *eventLog) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seq)
}

// snapshotRecorder is the operator's memory facade with the one write we care
// about instrumented. Only a SUCCESSFUL Put of an authz_session_config entry is
// recorded: the claim is about a durable record, not an attempt.
type snapshotRecorder struct {
	memory.Memory
	log *eventLog
}

func (s snapshotRecorder) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	out, err := s.Memory.Put(ctx, e)
	if err == nil && e.Kind == ascKind {
		s.log.add(evSnapshot)
	}
	return out, err
}

// runnerStartRecorder records that the reconcile reached the runner-start step
// BEFORE delegating, so the log marks the moment the process that reads the
// record is asked for — the earliest instant the ordering claim must beat.
type runnerStartRecorder struct {
	agentsession.RunnerFactory
	log *eventLog
}

func (f runnerStartRecorder) Start(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession,
	class *spiceboxv1alpha1.AgentClass, opts agentsession.StartOpts,
) error {
	f.log.add(evRunnerStart)
	return f.RunnerFactory.Start(ctx, sess, class, opts)
}

// instrument wires both recorders onto a reconciler and returns the log plus
// the underlying facade (which still exposes DeleteScope and the like).
func instrument(t *testing.T, r *agentsession.Reconciler) (*eventLog, *memory.Local) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	log := &eventLog{}
	r.LifecycleMemory = snapshotRecorder{Memory: mem, log: log}
	r.RunnerFactory = runnerStartRecorder{RunnerFactory: r.RunnerFactory, log: log}
	return log, mem
}

// scopedClass is a valid AgentClass with cold-start scope review enabled — the
// configuration whose gate authzd resolves out of this record.
func scopedClass(name string) *spiceboxv1alpha1.AgentClass {
	ac := validClass(name)
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Scope: &spiceboxv1alpha1.ScopeSpec{Enabled: true, ColdStart: "extractAndApprove"},
	}
	return ac
}

// reconcileUntilRunnerStarted drives Reconcile until the runner-start step is
// reached, so a test that asserts an ordering against it is never vacuously
// green because the reconcile stopped short. Several passes are needed (the
// first is EnsureFinalizer's short-circuit, and later steps persist status and
// return before the pod); the loop exits on the pass that reaches Start and
// fails loudly if none does.
func reconcileUntilRunnerStarted(
	t *testing.T, ctx context.Context, r *agentsession.Reconciler,
	sess *spiceboxv1alpha1.AgentSession, log *eventLog,
) {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)}
	for range 6 {
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err, "Reconcile must succeed for a valid scope-enabled session")
		if slices.Contains(log.events(), evRunnerStart) {
			return
		}
	}
	require.Contains(t, log.events(), evRunnerStart,
		"the reconcile never reached RunnerFactory.Start, so nothing here can be an ordering claim")
}

// TestReconcile_WritesAuthzSessionConfigBeforeTheRunnerPod is the liveness
// guarantee. A missing snapshot is not a soft degradation: authzd's
// resolveColdStartPolicy fails closed on it and the runner halts the session
// with a user-visible refusal. So the record must be durable by the time the
// runner is started — asserted as a SEQUENCE, on a FRESH session.
func TestReconcile_WritesAuthzSessionConfigBeforeTheRunnerPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	r := newReconciler(t, env)
	log, mem := instrument(t, r)

	ac := scopedClass("asc-cls")
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("asc-sess", ac.Name)
	require.NoError(t, env.Client.Create(ctx, sess))

	// The first pass only adds the finalizer and returns (controller.go's
	// EnsureFinalizer short-circuit), so the snapshot lands on a later pass —
	// the same pass that reaches the runner at step 5b.
	reconcileUntilRunnerStarted(t, ctx, r, sess, log)

	// THE ordering assertion: the snapshot Put must appear in the log strictly
	// before the runner is started. Compared by first occurrence, since a
	// later pass re-enters step 5b (Start is idempotent) and the read-before-
	// write makes an unchanged pass write nothing.
	events := log.events()
	snapAt := slices.Index(events, evSnapshot)
	startAt := slices.Index(events, evRunnerStart)
	require.NotEqual(t, -1, snapAt,
		"the operator never wrote authz_session_config; without it authzd fails this session closed: %v", events)
	require.NotEqual(t, -1, startAt, "runner never started: %v", events)
	assert.Less(t, snapAt, startAt,
		"the snapshot must be durable BEFORE the runner that reads it is started: %v", events)

	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	got, found, gerr := asc.Get(ctx, mem, scope)
	require.NoError(t, gerr)
	require.True(t, found, "the snapshot must still be readable after the reconcile")
	assert.True(t, got.ScopeEnabled, "scopeEnabled must mirror the AgentClass the operator read")
	assert.Equal(t, "extractAndApprove", got.ColdStart)

	// The pod is the runner-start event's real-world consequence. Asserted
	// strictly: a NotFound here would mean the ordering above was measured
	// against a step that produced no reader.
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{
		Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess),
	}, &pod), "runner pod must exist once RunnerFactory.Start has run")
}

// TestReconcile_AuthzSessionConfigSurvivesRestart pins the restart half: a
// session whose record was lost (scope GC, a fresh store) gets it rewritten by
// the next reconcile, again before the runner is restarted. Each step of that
// chain is asserted — the record existed, the loss really happened, the rewrite
// really happened — so a change that made the write once-per-session (a status
// latch, an in-process seen-set) fails here instead of passing quietly.
func TestReconcile_AuthzSessionConfigSurvivesRestart(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	r := newReconciler(t, env)
	log, mem := instrument(t, r)

	ac := scopedClass("asc-restart-cls")
	require.NoError(t, env.Client.Create(ctx, ac))
	markValid(t, env, ac)

	sess := validSession("asc-restart-sess", ac.Name)
	require.NoError(t, env.Client.Create(ctx, sess))

	key := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sess)}
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}

	// Drive past the finalizer short-circuit so the record genuinely exists —
	// otherwise the deletion below removes nothing and the "rewritten" claim is
	// satisfied by the first write that would have happened anyway.
	reconcileUntilRunnerStarted(t, ctx, r, sess, log)
	_, found, gerr := asc.Get(ctx, mem, scope)
	require.NoError(t, gerr)
	require.True(t, found, "precondition: the reconcile must have written the record before it can be lost")

	require.NoError(t, mem.DeleteScope(ctx, scope), "simulate the record being lost")
	_, found, gerr = asc.Get(ctx, mem, scope)
	require.NoError(t, gerr)
	require.False(t, found, "precondition: the record must actually be gone for this to test a rewrite")

	_, err := r.Reconcile(ctx, key)
	require.NoError(t, err)

	_, found, gerr = asc.Get(ctx, mem, scope)
	require.NoError(t, gerr)
	assert.True(t, found, "a later reconcile must rewrite the snapshot rather than leave the session unresolvable")
}
