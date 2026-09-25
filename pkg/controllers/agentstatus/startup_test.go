package agentstatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// blockedSession builds a not-yet-running AgentSession carrying the given
// conditions. A made-up fixture name — never an example's name.
func blockedSession(phase string, conds ...metav1.Condition) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-abc123", Namespace: "default"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase, Conditions: conds},
	}
}

func cond(t string, s metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg}
}

// The defect this exists to prevent: credentials resolve, the session moves on
// to scheduling and wedges there, but the thread still shows the credential
// watcher's persistent "waiting for identity setup…" caption because nothing
// ever supersedes it. The user is told to do something they already did, while
// the real blocker — a sandbox that cannot be placed — is never surfaced at all.
func TestStartupCaption_SchedulingBlockedAfterCredentialsResolve_NamesTheRealBlocker(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionTrue, "UserIdentityResolved", ""),
		cond(spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, metav1.ConditionFalse,
			spiceboxv1alpha1.ReasonSandboxUnschedulable,
			"demo-agent-abc123-codelike-pod pod unschedulable: 0/1 nodes are available: 1 Insufficient memory."),
	)

	text, short, ok := StartupCaption(sess)
	require.True(t, ok, "a session wedged on scheduling must say so, not sit under a stale caption")

	assert.Contains(t, text, "Insufficient memory", "the scheduler's own reason is the actionable part; it must reach the user")
	assert.NotContains(t, text, "identity setup", "the credential caption must not survive credentials being resolved")
	assert.NotEmpty(t, short)
}

// The credential watcher owns the caption while credentials are genuinely
// outstanding — publishing a competing one here would fight it every tick.
func TestStartupCaption_CredentialsStillOutstanding_DefersToCredentialWatcher(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionFalse, "AwaitingUserIdentity", "waiting on the user to link"),
	)

	_, _, ok := StartupCaption(sess)
	assert.False(t, ok, "the credential prompt's own caption owns this window")
}

// Once the runner is up it owns the caption surface (update_status, plans,
// tool progress). Publishing derived startup captions past that point would
// overwrite live turn state.
func TestStartupCaption_SessionStarted_StopsPublishing(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhaseRunning,
		cond(spiceboxv1alpha1.AgentSessionConditionSandboxScheduling, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable, "stale"),
	)

	_, _, ok := StartupCaption(sess)
	assert.False(t, ok, "a started session's captions belong to the runner")
}

// A session merely coming up (nothing False yet) has no blocker to report —
// the generic "starting…" placeholder is already correct, and republishing
// over it every tick would be churn.
func TestStartupCaption_PendingWithNoFailedGate_StaysQuiet(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionClassResolved, metav1.ConditionTrue, "AllReferencesResolve", ""),
	)

	_, _, ok := StartupCaption(sess)
	assert.False(t, ok, "nothing is wrong yet; there is nothing to supersede the starting placeholder with")
}

// The original complaint: a healthy session provisioning its bundles showed
// "⠸ Blocked: BundlesProvisioning" — a raw machine token wearing an alarming
// prefix, for a state that is ordinary startup progress. Both surfaces must
// read as progress, and the condition's CRD-speak message must not reach the
// user.
func TestStartupCaption_BundlesProvisioning_ReadsAsProgressNotMachineToken(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionTrue, "UserIdentityResolved", ""),
		cond(spiceboxv1alpha1.AgentSessionConditionBundlesReady, metav1.ConditionFalse,
			"BundlesProvisioning", "waiting for bundle SpiceboxSessions to become Ready"),
	)

	text, short, ok := StartupCaption(sess)
	require.True(t, ok)

	assert.NotContains(t, text, "BundlesProvisioning", "raw machine token must not reach the user")
	assert.NotContains(t, short, "BundlesProvisioning", "raw machine token must not reach the user")
	assert.NotContains(t, short, "Blocked", "ordinary provisioning is progress, not a blocker")
	assert.NotContains(t, text, "SpiceboxSession", "CRD names stay off the user surface")
	assert.Contains(t, text, "Preparing", "progress tone: say what is happening")
}

// Bundle provisioning failures are the other common startup wall and must be
// just as visible as scheduling ones.
func TestStartupCaption_BundlesBlocked_NamesTheBundleReason(t *testing.T) {
	sess := blockedSession(spiceboxv1alpha1.AgentSessionPhasePending,
		cond(spiceboxv1alpha1.AgentSessionConditionCredentialsReady, metav1.ConditionTrue, "UserIdentityResolved", ""),
		cond(spiceboxv1alpha1.AgentSessionConditionBundlesReady, metav1.ConditionFalse, "ImagePullBackOff", "pulling demo-toolchain:dev failed"),
	)

	text, _, ok := StartupCaption(sess)
	require.True(t, ok)
	assert.Contains(t, text, "pulling demo-toolchain:dev failed")
}

func TestStartupCaption_NilSession_StaysQuiet(t *testing.T) {
	_, _, ok := StartupCaption(nil)
	assert.False(t, ok)
}
