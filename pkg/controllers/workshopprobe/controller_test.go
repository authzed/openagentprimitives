package workshopprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// proberFunc adapts a plain function to the Prober interface, so a test can
// inject a fake without a live pod ever running — the same adaptor shape the
// brief's own TestWorkshopProbe_NoTuple_DeniedBeforeProbe snippet uses.
type proberFunc func(ctx context.Context, wp *v1alpha1.WorkshopProbe) (ProbeResult, error)

func (f proberFunc) Probe(ctx context.Context, wp *v1alpha1.WorkshopProbe) (ProbeResult, error) {
	return f(ctx, wp)
}

// fakeBuildChecker is a WorkshopBuildChecker whose answer a test picks. It
// counts calls so a test can prove the tuple check ran (or, more
// importantly, that a denial path is reached WITHOUT ever asking it — see
// the missing-labels cases, which must deny before the checker is even
// consulted).
type fakeBuildChecker struct {
	allowed bool
	err     error
	calls   int
}

func (f *fakeBuildChecker) CheckWorkshopBuild(_ context.Context, _, _, _ string) (bool, error) {
	f.calls++
	return f.allowed, f.err
}

// probedConditionReason returns the WorkshopProbeConditionProbed condition's
// Reason, or "" if the condition was never set.
func probedConditionReason(wp *v1alpha1.WorkshopProbe) string {
	for _, c := range wp.Status.Conditions {
		if c.Type == v1alpha1.WorkshopProbeConditionProbed {
			return c.Reason
		}
	}
	return ""
}

// testNamespace builds the workshop namespace W a WorkshopProbe lives in,
// carrying the two session-attribution labels the Workshop controller
// stamps at provisioning (BuildWorkshopNamespace, pkg/controllers/workshop/
// rbac.go) — nil/empty entries in labels omit that label, letting tests
// exercise "missing one" and "missing both" fail-closed cases.
func testNamespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// testWorkshop builds the Workshop CR the reconciler reads for
// spec.limits.maxConcurrentProbes — living beside the AgentSession at
// {sessNS, WorkshopName(sessName)}, exactly as pkg/controllers/workshop
// creates it.
func testWorkshop(sessNS, sessName string, maxConcurrentProbes int32) *v1alpha1.Workshop {
	return &v1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sessNS,
			Name:      v1alpha1.WorkshopName(sessName),
		},
		Spec: v1alpha1.WorkshopSpec{
			Session:        v1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName},
			SidecarToolbox: "workshop",
			Limits: v1alpha1.WorkshopLimits{
				MaxObjectsPerKind:   20,
				MaxObjects:          100,
				MaxConcurrentProbes: maxConcurrentProbes,
			},
		},
	}
}

// testProbe builds a minimal image-mode WorkshopProbe in namespace ns.
func testProbe(ns, name string) *v1alpha1.WorkshopProbe {
	return &v1alpha1.WorkshopProbe{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.WorkshopProbeSpec{
			Image:          "ghcr.io/example/probe:v1",
			TimeoutSeconds: 120,
		},
	}
}

func getProbe(t *testing.T, c client.Client, ns, name string) *v1alpha1.WorkshopProbe {
	t.Helper()
	var got v1alpha1.WorkshopProbe
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got))
	return &got
}

func TestReconcile_WorkshopProbeNotFound_ReturnsNoError(t *testing.T) {
	c := newFakeClient(t)
	r := &Reconciler{Client: c}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ws-missing", Name: "gone"}})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

func TestReconcile_TerminalPhase_NeverReprobes(t *testing.T) {
	for _, phase := range []string{v1alpha1.WorkshopProbePhaseSucceeded, v1alpha1.WorkshopProbePhaseFailed} {
		t.Run(phase+": already terminal, Reconcile is a no-op and the Prober never runs", func(t *testing.T) {
			c := newFakeClient(t)
			ctx := context.Background()

			wp := testProbe("ws-terminal-"+phase, "probe-1")
			require.NoError(t, c.Create(ctx, wp))
			wp.Status.Phase = phase
			require.NoError(t, c.Status().Update(ctx, wp))

			called := false
			r := &Reconciler{
				Client: c,
				Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
					called = true
					return ProbeResult{}, nil
				}),
			}
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, res)
			assert.False(t, called, "a terminal WorkshopProbe must never re-run its probe")
		})
	}
}

