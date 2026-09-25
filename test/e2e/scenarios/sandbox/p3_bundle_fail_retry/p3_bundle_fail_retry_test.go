//go:build e2e

package p3_bundle_fail_retry_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestP3_BundleFailure_RetriesOnceThenTerminal drives the retry ladder
// through the real controllers: a bundle SpiceboxSession stamped Failed
// mid-session is deleted + recreated once (session survives); the
// replacement failing is terminal BundleFailed.
func TestP3_BundleFailure_RetriesOnceThenTerminal(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{string(manifests)},
	})

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// 1. Stamp the SpiceboxToolspec Valid=True. The harness does not run
	// the SpiceboxToolspec controller, so without this the AgentClass
	// binding-coverage check rejects the bundle's toolspec.
	stampToolspecsValid(t, ctx, h.K8s, "ts-code")

	// 2. Background loop that stamps any bundle SpiceboxSession Ready=True
	// the AgentSession controller creates — EXCEPT sessions the test has
	// deliberately failed (or that are being deleted). Without the
	// Failed-skip guard the background loop races the test's own
	// stampBundleFailed writes and flips the condition back to Ready
	// before the controller observes the failure.
	go func() {
		defer close(done)
		stampBundleSessionsReadySkipFailed(ctx, t, h.K8s)
	}()

	// 3. Script: respond_to_user("hello") on the first user turn. The
	// AgentSession controller only (re)provisions/observes bundle
	// SpiceboxSessions while Phase is Pending or Running
	// (sandboxProvisioningDesired) — a completed turn parks the session
	// at Idle and the whole bundle-failure gate goes silent, which would
	// make the deliberately-Failed bundle sit forever unobserved. So the
	// respond_to_user tool_result's reply is a closure that blocks
	// (holding the ScriptedLLM lock, which is fine — this test has only
	// one in-flight session) until endTurn is released below, keeping
	// the runner's turn — and therefore the AgentSession's Phase=Running —
	// alive for the whole retry-ladder exercise.
	var endTurn atomic.Bool
	// Guarantee the release fires even if a require.* below aborts the test
	// body via runtime.Goexit — without this, an earlier assertion failure
	// would leave the ReplyFn goroutine spinning on endTurn.Load() forever,
	// holding the ScriptedLLM lock, turning a fast test regression into a
	// 15-minute -timeout hang.
	t.Cleanup(func() { endTurn.Store(true) })
	h.LLM.OnUserMessage("hi").Reply(e2e.RespondToUser("hello"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).ReplyFn(func() []e2e.ReplyPart {
		for !endTurn.Load() {
			time.Sleep(50 * time.Millisecond)
		}
		return []e2e.ReplyPart{e2e.EndTurn()}
	})

	h.WaitForAgentClassValid("retry-agent", 30*time.Second)
	h.SendUserMessage("hi")
	h.ExpectAgentReply(e2e.Contains("hello"))

	// 4. Locate the AgentSession the channelsd pipeline created.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessions, client.InNamespace("default")),
		"list AgentSessions")
	require.NotEmpty(t, sessions.Items, "at least one AgentSession created by the channelsd pipeline")
	sess := sessions.Items[0]
	bundleName := sess.Name + "-code"

	// 5. Wait for the bundle SpiceboxSession to exist and the AgentSession
	// to reach BundlesReady=True — both should already hold by the time
	// ExpectAgentReply returned (the runner cannot have replied otherwise),
	// but assert explicitly for a clear failure signal if that changes.
	var bundle spiceboxv1alpha1.SpiceboxSession
	eventually(t, 10*time.Second, func() bool {
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: bundleName}, &bundle); err != nil {
			return false
		}
		var got spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: sess.Name}, &got); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionBundlesReady)
	})

	// --- Failure #1: retry, not terminal. ---
	firstUID := stampBundleFailed(t, ctx, h.K8s, bundleName)

	var afterFirst spiceboxv1alpha1.AgentSession
	eventually(t, 30*time.Second, func() bool {
		var fresh spiceboxv1alpha1.SpiceboxSession
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: bundleName}, &fresh); err != nil {
			return false
		}
		// The controller's SSA apply recreates the bundle CR after
		// deleting the failed instance — key on UID change, not mere
		// existence, since Get succeeds against either instance.
		if fresh.UID == firstUID {
			return false
		}
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: sess.Name}, &afterFirst); err != nil {
			return false
		}
		return len(afterFirst.Status.BundleSessions) > 0 && afterFirst.Status.BundleSessions[0].Restarts == 1
	})
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, afterFirst.Status.Phase,
		"first bundle failure must NOT terminally fail the session")
	require.Len(t, afterFirst.Status.BundleSessions, 1, "bundle status entry")
	assert.Equal(t, int32(1), afterFirst.Status.BundleSessions[0].Restarts, "retry recorded")
	assert.Equal(t, string(firstUID), afterFirst.Status.BundleSessions[0].RetriedSessionUID,
		"retried instance UID recorded")

	// --- Failure #2 (replacement instance): terminal. ---
	stampBundleFailed(t, ctx, h.K8s, bundleName)

	var afterSecond spiceboxv1alpha1.AgentSession
	// Wait for the Failed CONDITION as well as the phase. Waiting on the phase
	// alone and then reading the condition assumes the two land in the same
	// status write; when they do not, this reads a session that is already
	// Failed but whose condition has not been recorded yet, and the require
	// below nils out. The assertions want the whole terminal state, so the
	// wait must describe the whole terminal state.
	eventually(t, 30*time.Second, func() bool {
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: sess.Name}, &afterSecond); err != nil {
			return false
		}
		return afterSecond.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed &&
			conditionByType(&afterSecond, spiceboxv1alpha1.AgentSessionConditionFailed) != nil
	})
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBundleFail, afterSecond.Status.FailureReason, "failureReason")
	failedCond := conditionByType(&afterSecond, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, failedCond, "Failed condition present")
	assert.Contains(t, failedCond.Message, "after 1 retry", "message notes the retry happened")

	// Release the blocked turn so the runner's Send call returns and
	// AssertAllRulesConsumed (which needs the ScriptedLLM lock the
	// blocked call holds) doesn't wait forever.
	endTurn.Store(true)
	h.AssertAllRulesConsumed()
}

