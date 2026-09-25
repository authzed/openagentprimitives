//go:build integration

// pkg/controllers/agentsession/identitychoice_envtest_test.go
//
// EXHAUSTIVE operator state-graph verification for the ask/dynamic identity
// modes. This drives every row of the identity-choice state-transition table
// through the REAL AgentSession reconciler under envtest — the linchpin that proves the
// operator gate (reconcileIdentityChoice), the passthrough cred-park reap, the
// EffectiveIdentityMode projection, and the deadline backstop behave end-to-end.
//
// How a row is driven:
//
//   - A spy RunnerFactory records Start/Stop AND creates/deletes a minimal real
//     Pod. The pod is load-bearing: the operator only reaches its end-of-loop
//     fold (derivePhase) when a runner pod already exists — the reconcile
//     returns early on the "pod not created yet" branch otherwise. So the spy
//     both records the call (the brief's "runner pod exists" assertion) and lets
//     the fold-driven phase advance.
//
//   - Lifecycle events are appended to the signed log exactly as the RUNNER
//     appends them (RegionRunner, turn 1), via the shared seedLifecycleEvent
//     helper (lifecycle_fold_envtest_test.go). The operator folds this log every
//     reconcile.
//
// Division of labor the table encodes (verified here, not assumed):
//
//   - IdentityChoicePending / IdentityChoiceResolved / IdentityChoiceCancelled
//     are RUNNER-emitted. For a TERMINAL runner outcome (cancel) or an ADVANCING
//     one (agent → Running), the runner ALSO writes the CR status directly
//     (StatusPatcher.WriteFailed / PatchProgress→Running) in the same process —
//     the operator's fold is a reconstruction/backstop, not the primary writer
//     of FailureReason or the post-choice Running phase. These rows therefore
//     faithfully simulate BOTH the log append AND the runner's status write; the
//     operator's job under test is to mirror EffectiveIdentityMode, honor the
//     passthrough re-park + reap, enforce the timeout backstop, and never
//     resurrect a terminal session. See the per-row comments.
package agentsession_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"

	// Registers MCP + CLI + toolspec authkind so RequiredCredentials can resolve
	// MCPServer targets in the passthrough (Row 3) path.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
)

// ---------------------------------------------------------------------------
// Spy runner factory: records Start/Stop and creates/deletes a real Pod.
// ---------------------------------------------------------------------------

// spyRunnerFactory records every Start/Stop and maintains a real (minimal) Pod
// so the reconcile reaches the end-of-loop fold. Idempotent like the production
// factories: a repeat Start on an existing pod is a no-op; a Stop of an absent
// pod is a no-op.
type spyRunnerFactory struct {
	mu     sync.Mutex
	cl     client.Client
	starts int
	stops  int
}

func (f *spyRunnerFactory) Start(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.AgentClass, _ agentsession.StartOpts) error {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runner", Image: "agentprimitives-runner:dev"}},
		},
	}
	if err := f.cl.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (f *spyRunnerFactory) Stop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}}
	if err := f.cl.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ObservedName returns "" — this fake is not *agentsession.PodRunnerFactory,
// so the reconciler never recorded its Pod on status.runnerPodName before
// ObservedName existed; returning "" here preserves that.
func (f *spyRunnerFactory) ObservedName(_ *spiceboxv1alpha1.AgentSession) string {
	return ""
}

func (f *spyRunnerFactory) startCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }
func (f *spyRunnerFactory) stopCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.stops }

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// newIdentityReconciler wires a reconciler with the spy factory and a real
// (inmem) signed lifecycle log — the operator folds this log to learn the
// runner-appended identity choice.
func newIdentityReconciler(t *testing.T, env *testenv.Env, spy *spyRunnerFactory, mem memory.Memory) *agentsession.Reconciler {
	t.Helper()
	r := newReconciler(t, env)
	r.RunnerFactory = spy
	r.LifecycleMemory = mem
	return r
}

