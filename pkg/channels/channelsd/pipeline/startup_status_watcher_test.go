package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// capturedPublish records every envelope a watcher publishes, so tests assert
// on real published bytes rather than on a mock's call log.
type capturedPublish struct {
	subjects []string
	payloads [][]byte
}

func (c *capturedPublish) publish(subject string, data []byte) error {
	c.subjects = append(c.subjects, subject)
	c.payloads = append(c.payloads, data)
	return nil
}

// blockedSession builds a not-yet-running AgentSession carrying the given
// conditions. Uses a made-up fixture name — never an example's name.
func blockedSession(phase string, conds ...metav1.Condition) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-abc123", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      phase,
			Conditions: conds,
		},
	}
}

func cond(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg}
}

// attach gives a session the channel binding the watcher requires before it
// will consider publishing anything to a thread.
func attach(sess *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Kind:              browser.KindName,
		Name:              "demo-agent-chan",
		Key:               browser.ChannelKey("default", "demo-agent-chan"),
		NATSSubjectPrefix: "ap.session.default.demo-agent-abc123",
	}
	return sess
}

func unschedulableSession() *spiceboxv1alpha1.AgentSession {
	return attach(blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionTrue, "UserIdentityResolved", ""),
		cond(spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, metav1.ConditionFalse,
			spiceboxv1alpha1.ReasonSandboxUnschedulable,
			"demo-agent-abc123-codelike-pod pod unschedulable: 0/1 nodes are available: 1 Insufficient memory."),
	))
}

func TestStartupStatusWatcher_PublishesTheRealBlockerToTheThread(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}

	require.NoError(t, w.ReconcileOne(context.Background(), unschedulableSession()))

	require.Len(t, cap.payloads, 1, "a wedged session must get a caption naming why")
	assert.Contains(t, string(cap.payloads[0]), "Insufficient memory")
}

// The watcher ticks every few seconds for the whole time a session is wedged.
// Republishing an unchanged caption would rewrite the thread's status surface
// on every tick — churn the user sees as a flickering caption and Slack sees as
// a rate-limited edit storm.
func TestStartupStatusWatcher_UnchangedCaption_PublishedOnceNotEveryTick(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}
	ctx := context.Background()

	require.NoError(t, w.ReconcileOne(ctx, unschedulableSession()))
	require.NoError(t, w.ReconcileOne(ctx, unschedulableSession()))
	require.NoError(t, w.ReconcileOne(ctx, unschedulableSession()))

	assert.Len(t, cap.payloads, 1, "the same blocker must be announced once, not once per tick")
}

// A session that moves from one blocker to a different one must re-announce:
// the caption is persistent, so a stale one is exactly the defect this fixes.
func TestStartupStatusWatcher_BlockerChanges_RepublishesTheNewOne(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}
	ctx := context.Background()

	require.NoError(t, w.ReconcileOne(ctx, unschedulableSession()))

	moved := attach(blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionTrue, "UserIdentityResolved", ""),
		cond(spiceboxv1alpha1.AgentSessionConditionBundlesReady, metav1.ConditionFalse, "ImagePullBackOff", "pulling demo-toolchain:dev failed"),
	))
	require.NoError(t, w.ReconcileOne(ctx, moved))

	require.Len(t, cap.payloads, 2, "a different blocker is new information and must reach the thread")
	assert.Contains(t, string(cap.payloads[1]), "pulling demo-toolchain:dev failed")
}

func TestStartupStatusWatcher_NothingToSay_PublishesNothing(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}

	healthy := attach(blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionClassResolved, metav1.ConditionTrue, "AllReferencesResolve", ""),
	))
	require.NoError(t, w.ReconcileOne(context.Background(), healthy))

	assert.Empty(t, cap.payloads)
}

// A session with no channel has no thread to publish into.
func TestStartupStatusWatcher_NoInputChannel_PublishesNothing(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}

	detached := blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable, "no room"),
	)
	require.NoError(t, w.ReconcileOne(context.Background(), detached))

	assert.Empty(t, cap.payloads)
}

// The dedup memory must not outlive the sessions it is about, or a
// long-running channelsd accumulates one entry per session it ever saw.
func TestStartupStatusWatcher_ForgetsSessionsThatStarted(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}
	ctx := context.Background()

	require.NoError(t, w.ReconcileOne(ctx, unschedulableSession()))
	assert.Equal(t, 1, w.trackedSessions(), "a wedged session is remembered so its caption is not republished")

	started := attach(blockedSession(spiceboxv1alpha1.AgentSessionPhaseRunning))
	require.NoError(t, w.ReconcileOne(ctx, started))
	assert.Zero(t, w.trackedSessions(), "once a session starts, the runner owns its captions and the entry is dead weight")
}

// A session that ends in Failed died without the agent ever posting a closing
// message (an eviction, a crashed runner), so the thread just stops. The
// watcher must post the failure notice ONCE — the failure mode where a
// BundleFailed session left its channel thread silent. Only the friendly lead
// reaches the thread; the raw infra detail and the reason token do not.
func TestStartupStatusWatcher_TerminalFailure_PostsFriendlyNoticeOnce(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}
	ctx := context.Background()

	failed := attach(blockedSession(spiceboxv1alpha1.AgentSessionPhaseFailed,
		cond(spiceboxv1alpha1.AgentSessionConditionFailed, metav1.ConditionTrue,
			spiceboxv1alpha1.ReasonAgentSessionBundleFail,
			"bundle SpiceboxSession failed: The node was low on resource: ephemeral-storage")))
	failed.Status.FailureReason = spiceboxv1alpha1.ReasonAgentSessionBundleFail

	require.NoError(t, w.ReconcileOne(ctx, failed))
	require.NoError(t, w.ReconcileOne(ctx, failed)) // second tick: must not re-post

	require.Len(t, cap.payloads, 1, "a terminal failure must reach the thread exactly once")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(cap.payloads[0], &env))
	assert.Equal(t, channelevents.KindNotification, env.Kind)
	var pl channelevents.NotificationPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Contains(t, pl.Text, "couldn't run", "the friendly failure lead must reach the user")
	assert.NotContains(t, pl.Text, "ephemeral-storage", "raw infra detail stays out of the user's thread")
	assert.NotContains(t, pl.Text, "BundleFailed", "no raw reason token in the user's thread")
}

// Publishing the caption must produce a real notification envelope the outbound
// relay recognizes, not just any bytes on the subject.
func TestStartupStatusWatcher_PublishesANotificationEnvelope(t *testing.T) {
	var cap capturedPublish
	w := &StartupStatusWatcher{NATSPublish: cap.publish}

	require.NoError(t, w.ReconcileOne(context.Background(), unschedulableSession()))
	require.Len(t, cap.payloads, 1)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(cap.payloads[0], &env), "the published bytes must decode as a channelevents envelope")
	assert.Equal(t, channelevents.KindNotification, env.Kind)
	assert.NoError(t, env.Validate(), "the outbound relay will reject an envelope that does not validate")
}