// eventually polls fn until it returns true or d elapses, failing the test
// on timeout.
func eventually(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %v", d)
}

// conditionByType returns the named condition from an AgentSession's
// status, or nil if absent.
func conditionByType(s *spiceboxv1alpha1.AgentSession, condType string) *metav1.Condition {
	for i := range s.Status.Conditions {
		if s.Status.Conditions[i].Type == condType {
			return &s.Status.Conditions[i]
		}
	}
	return nil
}

// stampBundleFailed marks the named bundle SpiceboxSession Failed=PodCrashed,
// mirroring what the SpiceboxSession controller writes when its sandbox
// backend reports sandboxkinds.PhaseFailed (and what the integration suite's
// stampBundleFailed does). Returns the stamped object's UID so the test can
// distinguish the original instance from its replacement.
func stampBundleFailed(t *testing.T, ctx context.Context, c client.Client, name string) types.UID {
	t.Helper()
	// The SpiceboxSession controller is live in this harness and reconciles the
	// same object, so a bare read-modify-write races it: the resourceVersion
	// moves between the Get and the Update, and the write is rejected as a
	// conflict. Invisible on an idle machine, reproducible under a loaded
	// suite. Re-Get inside the retry so every attempt writes against the
	// version it just read.
	var uid types.UID
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var bs spiceboxv1alpha1.SpiceboxSession
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &bs); err != nil {
			return err
		}
		bs.Status.Conditions = []metav1.Condition{{
			Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonPodCrashed, Message: "Pod entered Failed phase",
			LastTransitionTime: metav1.Now(),
		}}
		uid = bs.UID
		return c.Status().Update(ctx, &bs)
	}), "stamp bundle Failed=True")
	return uid
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

// stampBundleSessionsReadySkipFailed polls for SpiceboxSessions in the
// default namespace and stamps any without a Ready=True condition, EXCEPT:
//
//   - sessions the test has deliberately marked Failed=True — otherwise
//     this loop races the test's stampBundleFailed call and flips the
//     condition back to Ready before the AgentSession controller ever
//     observes the failure;
//   - sessions with a non-zero DeletionTimestamp — a session mid-delete
//     (the controller's retry-recreate step) shouldn't be resurrected with
//     a status write.
//
// Runs until ctx is canceled (test cleanup). Idempotent — touching a
// session that's already Ready is a no-op write.
//
// Cloned from p1_multi_bundle_pvc's stampBundleSessionsReady with the two
// skip guards added; the helper lives per-scenario-file by design (see the
// task brief), not a shared package.
func stampBundleSessionsReadySkipFailed(ctx context.Context, t *testing.T, c client.Client) {
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
			t.Logf("stampBundleSessionsReadySkipFailed: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if s.DeletionTimestamp != nil {
				continue
			}
			if meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed) {
				continue
			}
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
				t.Logf("stampBundleSessionsReadySkipFailed: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}
