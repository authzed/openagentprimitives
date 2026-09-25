//go:build e2e

package sleep_wake_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestSleepWake_ReapsThenReprovisionsBeforeRunnerStarts exercises the
// idle-session sleep → wake round trip: a channel-attached AgentSession
// with a tiny spec.channels.sleepAfter (3s) has ALL its pods (both bundle
// SpiceboxSessions + the runner) reaped once it sits Idle past that grace,
// while the AgentSession CR itself stays Idle and wakeable. The next
// inbound message on the same channel/thread must re-provision both
// bundles AND wait for them to go Ready again before the runner is allowed
// to start — if the runner started too early it would race the sandbox
// pods and the second turn's tool-less reply would still succeed, but a
// regression that lets the runner start against torn-down bundles is
// exactly what turn 2's ExpectAgentReply is here to catch.
//
// The harness does not wire the SpiceboxToolspec controller, so this test
// stamps each Toolspec's Valid=True manually after Apply. It also stamps
// every bundle SpiceboxSession Ready=True as it appears (envtest has no Pod
// scheduler) — both on first boot and again after the wake re-creates them
// — unblocking the AgentSession reconciler past the BundlesReady gate each
// time.
func TestSleepWake_ReapsThenReprovisionsBeforeRunnerStarts(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		ExtraManifests:         []string{string(manifests)},
	})

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// 1. Stamp every SpiceboxToolspec Valid=True. The harness does not run
	// the SpiceboxToolspec controller, so without this the AgentClass
	// binding-coverage check rejects every bundle's toolspecs.
	stampToolspecsValid(t, ctx, h.K8s, "sw-ts-git", "sw-ts-codey")

	// 2. Background loop that stamps any bundle SpiceboxSession Ready=True
	// the AgentSession controller creates — on first boot AND again after
	// the wake re-creates them. The harness has no Pod scheduler, so
	// without this the bundle sessions sit Progressing forever and the
	// AgentSession stalls on BundlesReady=False.
	go func() {
		defer close(done)
		stampBundleSessionsReady(ctx, t, h.K8s)
	}()

	// 3. Script: respond_to_user("hello") on turn 1, respond_to_user("yes")
	// on turn 2 (post-wake), each followed by EndTurn once the runner
	// reports "delivered". The tool-result rule is Repeating — it fires for
	// both turns.
	h.LLM.OnUserMessage("hi").Reply(e2e.RespondToUser("hello"))
	h.LLM.OnUserMessage("still there?").Reply(e2e.RespondToUser("yes"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid("ac-sleep-wake", 30*time.Second)

	// 4. Turn 1: drive the session to Idle.
	h.SendUserMessage("hi")
	h.ExpectAgentReply(e2e.Contains("hello"))

	// 5. Locate the session + its two bundle SpiceboxSession names. Names
	// are deterministic (<session>-<bundle>) and status.BundleSessions
	// survives a reap (only the pods/CRs it points at are deleted), so
	// capturing them once here is enough for both the sleep and wake
	// assertions below.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions, client.InNamespace("default")),
		"list AgentSessions")
	require.NotEmpty(t, sessions.Items, "at least one AgentSession created by the channelsd pipeline")
	sessNS, sessName := sessions.Items[0].Namespace, sessions.Items[0].Name

	require.Eventually(t, func() bool {
		var sess spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: sessName}, &sess); err != nil {
			return false
		}
		return sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle && len(sess.Status.BundleSessions) == 2
	}, 15*time.Second, 200*time.Millisecond, "session must reach Idle with both bundles resolved before sleep can be observed")

	var idleSess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: sessName}, &idleSess))
	require.Len(t, idleSess.Status.BundleSessions, 2, "AgentClass declared two toolBundles")
	bundleNames := make([]string, 0, 2)
	for _, b := range idleSess.Status.BundleSessions {
		require.NotEmpty(t, b.SpiceboxSessionName, "bundle %q must have a resolved SpiceboxSession name", b.Name)
		bundleNames = append(bundleNames, b.SpiceboxSessionName)
	}

	// 6. Assert sleep: within sleepAfter (3s) + reconcile slack, the
	// operator reaps both bundle SpiceboxSessions while keeping the
	// AgentSession CR itself Idle and wakeable.
	require.Eventually(t, func() bool {
		for _, name := range bundleNames {
			var sbox spiceboxv1alpha1.SpiceboxSession
			err := h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: name}, &sbox)
			if !errors.IsNotFound(err) {
				return false
			}
		}
		return true
	}, 20*time.Second, 200*time.Millisecond, "both bundle SpiceboxSessions must be reaped after sleepAfter elapses")

	var sleptSess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: sessName}, &sleptSess))
	require.NotNil(t, sleptSess.Status.SleptAt, "SleptAt must be stamped once the session's pods are reaped")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseIdle, sleptSess.Status.Phase,
		"a slept session stays Idle, not some other phase")

	// 7. Turn 2 (same default thread → same session → wake). This is the
	// load-bearing assertion: it only passes if the runner waited for its
	// re-provisioned bundles to go Ready again before starting — a
	// regression in the wake-ordering fix would let the runner start
	// against torn-down/nonexistent sandbox pods.
	h.SendUserMessage("still there?")
	h.ExpectAgentReply(e2e.Contains("yes"))

	// 8. Bundles re-created (same deterministic names) and SleptAt cleared
	// now that the session is awake again.
	for _, name := range bundleNames {
		var sbox spiceboxv1alpha1.SpiceboxSession
		require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: name}, &sbox),
			"bundle SpiceboxSession %q must be re-created on wake", name)
	}
	var wokenSess spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: sessNS, Name: sessName}, &wokenSess))
	assert.Nil(t, wokenSess.Status.SleptAt, "SleptAt must be cleared once the session wakes")

	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec so the AgentClass binding-coverage check passes. Stamps
// once at test-setup time; the SpiceboxToolspec controller would normally
// re-affirm Valid on every reconcile, but in the harness no controller is
// watching, so a single status write is enough.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts),
			"get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions in the default
// namespace and stamps any without a Ready=True condition. Runs until ctx
// is canceled (test cleanup). Idempotent — touching a session that's
// already Ready is a no-op write. Fires repeatedly across the whole test
// lifetime, so it re-stamps the bundles the wake path re-creates after the
// sleep-reap just as it stamped the originals.
func stampBundleSessionsReady(ctx context.Context, t *testing.T, c client.Client) {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("stampBundleSessionsReady: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady) {
				continue
			}
			s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
				Type:               spiceboxv1alpha1.SpiceboxSessionConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonPodReady,
				Message:            "stamped Ready by e2e harness helper",
				LastTransitionTime: metav1.Now(),
			})
			if err := c.Status().Update(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Conflict on a concurrent reconcile is benign; the next
				// tick re-tries with a fresh ResourceVersion.
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}