// TestReconcile_DeniedBeforeProbe is the fake-client-level companion to the
// security-critical integration test: every one of these cases must reach
// phase Failed WITHOUT the Prober ever being called — a missing session
// label, a denied tuple check, an erroring tuple check (fail CLOSED, not
// open), and an entirely unwired checker all take the same "no probe pod
// today" path.
func TestReconcile_DeniedBeforeProbe(t *testing.T) {
	fullLabels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-1",
	}

	cases := []struct {
		name       string
		nsLabels   map[string]string
		build      WorkshopBuildChecker
		wantReason string
	}{
		{
			name:       "workshop namespace carries neither session label: phase Failed/ProbeFailed, Prober never runs",
			nsLabels:   map[string]string{},
			build:      &fakeBuildChecker{allowed: true},
			wantReason: v1alpha1.ReasonWorkshopProbeFailed,
		},
		{
			name:       "workshop namespace missing session-namespace label only: phase Failed/ProbeFailed, Prober never runs",
			nsLabels:   map[string]string{v1alpha1.LabelWorkshopSessionName: "builder-1"},
			build:      &fakeBuildChecker{allowed: true},
			wantReason: v1alpha1.ReasonWorkshopProbeFailed,
		},
		{
			name:       "workshop namespace missing session-name label only: phase Failed/ProbeFailed, Prober never runs",
			nsLabels:   map[string]string{v1alpha1.LabelWorkshopSessionNamespace: "default"},
			build:      &fakeBuildChecker{allowed: true},
			wantReason: v1alpha1.ReasonWorkshopProbeFailed,
		},
		{
			name:       "tuple check denies: phase Failed/ProbeDenied, Prober never runs",
			nsLabels:   fullLabels,
			build:      &fakeBuildChecker{allowed: false},
			wantReason: v1alpha1.ReasonWorkshopProbeDenied,
		},
		{
			name:       "tuple check errors: phase Failed/ProbeDenied (fail closed, not treated as allow), Prober never runs",
			nsLabels:   fullLabels,
			build:      &fakeBuildChecker{err: errors.New("spicedb unreachable")},
			wantReason: v1alpha1.ReasonWorkshopProbeDenied,
		},
		{
			name:       "no WorkshopBuildChecker wired: phase Failed/ProbeDenied, Prober never runs",
			nsLabels:   fullLabels,
			build:      nil,
			wantReason: v1alpha1.ReasonWorkshopProbeDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClient(t)
			ctx := context.Background()

			const wsNamespace = "ws-denied"
			require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, tc.nsLabels)))
			wp := testProbe(wsNamespace, "probe-1")
			require.NoError(t, c.Create(ctx, wp))

			called := false
			r := &Reconciler{
				Client: c,
				Build:  tc.build,
				Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
					called = true
					return ProbeResult{}, nil
				}),
			}
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
			require.NoError(t, err, "a refused probe is a recorded result, not a reconcile error")
			assert.Equal(t, ctrl.Result{}, res)
			assert.False(t, called, "the probe must never run once the gate refuses it")

			got := getProbe(t, c, wp.Namespace, wp.Name)
			assert.Equal(t, v1alpha1.WorkshopProbePhaseFailed, got.Status.Phase)
			assert.Equal(t, tc.wantReason, probedConditionReason(got))
			assert.NotEmpty(t, got.Status.PodFailure, "the refusal must be recorded on status.podFailure")

			if fb, ok := tc.build.(*fakeBuildChecker); ok && len(tc.nsLabels) == 2 {
				assert.Equal(t, 1, fb.calls, "the tuple check must actually run once labels are present")
			}
		})
	}
}

