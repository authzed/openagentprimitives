// pkg/controllers/agentsession/runner_refused_test.go
//
// A runner the cluster refuses to create must be a VISIBLE failure. Before
// this, the reconcile returned the refusal as a plain error: it reached the
// operator's log and nowhere else, so the session sat Pending with no
// condition, no message and no durable record of why — which is exactly what a
// person saw for five minutes when a namespace quota rejected their runner.
package agentsession_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// quotaRefusal is the apiserver's own words when a namespace ResourceQuota
// demands values a runner pod does not carry — the refusal a person actually
// hit.
const quotaRefusal = `failed quota: workshop-quota: must specify limits.cpu,limits.memory,requests.cpu,requests.memory`

// quotaRefusalErr builds the Forbidden error a namespace ResourceQuota admission
// verdict returns for podName — the one shape surfaceRunnerRefused must render
// as ReasonRunnerPodRefused.
func quotaRefusalErr(podName string) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, podName,
		assertableErr(quotaRefusal+" for: runner"))
}

// transientAPIServerErr is an unrelated Start failure — neither Forbidden nor
// Invalid — standing in for a transient apiserver hiccup. It must NOT be
// rendered with ReasonRunnerPodRefused's "not allowed the resources it needs"
// phrasing: nothing here refused the runner anything.
const transientAPIServerErr = "etcd request timed out"

// refusingReconciler builds a Reconciler over a fake client that refuses to
// Create the runner pod for session sessName with errForPodName(podName), and
// wires a real (in-memory) lifecycle log so the durable half is observable.
func refusingReconciler(t *testing.T, sessName string, errForPodName func(podName string) error, objs ...client.Object) (*agentsession.Reconciler, client.Client) {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme, networkingv1.AddToScheme)
	podName := agentsession.RunnerPodName(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: sessName},
	})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SpiceboxSession{}).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Name == podName {
					return errForPodName(podName)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &agentsession.Reconciler{
		Client:          c,
		APIReader:       c,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		LifecycleMemory: memory.NewLocal(inmem.NewBackend()),
		Now:             func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) },
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return r, c
}

// assertableErr wraps a plain message as an error, for apierrors.NewForbidden's
// cause argument.
type assertableErr string

func (e assertableErr) Error() string { return string(e) }

// refuseOnceReconciler builds a Reconciler over a fake client whose FIRST
// Create of the runner pod is refused with a quota Forbidden; every later
// Create for that same pod name succeeds normally. It drives the
// refused-then-recovered sequence live evidence showed: a quota that rejects
// one attempt and clears before the next.
func refuseOnceReconciler(t *testing.T, sessName string, objs ...client.Object) (*agentsession.Reconciler, client.Client) {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme, networkingv1.AddToScheme)
	podName := agentsession.RunnerPodName(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: sessName},
	})
	refused := false
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SpiceboxSession{}).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Name == podName && !refused {
					refused = true
					return quotaRefusalErr(podName)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &agentsession.Reconciler{
		Client:          c,
		APIReader:       c,
		Tokens:          tokens.NewRegistry(),
		Memory:          memory.NewLocal(inmem.NewBackend()),
		LifecycleMemory: memory.NewLocal(inmem.NewBackend()),
		Now:             func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) },
	}
	r.RunnerFactory = &agentsession.PodRunnerFactory{
		Client: c, RunnerImage: "runner:dev", OperatorURL: "http://op:8082", NATSURL: "nats://nats:4222",
	}
	return r, c
}

