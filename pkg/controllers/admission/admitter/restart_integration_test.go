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

// startAdmitter runs one admitter in its own manager, as a fresh operator
// process would, and returns a stop func that blocks until the manager exits.
func startAdmitter(t *testing.T, env *testenv.Env) (stop func()) {
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
		Demand: oneCPUDemand,
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

	stop := startAdmitter(t, env)
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

	startAdmitter(t, env)
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
