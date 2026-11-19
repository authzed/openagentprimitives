//go:build integration

package admitter

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/admission"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

const restartNamespace = "admission-restart"

// demandIn prices sessions in namespace ns with oneCPUDemand and every other
// session as needing nothing. The admitter lists cluster-wide, and envtest is
// shared: another test's sessions must not take this test's room.
func demandIn(ns string) func(context.Context, *spiceboxv1alpha1.AgentSession) (admission.Demand, error) {
	return func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (admission.Demand, error) {
		if sess.Namespace != ns {
			return admission.Demand{}, nil
		}
		return oneCPUDemand(ctx, sess)
	}
}

// startAdmitter runs one admitter in its own manager, as a fresh operator
// process would, and returns a stop func that blocks until the manager exits.
// Only sessions in ns are priced (demandIn).
func startAdmitter(t *testing.T, env *testenv.Env, ns string) (stop func()) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")
	require.NoError(t, (&Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Capacity:  roomFor(2),
		Enabled:   true,
		// envtest runs no AgentSession reconciler, so settings are never
		// resolved; every session needs the same one-CPU pod set.
		Demand: demandIn(ns),
	}).SetupWithManager(mgr), "admitter SetupWithManager")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager exited: %v", err)
		}
	}()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
	var once bool
	stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

// admittedLive lists the names of non-terminal sessions with Admitted=True,
// and every session ever admitted (terminal ones included), sorted.
func admittedLive(t *testing.T, c client.Client) (live, ever []string) {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(restartNamespace)), "list sessions")
	for i := range list.Items {
		s := &list.Items[i]
		if !admission.IsAdmitted(s) {
			continue
		}
		ever = append(ever, s.Name)
		if !isTerminal(s) {
			live = append(live, s.Name)
		}
	}
	slices.Sort(live)
	slices.Sort(ever)
	return live, ever
}

// waitForAdmitted polls until exactly want sessions have ever been admitted.
func waitForAdmitted(t *testing.T, c client.Client, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var ever []string
	for time.Now().Before(deadline) {
		_, ever = admittedLive(t, c)
		if len(ever) >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("want %d sessions admitted within 30s, got %v", want, ever)
}

// assertStaysAdmitted asserts over a window that the admitted set never grows
// past wantEver and the live holders never exceed the room.
func assertStaysAdmitted(t *testing.T, c client.Client, wantLive, wantEver []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		live, ever := admittedLive(t, c)
		if !assert.Equal(t, wantLive, live, "live holders") || !assert.Equal(t, wantEver, ever, "ever admitted") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestAdmitterRestartRebuildsLine is the restart property: the line lives in
// AgentSession status, not in the admitter's memory, so a new operator
// process continues it exactly and never double-admits.
//
// Sessions are created s1..s5 in order. creationTimestamp is to the second, so
// several share a join time and the tie breaks on namespace/name, which is
// the same order.
func TestAdmitterRestartRebuildsLine(t *testing.T) {
	env := testenv.Shared(t)
	c := env.Client
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: restartNamespace}}); !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create namespace")
	}

	stop := startAdmitter(t, env, restartNamespace)
	for i := 1; i <= 5; i++ {
		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("s%d", i), Namespace: restartNamespace},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Class:  testClass,
				Prompt: spiceboxv1alpha1.PromptSource{Inline: "review this"},
			},
		}
		require.NoError(t, c.Create(ctx, sess), "create s%d", i)
		// envtest runs no AgentSession reconciler: put the session in line the
		// way its admission gate does, Admitted=False/Queued at arrival.
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
		meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonAdmissionQueued, Message: "waiting in line",
			LastTransitionTime: sess.CreationTimestamp,
		})
		require.NoError(t, c.Status().Update(ctx, sess), "queue s%d", i)
	}

	waitForAdmitted(t, c, 2)
	assertStaysAdmitted(t, c, []string{"s1", "s2"}, []string{"s1", "s2"})
	stop()

	startAdmitter(t, env, restartNamespace)
	// Release a holder the way a finished session does: it goes terminal.
	var s1 spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: restartNamespace, Name: "s1"}, &s1), "get s1")
	s1.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	require.NoError(t, c.Status().Update(ctx, &s1), "mark s1 Succeeded")

	waitForAdmitted(t, c, 3)
	assertStaysAdmitted(t, c, []string{"s2", "s3"}, []string{"s1", "s2", "s3"})

	var s4 spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: restartNamespace, Name: "s4"}, &s4), "get s4")
	require.NotNil(t, s4.Status.Admission, "s4 is positioned in line")
	assert.Equal(t, int32(1), s4.Status.Admission.Position, "s4 heads the line")
	assert.Equal(t, int32(2), s4.Status.Admission.LineLength, "two still waiting")
}