func TestReconcile_ConcurrencyCap_RequeuesWithoutProbing(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-cap"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-cap",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-cap", 1)))

	// A slot is already taken by another, unrelated, non-terminal probe.
	already := testProbe(wsNamespace, "probe-already-running")
	require.NoError(t, c.Create(ctx, already))
	already.Status.Phase = v1alpha1.WorkshopProbePhaseRunning
	require.NoError(t, c.Status().Update(ctx, already))

	wp := testProbe(wsNamespace, "probe-blocked")
	require.NoError(t, c.Create(ctx, wp))

	called := false
	r := &Reconciler{
		Client: c,
		Build:  &fakeBuildChecker{allowed: true},
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			called = true
			return ProbeResult{Tools: []v1alpha1.WorkshopProbedTool{{Name: "should-not-run"}}}, nil
		}),
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter.Nanoseconds(), int64(0), "an at-cap probe must requeue rather than run immediately")
	assert.False(t, called, "the Prober must never run while every slot is taken")

	got := getProbe(t, c, wp.Namespace, wp.Name)
	assert.NotEqual(t, v1alpha1.WorkshopProbePhaseFailed, got.Status.Phase, "a cap-blocked probe is not refused — it will run once a slot frees")
	assert.NotEqual(t, v1alpha1.WorkshopProbePhaseSucceeded, got.Status.Phase)
}

// TestReconcile_ConcurrencyCap_PendingProbesDoNotDeadlockEachOther is the
// regression for the counting bug: N probes created together (Phase == "",
// non-terminal, but never yet ADMITTED) with cap M < N must not all count
// each other as "in flight" — only a probe actually Phase == Running does.
// Before the fix, every one of these probes' cap check counted its equally-
// pending siblings as non-terminal, so ALL of them would requeue and NONE
// would ever reach the Prober: a permanent livelock on exactly the scenario
// (pending > cap) the cap exists to handle.
func TestReconcile_ConcurrencyCap_PendingProbesDoNotDeadlockEachOther(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-burst"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-burst",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-burst", 1))) // cap M = 1

	// N = 3 probes, all freshly created — Phase == "", non-terminal, never
	// admitted — the burst the cap exists to bound.
	names := []string{"probe-a", "probe-b", "probe-c"}
	for _, name := range names {
		require.NoError(t, c.Create(ctx, testProbe(wsNamespace, name)))
	}

	calls := 0
	r := &Reconciler{
		Client: c,
		Build:  &fakeBuildChecker{allowed: true},
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			calls++
			return ProbeResult{Tools: []v1alpha1.WorkshopProbedTool{{Name: "tool"}}}, nil
		}),
	}

	// Reconcile the FIRST probe while its two siblings are STILL Phase=="":
	// under the old (buggy) counting this alone would see 2 non-terminal
	// siblings >= cap(1) and block, proving the fix does not depend on how
	// many of the LATER probes also happen to complete.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wsNamespace, Name: names[0]}})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, `the FIRST probe must run: its still-pending (Phase=="") siblings must not count against the cap`)

	// Reconcile the rest of the burst too — it must be able to drain
	// entirely, not just admit the first arrival.
	for _, name := range names[1:] {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wsNamespace, Name: name}})
		require.NoError(t, err)
	}
	assert.Greater(t, calls, 0, "at least one probe in the burst must reach the Prober — zero is the deadlock this test guards against")
	assert.Equal(t, len(names), calls, "with a Prober that completes synchronously, every probe in the burst drains once pending siblings stop counting against the cap")
}