// TestReconcileRunnerRefused_RecoversWhenStartLaterSucceeds is the live-verified
// fix: pr-linear-linker-f0e35be0 was refused by a quota (RunnerReady=False/
// RunnerPodRefused, correct), the quota then freed and the runner pod was
// created and ran — but RunnerReady stayed stuck at False/RunnerPodRefused and
// status.runnerPodName stayed empty, telling a person the agent could not
// start when it was in fact running.
func TestReconcileRunnerRefused_RecoversWhenStartLaterSucceeds(t *testing.T) {
	ac := classWithBundle("ac4")
	sess := sessionCreatedAt("s4", "ac4", time.Date(2026, 9, 13, 11, 58, 0, 0, time.UTC))
	r, c := refuseOnceReconciler(t, "s4", ac, sess)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	sessKey := types.NamespacedName{Namespace: "default", Name: "s4"}

	// Drive to the refusal exactly like the other tests in this file.
	require.Error(t, reconcileUntilRunnerRefused(t, r, "s4"))
	got := getSession(t, c, "s4")
	rr := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr, "must start from a refused runner")
	require.Equal(t, spiceboxv1alpha1.ReasonRunnerPodRefused, rr.Reason, "must start from a refused runner")

	// The quota clears: the next reconcile's Start succeeds. The condition must
	// move OFF the stale refusal onto the same RunnerCreating a fresh start would
	// get, and the pod name must be recorded.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	require.NoError(t, err, "the recovery reconcile must not error")

	got = getSession(t, c, "s4")
	rr = apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr)
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRunnerCreating, rr.Reason,
		"a recovered Start must clear the stale refusal instead of leaving RunnerPodRefused standing")
	assert.NotEmpty(t, got.Status.RunnerPodName, "runnerPodName must be recorded once the pod is created")

	// The pod comes up and its runner container reports Ready: the next
	// reconcile must move RunnerReady all the way to True.
	var pod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: got.Status.RunnerPodName}, &pod),
		"Get the recovered runner pod")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner", Ready: true}}
	require.NoError(t, c.Status().Update(ctx, &pod), "stamp the runner pod Ready")

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: sessKey})
	require.NoError(t, err, "the ready-pod reconcile must not error")

	got = getSession(t, c, "s4")
	rr = apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr)
	assert.Equal(t, metav1.ConditionTrue, rr.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRunnerReady, rr.Reason)

	// The durable refusal event was recorded exactly once, on the original
	// refusal; the recovery must not append a second one.
	n := 0
	for _, ev := range lifecycleEvents(t, r, &got) {
		if _, ok := ev.(lifecyclecore.RunnerPodRefused); ok {
			n++
		}
	}
	assert.Equal(t, 1, n, "the refusal is recorded once; recovery must not add another")
}

// reconcileUntilRunnerRefused drives Reconcile until a pass returns the
// start-runner error, and returns it. Provisioning takes several passes before
// it reaches the runner at all, so this is a bounded loop rather than a fixed
// call count.
func reconcileUntilRunnerRefused(t *testing.T, r *agentsession.Reconciler, name string) error {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for i := 0; i < 10; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		if err != nil {
			return err
		}
	}
	t.Fatalf("no reconcile pass surfaced the runner refusal within 10 passes")
	return nil
}

func TestReconcileRunnerRefused_ConditionMessageAndDurableRecord(t *testing.T) {
	ac := classWithBundle("ac1")
	sess := sessionCreatedAt("s1", "ac1", time.Date(2026, 9, 13, 11, 58, 0, 0, time.UTC))
	r, c := refusingReconciler(t, "s1", quotaRefusalErr, ac, sess)

	err := reconcileUntilRunnerRefused(t, r, "s1")

	// The error still reaches the caller, so controller-runtime still retries.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start runner", "the reconcile error is preserved for retry")

	got := getSession(t, c, "s1")

	// 1. The condition a person's own surfaces read (webd's chat health watcher
	//    and channelsd's startup caption both rank RunnerReady) now carries the
	//    real cause instead of nothing at all.
	rr := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr, "RunnerReady must be stamped when the runner cannot be created")
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRunnerPodRefused, rr.Reason)
	assert.Contains(t, rr.Message, quotaRefusal, "the refusal's own words must survive onto the condition")

	// 2. The session stays Pending: a refusal is retried, not terminal.
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a refused runner is retried, never terminalized here")

	// 3. The durable half: the same words are in the session's signed log, which
	//    outlives both the CR's status and the operator's process.
	events := lifecycleEvents(t, r, &got)
	var refusals []lifecyclecore.RunnerPodRefused
	for _, ev := range events {
		if rp, ok := ev.(lifecyclecore.RunnerPodRefused); ok {
			refusals = append(refusals, rp)
		}
	}
	require.Len(t, refusals, 1, "exactly one refusal recorded, got events %v", events)
	assert.Contains(t, refusals[0].Message, quotaRefusal)
}