// askClass builds a valid ask|dynamic AgentClass. The CEL surface requires a
// non-empty spec.agentIdentity for both interactive modes (and a recommender
// prompt for dynamic), so those are always populated.
func askClass(name, mode string, mutate func(*spiceboxv1alpha1.AgentClass)) *spiceboxv1alpha1.AgentClass {
	ac := validClass(name)
	ac.Spec.IdentityMode = mode
	if mode == spiceboxv1alpha1.IdentityModeAsk || mode == spiceboxv1alpha1.IdentityModeDynamic {
		ac.Spec.AgentIdentity = "agent-identity"
	}
	if mode == spiceboxv1alpha1.IdentityModeDynamic {
		ac.Spec.IdentityRecommendation = &spiceboxv1alpha1.IdentityRecommendationConfig{
			Prompt: "prefer the agent identity in busy multi-person threads",
		}
	}
	if mutate != nil {
		mutate(ac)
	}
	return ac
}

// starterSession returns a session carrying the started-by annotation (required
// for a userPassthrough credential gate).
func starterSession(name, class string) *spiceboxv1alpha1.AgentSession {
	s := validSession(name, class)
	s.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:starter@example.com",
	}
	return s
}

// reconcileTimes runs n reconciles against key, requiring each returns without
// error. The identity gate returns (result, nil) on every parked/proceed path,
// so a hard error is always a real failure.
func reconcileTimes(t *testing.T, ctx context.Context, r *agentsession.Reconciler, key types.NamespacedName, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			require.NoErrorf(t, err, "reconcile %d/%d for %s", i+1, n, key)
		}
	}
}

// loadSession reloads the session CR.
func loadSession(t *testing.T, ctx context.Context, env *testenv.Env, key types.NamespacedName) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var s spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &s), "get session %s", key)
	return &s
}

// writeRunnerStatus simulates the runner writing the CR status directly (the
// StatusPatcher path): fetch fresh, mutate status, Status().Update. Used to
// reproduce the terminal WriteFailed and the PatchProgress→Running writes the
// runner performs alongside its lifecycle-log appends.
func writeRunnerStatus(t *testing.T, ctx context.Context, env *testenv.Env, key types.NamespacedName, mutate func(*spiceboxv1alpha1.AgentSessionStatus)) {
	t.Helper()
	var fresh spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &fresh), "get session for runner status write")
	mutate(&fresh.Status)
	require.NoError(t, env.Client.Status().Update(ctx, &fresh), "runner status write on %s", key)
}

// readLifecycleEvents returns the ordered, typed events in a session's signed
// lifecycle log (the same read the operator's fold performs).
func readLifecycleEvents(t *testing.T, mem memory.Memory, ns, name string) []lifecyclecore.Event {
	t.Helper()
	events, err := lifecyclekind.Events(memory.WithSystemApproval(context.Background(), "test"), mem, lifecycleScopeFor(ns, name))
	require.NoError(t, err, "read lifecycle log %s/%s", ns, name)
	return events
}

// identityEventCounts tallies the identity-choice events present in the signed
// lifecycle log, for the edge-gating (no-dup) and durability assertions.
func identityEventCounts(t *testing.T, mem memory.Memory, ns, name string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	ev := readLifecycleEvents(t, mem, ns, name)
	for _, e := range ev {
		switch e.(type) {
		case lifecyclecore.IdentityChoicePending:
			counts["IdentityChoicePending"]++
		case lifecyclecore.IdentityChoiceResolved:
			counts["IdentityChoiceResolved"]++
		case lifecyclecore.IdentityChoiceCancelled:
			counts["IdentityChoiceCancelled"]++
		case lifecyclecore.IdentityChoiceTimeout:
			counts["IdentityChoiceTimeout"]++
		}
	}
	return counts
}

// injectPending appends the runner's IdentityChoicePending at turn 1, block 1 —
// after the operator's turn-0 SettingsAccepted so it folds last.
func injectPending(t *testing.T, mem memory.Memory, sess *spiceboxv1alpha1.AgentSession, uid string) {
	t.Helper()
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+1, lifecyclecore.IdentityChoicePending{})
}

// injectResolved appends the runner's IdentityChoiceResolved{mode} at block 2.
func injectResolved(t *testing.T, mem memory.Memory, sess *spiceboxv1alpha1.AgentSession, uid, mode string) {
	t.Helper()
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+2, lifecyclecore.IdentityChoiceResolved{Mode: mode})
}