const adoptNamespace = "admission-adopt"

// TestAdmitterAdoptsPreAdmissionSessionsEnvtest is the rollout property
// against a real apiserver: sessions whose pods were created before admission
// was switched on — a Running one with a bound runner pod, a Pending one
// mid-boot with an unbound sandbox pod under a warm-pool name — carry no
// Admitted condition. The admitter adopts both (Admitted=True/Adopted) and
// counts them as holders: the Pending one's unplaced pod takes one of the two
// CPUs, so of two queued sessions exactly one is admitted.
func TestAdmitterAdoptsPreAdmissionSessionsEnvtest(t *testing.T) {
	env := testenv.Shared(t)
	c := env.Client
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: adoptNamespace}}); !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create namespace")
	}

	create := func(name string, mutate func(s *spiceboxv1alpha1.AgentSession)) {
		t.Helper()
		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: adoptNamespace},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Class:  testClass,
				Prompt: spiceboxv1alpha1.PromptSource{Inline: "review this"},
			},
		}
		require.NoError(t, c.Create(ctx, sess), "create %s", name)
		mutate(sess)
		require.NoError(t, c.Status().Update(ctx, sess), "status %s", name)
	}
	pod := func(name string, labels map[string]string, node string) {
		t.Helper()
		require.NoError(t, c.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: adoptNamespace, Labels: labels},
			Spec: corev1.PodSpec{
				NodeName:   node,
				Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
			},
		}), "create pod %s", name)
	}

	create("old-running", func(s *spiceboxv1alpha1.AgentSession) {
		s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
		s.Status.RunnerPodName = "old-running-runner"
	})
	pod("old-running-runner", map[string]string{sessionPodLabel: "old-running"}, "node-a")
	create("old-booting", func(s *spiceboxv1alpha1.AgentSession) {
		s.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
		s.Status.BundleSessions = []spiceboxv1alpha1.ResolvedBundle{{Name: "code", SpiceboxSessionName: "old-booting-code"}}
	})
	pod("warmpool-x7k2p", map[string]string{sandboxPodLabel: "old-booting-code"}, "")
	for _, name := range []string{"new-a", "new-b"} {
		create(name, func(s *spiceboxv1alpha1.AgentSession) {
			s.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type: spiceboxv1alpha1.AgentSessionConditionAdmitted, Status: metav1.ConditionFalse,
				Reason: spiceboxv1alpha1.ReasonAdmissionQueued, Message: "waiting in line",
				LastTransitionTime: s.CreationTimestamp,
			})
		})
	}

	startAdmitter(t, env, adoptNamespace)

	reasons := func() map[string]string {
		var list spiceboxv1alpha1.AgentSessionList
		require.NoError(t, c.List(ctx, &list, client.InNamespace(adoptNamespace)), "list sessions")
		out := map[string]string{}
		for i := range list.Items {
			if cond := meta.FindStatusCondition(list.Items[i].Status.Conditions, spiceboxv1alpha1.AgentSessionConditionAdmitted); cond != nil && cond.Status == metav1.ConditionTrue {
				out[list.Items[i].Name] = cond.Reason
			}
		}
		return out
	}
	deadline := time.Now().Add(30 * time.Second)
	var got map[string]string
	for time.Now().Before(deadline) {
		got = reasons()
		if len(got) >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionAdopted, got["old-running"], "running pre-admission session adopted")
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmissionAdopted, got["old-booting"], "booting pre-admission session (pool-named pod) adopted")
	assert.Equal(t, spiceboxv1alpha1.ReasonAdmitted, got["new-a"], "the oldest queued session takes the free CPU")

	// Over a window, the adopted holders keep new-b out.
	window := time.Now().Add(2 * time.Second)
	for time.Now().Before(window) {
		if !assert.NotContains(t, reasons(), "new-b", "new-b stays in line: the adopted holders count") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