// TestReconcile_Success_RecordsToolsAndSucceeds also pins self-exclusion
// from the concurrency count: maxConcurrentProbes is 1, and the ONLY
// WorkshopProbe in the namespace is the one being reconciled — if the
// reconciler counted itself among the "running" probes, this would wrongly
// requeue on a fresh cluster's very first probe instead of ever running it.
func TestReconcile_Success_RecordsToolsAndSucceeds(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-success"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-ok",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-ok", 1)))

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, c.Create(ctx, wp))

	build := &fakeBuildChecker{allowed: true}
	wantTools := []v1alpha1.WorkshopProbedTool{
		{Name: "read_file", Description: "reads a file"},
		{Name: "write_file", Description: "writes a file"},
	}
	r := &Reconciler{
		Client: c,
		Build:  build,
		Prober: proberFunc(func(_ context.Context, gotWP *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			assert.Equal(t, wp.Name, gotWP.Name, "the Prober must receive the probe being reconciled")
			return ProbeResult{Tools: wantTools, ResolvedDigest: "sha256:deadbeef"}, nil
		}),
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Equal(t, 1, build.calls)

	got := getProbe(t, c, wp.Namespace, wp.Name)
	assert.Equal(t, v1alpha1.WorkshopProbePhaseSucceeded, got.Status.Phase)
	assert.Equal(t, wantTools, got.Status.Tools)
	assert.Equal(t, "sha256:deadbeef", got.Status.ResolvedDigest)
	assert.Empty(t, got.Status.PodFailure)
	assert.Equal(t, v1alpha1.ReasonWorkshopProbeSucceeded, probedConditionReason(got))
}

func TestReconcile_ProbeResultPodFailure_MarksFailed(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-podfailure"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-pf",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-pf", 2)))

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, c.Create(ctx, wp))

	r := &Reconciler{
		Client: c,
		Build:  &fakeBuildChecker{allowed: true},
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			return ProbeResult{PodFailure: "probe pod never became ready", ResolvedDigest: "sha256:cafe"}, nil
		}),
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err, "a PodFailure result is a recorded outcome, not a reconcile error")

	got := getProbe(t, c, wp.Namespace, wp.Name)
	assert.Equal(t, v1alpha1.WorkshopProbePhaseFailed, got.Status.Phase)
	assert.Equal(t, "probe pod never became ready", got.Status.PodFailure)
	assert.Equal(t, "sha256:cafe", got.Status.ResolvedDigest, "a digest resolved alongside a PodFailure must still be recorded")
	assert.Equal(t, v1alpha1.ReasonWorkshopProbeFailed, probedConditionReason(got))
}

func TestReconcile_ProberInfraError_ReturnsErrorAndStaysRunning(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-infra-error"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-err",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-err", 2)))

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, c.Create(ctx, wp))

	r := &Reconciler{
		Client: c,
		Build:  &fakeBuildChecker{allowed: true},
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			return ProbeResult{}, errors.New("apply probe NetworkPolicy: connection refused")
		}),
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.Error(t, err, "an infrastructure fault from the Prober must be returned so the reconcile retries")

	got := getProbe(t, c, wp.Namespace, wp.Name)
	assert.Equal(t, v1alpha1.WorkshopProbePhaseRunning, got.Status.Phase, "left Running so the retry re-enters and re-probes")
}

func TestReconcile_ProberNotWired_ReturnsError(t *testing.T) {
	c := newFakeClient(t)
	ctx := context.Background()

	const wsNamespace = "ws-no-prober"
	labels := map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: "default",
		v1alpha1.LabelWorkshopSessionName:      "builder-np",
	}
	require.NoError(t, c.Create(ctx, testNamespace(wsNamespace, labels)))
	require.NoError(t, c.Create(ctx, testWorkshop("default", "builder-np", 2)))

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, c.Create(ctx, wp))

	r := &Reconciler{Client: c, Build: &fakeBuildChecker{allowed: true}} // Prober deliberately unset
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.Error(t, err, "an unwired Prober must error loudly, never panic on a nil interface call")
}