func TestReconcileRunnerRefused_RecordsOncePerRefusal(t *testing.T) {
	ac := classWithBundle("ac1")
	sess := sessionCreatedAt("s2", "ac1", time.Date(2026, 9, 13, 11, 58, 0, 0, time.UTC))
	r, c := refusingReconciler(t, "s2", quotaRefusalErr, ac, sess)

	require.Error(t, reconcileUntilRunnerRefused(t, r, "s2"))
	// Keep re-reconciling the way controller-runtime's retry does. The condition
	// is idempotent; the append-only log is not, and an unconditional append
	// would grow it without bound for as long as the refusal lasts.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for i := 0; i < 4; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s2"}})
		require.Error(t, err, "the refusal keeps erroring on every retry")
	}

	got := getSession(t, c, "s2")
	n := 0
	for _, ev := range lifecycleEvents(t, r, &got) {
		if _, ok := ev.(lifecyclecore.RunnerPodRefused); ok {
			n++
		}
	}
	assert.Equal(t, 1, n, "a standing refusal must be recorded once, not once per retry")
}

// TestReconcileRunnerRefused_NonAdmissionError_NeutralReason proves the other
// branch of the classification: a Start error that is NOT an admission
// verdict (apierrors.IsForbidden / IsInvalid) must not be rendered with
// ReasonRunnerPodRefused's "not allowed the resources it needs" phrasing —
// that would misattribute a transient apiserver hiccup to a quota or policy
// that was never involved. It still gets a condition, a durable record and a
// retry, exactly like the admission-refusal branch — only the reason and
// message differ.
func TestReconcileRunnerRefused_NonAdmissionError_NeutralReason(t *testing.T) {
	ac := classWithBundle("ac1")
	sess := sessionCreatedAt("s3", "ac1", time.Date(2026, 9, 13, 11, 58, 0, 0, time.UTC))
	r, c := refusingReconciler(t, "s3", func(string) error { return assertableErr(transientAPIServerErr) }, ac, sess)

	err := reconcileUntilRunnerRefused(t, r, "s3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start runner", "the reconcile error is preserved for retry")

	got := getSession(t, c, "s3")
	rr := apimeta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	require.NotNil(t, rr, "RunnerReady must be stamped even for a non-admission Start error")
	assert.Equal(t, metav1.ConditionFalse, rr.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonRunnerStartError, rr.Reason,
		"a non-admission error must NOT be reported as RunnerPodRefused")
	assert.True(t, strings.HasPrefix(rr.Message, "could not be started yet: "),
		"a neutral start error gets the neutral phrasing, not the refusal's own words alone")
	assert.Contains(t, rr.Message, transientAPIServerErr, "the underlying error's own words still survive")

	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, got.Status.Phase,
		"a start error is retried, never terminalized here")

	events := lifecycleEvents(t, r, &got)
	var refusals []lifecyclecore.RunnerPodRefused
	for _, ev := range events {
		if rp, ok := ev.(lifecyclecore.RunnerPodRefused); ok {
			refusals = append(refusals, rp)
		}
	}
	require.Len(t, refusals, 1, "exactly one record, got events %v", events)
	assert.Contains(t, refusals[0].Message, transientAPIServerErr)
}

// lifecycleEvents reads the session's signed transition log back.
func lifecycleEvents(t *testing.T, r *agentsession.Reconciler, sess *spiceboxv1alpha1.AgentSession) []lifecyclecore.Event {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	events, err := lifecyclekind.Events(ctx, r.LifecycleMemory,
		memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name})
	require.NoError(t, err, "read the session's lifecycle log")
	return events
}