// injectCancelled appends the runner's IdentityChoiceCancelled at block 2.
func injectCancelled(t *testing.T, mem memory.Memory, sess *spiceboxv1alpha1.AgentSession, uid string) {
	t.Helper()
	seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+2, lifecyclecore.IdentityChoiceCancelled{})
}

// driveIdentityChoice creates ac+sess, bootstraps to a Started steady state,
// invokes inject (the runner's actions, given the resolved session UID),
// reconciles once, and returns the fresh session. This is the brief's core
// helper; multi-step rows compose it with the finer helpers above.
func driveIdentityChoice(
	t *testing.T, ctx context.Context, env *testenv.Env, r *agentsession.Reconciler, mem memory.Memory,
	ac *spiceboxv1alpha1.AgentClass, sess *spiceboxv1alpha1.AgentSession,
	inject func(uid string),
) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	bootstrapSession(t, env, r, ac, sess)
	key := client.ObjectKeyFromObject(sess)
	created := loadSession(t, ctx, env, key)
	if inject != nil {
		inject(string(created.UID))
	}
	reconcileTimes(t, ctx, r, key, 1)
	return loadSession(t, ctx, env, key)
}

// setPodReady flips the runner Pod to Ready — Phase=Running with a Ready
// runner container — the kubelet-side edge reflectRunnerPod watches to fire
// RunnerClaimed (see controller.go's "Authority handoff ①"). The spy
// factory's Pod carries no status at all (no real kubelet in envtest), so
// every other row in this file never crosses this edge; this helper drives
// it explicitly to reproduce a live, healthy runner claiming the session.
func setPodReady(t *testing.T, ctx context.Context, env *testenv.Env, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}, &pod),
		"get runner pod to mark Ready")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner", Ready: true}}
	require.NoError(t, env.Client.Status().Update(ctx, &pod), "mark runner pod Ready")
}

// parkInteractive bootstraps an interactive session and drives it to a stable
// AwaitingIdentityChoice park (phase set by the fold, IdentityChoiceParkedAt
// stamped by the operator's deadline backstop). Returns the fresh session + UID.
func parkInteractive(
	t *testing.T, ctx context.Context, env *testenv.Env, r *agentsession.Reconciler, mem memory.Memory,
	ac *spiceboxv1alpha1.AgentClass, sess *spiceboxv1alpha1.AgentSession,
) (*spiceboxv1alpha1.AgentSession, string) {
	t.Helper()
	bootstrapSession(t, env, r, ac, sess)
	key := client.ObjectKeyFromObject(sess)
	created := loadSession(t, ctx, env, key)
	uid := string(created.UID)
	injectPending(t, mem, sess, uid)
	// R1: gate proceeds (phase still Pending); end-of-loop fold sets phase
	// AwaitingIdentityChoice. R2: gate observes the park and stamps ParkedAt.
	reconcileTimes(t, ctx, r, key, 3)
	parked := loadSession(t, ctx, env, key)
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, parked.Status.Phase,
		"session must be parked in AwaitingIdentityChoice after IdentityChoicePending")
	require.NotNil(t, parked.Status.IdentityChoiceParkedAt, "operator must stamp IdentityChoiceParkedAt on park")
	return parked, uid
}

// ---------------------------------------------------------------------------
// The state-graph test.
// ---------------------------------------------------------------------------

func TestIdentityChoiceStateGraph_Envtest(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Row 1: Pending(ask) + IdentityChoicePending ⇒ AwaitingIdentityChoice, runner pod exists.
	t.Run("Row1: IdentityChoicePending parks AwaitingIdentityChoice with the runner pod up", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row1-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row1", "row1-cls")
		parked, _ := parkInteractive(t, ctx, env, r, mem, ac, sess)

		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, parked.Status.Phase)
		assert.Empty(t, parked.Status.EffectiveIdentityMode, "E stays unset until the user chooses")
		assert.GreaterOrEqual(t, spy.startCount(), 1, "runner pod must have been Started (gate is a runner-spawning park)")
		assert.Zero(t, spy.stopCount(), "the operator keeps the runner alive in AwaitingIdentityChoice")

		// The pod literally exists in the API server.
		var pod corev1.Pod
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}, &pod),
			"runner pod must exist while parked in AwaitingIdentityChoice")
	})

	// Fast-follow I1: the runner Pod flips Ready (⇒ RunnerClaimed, folds
	// RunnerClaimed→Running) in the SAME reconcile the runner has already
	// appended IdentityChoicePending (folds→AwaitingIdentityChoice). Both Row 1
	// and the e2e harness use a Pod that never goes Ready, so this race — the
	// production case of a healthy runner parked on the identity choice — was
	// untested.
	t.Run("FF-I1: pod-Ready RunnerClaimed and IdentityChoicePending fold in the same reconcile", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("ffi1-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("ffi1", "ffi1-cls")
		bootstrapSession(t, env, r, ac, sess)
		key := client.ObjectKeyFromObject(sess)
		created := loadSession(t, ctx, env, key)
		uid := string(created.UID)

		// Precondition: bootstrap never crosses a real kubelet, so the Pod is
		// still statusless and RunnerReady is False/absent.
		require.False(t, conditions.IsTrue(created.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady),
			"precondition: RunnerReady must be False/absent before the Pod is marked Ready")

		// The runner appends IdentityChoicePending AND its Pod goes Ready before
		// the next reconcile observes either — reproducing the production race
		// where both land in the same fold.
		injectPending(t, mem, sess, uid)
		setPodReady(t, ctx, env, sess)

		// The transition reconcile: reflectRunnerPod flips RunnerReady on the
		// False→True edge and fires RunnerClaimed (mid-reconcile ProjectStatus
		// sets phase=Running); the end-of-loop fold then re-reads the full log.
		// RunnerClaimed anchors at (turn, SeqBlockStart); IdentityChoicePending
		// (seeded by injectPending) anchors at (turn, SeqBlockStart+1) — a
		// strictly higher Seq, so it folds AFTER RunnerClaimed and its
		// unconditional AwaitingIdentityChoice write wins the fold. Region-based
		// tiebreaking (RegionOperatorPre before RegionRunner) only applies at
		// EQUAL Seq, which does not happen here.
		reconcileTimes(t, ctx, r, key, 1)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"KEY FINDING: IdentityChoicePending's higher Seq outranks the same-turn RunnerClaimed — the fold lands on the park, not Running")

		// One more reconcile: the identity gate now reads phase==AwaitingIdentityChoice
		// at reconcile entry and stamps ParkedAt (mirrors parkInteractive's R2).
		reconcileTimes(t, ctx, r, key, 1)
		parked := loadSession(t, ctx, env, key)
		require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, parked.Status.Phase)
		require.NotNil(t, parked.Status.IdentityChoiceParkedAt,
			"operator must stamp IdentityChoiceParkedAt on park even though the runner Pod is already Ready")

		// The deadline backstop is reachable in the live-pod case too: back-date
		// the park past the timeout, exactly as Row 7 does, and confirm the same
		// Failed/IdentityChoiceTimeout outcome.
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			past := metav1.NewTime(time.Now().Add(-40 * time.Minute))
			s.IdentityChoiceParkedAt = &past
		})
		reconcileTimes(t, ctx, r, key, 1)

		final := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, final.Status.Phase,
			"the deadline backstop still fires with a Ready runner Pod")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceTimeout, final.Status.FailureReason)
		assert.NotNil(t, final.Status.FinishedAt, "FinishedAt stamped on the timeout")
	})

	// Row 2: AwaitingIdentityChoice + Resolved{agent} ⇒ phase advances past the park, E=agent.
	//
	// The runner emits IdentityChoiceResolved{agent} AND proceeds past the gate,
	// writing phase=Running via its status patcher (PatchProgress). The operator
	// folds the log (which unparks to Pending) to LEARN + mirror E=agent, and its
	// reconcilePhase preserves the runner-written Running against the stale
	// fold-Pending. We assert both facets: the fold unparked, and the CR advanced.
	t.Run("Row2: Resolved{agent} advances past AwaitingIdentityChoice with EffectiveIdentityMode=agent", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row2-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row2", "row2-cls")
		parked, uid := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		injectResolved(t, mem, sess, uid, spiceboxv1alpha1.IdentityModeAgent)
		// The runner proceeds past the gate on an agent choice and writes Running.
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			s.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
		})
		reconcileTimes(t, ctx, r, key, 1)

		got := loadSession(t, ctx, env, key)
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"phase must advance past AwaitingIdentityChoice once the user chooses agent")
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, got.Status.Phase,
			"operator preserves the runner-written Running")
		assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode,
			"operator mirrors the resolved agent choice onto status")
		// The operator's own fold of the signed log unparks to Pending — the choice
		// was applied, not stuck.
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhasePending, foldPhase(t, mem, sess.Namespace, sess.Name),
			"the signed log folds to Pending (unparked) after Resolved{agent}")
		_ = parked
	})

	// Row 3: AwaitingIdentityChoice + Resolved{userPassthrough} ⇒ E=userPassthrough,
	// the passthrough gate parks AwaitingCredentials AND reaps the gate runner
	// (Task-7 cred-park reap — the Task-7 coverage gap this row closes).
	t.Run("Row3: Resolved{userPassthrough} parks AwaitingCredentials and reaps the runner", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		// The provisional agent identity must resolve for a class that references
		// MCP servers — the operator requeues on a missing tool AgentIdentity
		// before it ever spawns the runner. A minimal CR is enough here.
		require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-identity", Namespace: "default"},
		}), "create AgentIdentity for Row 3")

		// A credentialed MCP target so the passthrough gate finds a missing
		// credential and parks (no linked UserIdentity for the starter).
		mcp := &spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "row3-mcp", Namespace: "default"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.example.com/mcp"},
				Tools:  []spiceboxv1alpha1.MCPServerTool{},
				Auth:   spiceboxv1alpha1.MCPServerAuth{Credential: "row3-oauth"},
			},
		}
		require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer for Row 3")

		ac := askClass("row3-cls", spiceboxv1alpha1.IdentityModeAsk, func(ac *spiceboxv1alpha1.AgentClass) {
			ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "row3-mcp", Ref: "row3-mcp"}}
		})
		sess := starterSession("row3", "row3-cls")
		_, uid := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		stopsBefore := spy.stopCount()
		injectResolved(t, mem, sess, uid, spiceboxv1alpha1.IdentityModeUserPassthrough)
		reconcileTimes(t, ctx, r, key, 1)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.IdentityModeUserPassthrough, got.Status.EffectiveIdentityMode,
			"operator mirrors the resolved userPassthrough choice")
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, got.Status.Phase,
			"a userPassthrough choice with missing creds re-parks AwaitingCredentials")
		assert.Greater(t, spy.stopCount(), stopsBefore,
			"the cred-park must reap the gate runner (AwaitingCredentials has no runner)")

		// The reap actually removed the pod.
		var pod corev1.Pod
		err := env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}, &pod)
		assert.True(t, apierrors.IsNotFound(err), "runner pod must be gone after the AwaitingCredentials reap")
	})

	// Row 6: AwaitingIdentityChoice + Cancelled ⇒ Failed/IdentityChoiceCancelled.
	//
	// The runner emits IdentityChoiceCancelled AND writes the terminal Failed
	// status (StatusPatcher.WriteFailed → reason IdentityChoiceCancelled) in the
	// same process. The operator must preserve that terminal state (not resurrect
	// it, not relabel it as the timeout backstop).
	t.Run("Row6: Cancelled ⇒ Failed with FailureReason=IdentityChoiceCancelled", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row6-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row6", "row6-cls")
		_, uid := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		injectCancelled(t, mem, sess, uid)
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			s.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
			s.FailureReason = spiceboxv1alpha1.ReasonIdentityChoiceCancelled
			now := metav1.Now()
			s.FinishedAt = &now
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type:   spiceboxv1alpha1.AgentSessionConditionFailed,
				Status: metav1.ConditionTrue,
				Reason: spiceboxv1alpha1.ReasonIdentityChoiceCancelled,
			})
		})
		reconcileTimes(t, ctx, r, key, 2)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "cancel is terminal Failed")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceCancelled, got.Status.FailureReason,
			"the operator must not relabel the runner's cancel reason")
		// The signed log itself folds to Failed with the cancel reason.
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, foldPhase(t, mem, sess.Namespace, sess.Name),
			"the signed log folds to Failed on IdentityChoiceCancelled")
	})

	// Row 7: AwaitingIdentityChoice + deadline elapsed, no decision ⇒
	// Failed/IdentityChoiceTimeout (operator backstop, no runner event).
	t.Run("Row7: choice deadline elapses ⇒ Failed/IdentityChoiceTimeout (operator backstop)", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row7-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row7", "row7-cls")
		parked, _ := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		// Backdate the park past the default 30m deadline (the gate reads real
		// wall-clock via time.Until, so we move ParkedAt, not an injected clock).
		require.NotNil(t, parked.Status.IdentityChoiceParkedAt)
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			past := metav1.NewTime(time.Now().Add(-40 * time.Minute))
			s.IdentityChoiceParkedAt = &past
		})
		reconcileTimes(t, ctx, r, key, 1)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "the deadline fails the session")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceTimeout, got.Status.FailureReason)
		assert.NotNil(t, got.Status.FinishedAt, "FinishedAt stamped on the timeout")
		// The operator (not a runner) appended exactly the timeout event.
		counts := identityEventCounts(t, mem, sess.Namespace, sess.Name)
		assert.Equal(t, 1, counts["IdentityChoiceTimeout"], "operator appends the backstop timeout event once")
		assert.Zero(t, counts["IdentityChoiceResolved"], "no decision was injected")
	})

	// Row 9: re-reconcile with no new event ⇒ phase stable, exactly ONE
	// IdentityChoicePending in the log (edge-gating: no operator re-append).
	t.Run("Row9: idle re-reconciles keep AwaitingIdentityChoice with exactly one IdentityChoicePending", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row9-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row9", "row9-cls")
		parked, _ := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		// Several more reconciles with no injected event.
		reconcileTimes(t, ctx, r, key, 4)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"phase is stable across idempotent re-reconciles")
		assert.Equal(t, parked.Status.IdentityChoiceParkedAt.Time, got.Status.IdentityChoiceParkedAt.Time,
			"ParkedAt is stamped once, not re-stamped each reconcile")
		counts := identityEventCounts(t, mem, sess.Namespace, sess.Name)
		assert.Equal(t, 1, counts["IdentityChoicePending"],
			"the operator never re-appends IdentityChoicePending (only the runner emits it, exactly once)")
	})

	// Row 10: the idle-sleep reaper never fires in AwaitingIdentityChoice —
	// the pod stays up (the sleep path is Idle-gated and unreachable here).
	t.Run("Row10: idle-sleep reaper does not reap a session parked in AwaitingIdentityChoice", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)
		// Aggressive sleep grace that WOULD reap were the phase Idle.
		r.DefaultSessionSleepAfter = time.Minute

		ac := askClass("row10-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := channelSession("row10", "row10-cls") // channel-attached so the sleep guard is otherwise satisfiable
		_, _ = parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		// Backdate LastIdleAt so a sleep WOULD be due if the phase were Idle.
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			old := metav1.NewTime(time.Now().Add(-30 * time.Minute))
			s.LastIdleAt = &old
		})
		stopsBefore := spy.stopCount()
		reconcileTimes(t, ctx, r, key, 2)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"the sleep reaper is Idle-gated; AwaitingIdentityChoice is untouched")
		assert.Equal(t, stopsBefore, spy.stopCount(), "no reap (Stop) while parked in AwaitingIdentityChoice")
		var pod corev1.Pod
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: agentsession.RunnerPodName(sess)}, &pod),
			"runner pod must survive the sleep-reconcile pass in AwaitingIdentityChoice")
	})

	// Row 12: static agent + static userPassthrough never enter
	// AwaitingIdentityChoice and get EffectiveIdentityMode mirrored from spec.
	t.Run("Row12: static agent mirrors E=agent and never enters AwaitingIdentityChoice", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := validClass("row12a-cls")
		ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeAgent
		sess := validSession("row12a", "row12a-cls")
		got := driveIdentityChoice(t, ctx, env, r, mem, ac, sess, nil)

		assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode, "spec mirrored to status")
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"a static-mode session never parks AwaitingIdentityChoice")
		counts := identityEventCounts(t, mem, sess.Namespace, sess.Name)
		assert.Zero(t, counts["IdentityChoicePending"], "no identity-choice events for a static class")
	})

	t.Run("Row12: static userPassthrough mirrors E=userPassthrough and never enters AwaitingIdentityChoice", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		// No MCP targets → no required credentials → the passthrough gate proceeds
		// without parking; the point is only that E mirrors and it never enters
		// AwaitingIdentityChoice.
		ac := validClass("row12b-cls")
		ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
		sess := starterSession("row12b", "row12b-cls")
		got := driveIdentityChoice(t, ctx, env, r, mem, ac, sess, nil)

		assert.Equal(t, spiceboxv1alpha1.IdentityModeUserPassthrough, got.Status.EffectiveIdentityMode, "spec mirrored to status")
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"a static userPassthrough session never parks AwaitingIdentityChoice")
	})

	// Row 13: an IdentityChoiceResolved after the session is terminal is a no-op
	// (terminal-sticky): the fold does not apply it, and the operator does not
	// resurrect the CR.
	t.Run("Row13: Resolved after Failed is a no-op (terminal-sticky)", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row13-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row13", "row13-cls")
		_, uid := parkInteractive(t, ctx, env, r, mem, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		// Runner cancels (event + terminal status), as in Row 6.
		injectCancelled(t, mem, sess, uid)
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			s.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
			s.FailureReason = spiceboxv1alpha1.ReasonIdentityChoiceCancelled
			now := metav1.Now()
			s.FinishedAt = &now
		})
		reconcileTimes(t, ctx, r, key, 1)
		require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, loadSession(t, ctx, env, key).Status.Phase, "precondition: Failed")

		// A late IdentityChoiceResolved{agent} arrives AFTER the terminal.
		seedLifecycleEvent(t, mem, sess.Namespace, sess.Name, uid, 1, channelevents.SeqBlockStart+3,
			lifecyclecore.IdentityChoiceResolved{Mode: spiceboxv1alpha1.IdentityModeAgent})
		reconcileTimes(t, ctx, r, key, 2)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase, "terminal is sticky: still Failed")
		assert.Equal(t, spiceboxv1alpha1.ReasonIdentityChoiceCancelled, got.Status.FailureReason, "reason unchanged")
		assert.NotEqual(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode,
			"a post-terminal Resolved must not set EffectiveIdentityMode")
		// The fold agrees: terminal-sticky drops the late Resolved.
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, foldPhase(t, mem, sess.Namespace, sess.Name),
			"the signed log stays Failed — the late Resolved is audit-only")
	})

	// Row 14: a session re-spawned with EffectiveIdentityMode already set skips
	// the gate — no new IdentityChoicePending — and proceeds normally.
	t.Run("Row14: E already set (re-spawn) skips the gate and proceeds normally", func(t *testing.T) {
		spy := &spyRunnerFactory{cl: env.Client}
		mem := memory.NewLocal(inmem.NewBackend())
		r := newIdentityReconciler(t, env, spy, mem)

		ac := askClass("row14-cls", spiceboxv1alpha1.IdentityModeAsk, nil)
		sess := validSession("row14", "row14-cls")
		bootstrapSession(t, env, r, ac, sess)
		key := client.ObjectKeyFromObject(sess)

		// Pre-populate the resolved mode (a prior choice survived a re-spawn).
		writeRunnerStatus(t, ctx, env, key, func(s *spiceboxv1alpha1.AgentSessionStatus) {
			s.EffectiveIdentityMode = spiceboxv1alpha1.IdentityModeAgent
		})
		reconcileTimes(t, ctx, r, key, 2)

		got := loadSession(t, ctx, env, key)
		assert.Equal(t, spiceboxv1alpha1.IdentityModeAgent, got.Status.EffectiveIdentityMode, "the pre-set choice is honored")
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice, got.Status.Phase,
			"a resolved re-spawn never re-enters AwaitingIdentityChoice")
		counts := identityEventCounts(t, mem, sess.Namespace, sess.Name)
		assert.Zero(t, counts["IdentityChoicePending"], "the gate is skipped: no new IdentityChoicePending")
	})
}
