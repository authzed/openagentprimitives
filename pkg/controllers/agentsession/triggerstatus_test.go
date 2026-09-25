// pkg/controllers/agentsession/triggerstatus_test.go
//
// The framework answering a trigger nobody else can: a session that reached a
// terminal failure BEFORE its runner ever came up has no agent to report from
// inside, so the pull request that started it would otherwise show nothing at
// all — not a failure, not a claim, nothing. From the author's side the
// reviewer simply never existed.
package agentsession_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

const (
	tsNS          = "default"
	tsSessionName = "demo-reviewbot-gh-abc123"
	tsChannelName = "demo-reviewbot-gh"
	tsSecretName  = "demo-reviewbot-gh-creds"
	tsBindingKey  = "pr:demo-org/platform#42"
	// tsKindName is a kind that owns a trigger status surface; the registered
	// `fake` kind, which does not, is the control.
	tsKindName = "faketrigger-operator"
)

// operatorTriggerKind is a registered channel kind with a status surface,
// standing in for github. A fake rather than the real kind because these tests
// are about WHEN the operator answers a trigger and what it reads first — a
// live provider client in the middle would let a github-side bug read as an
// operator-side pass, or the reverse.
type operatorTriggerKind struct {
	fakekind.Kind

	mu sync.Mutex
	// standing is what Claim reports back when set: a trigger that already
	// carries a verdict from a real agent. claimErr makes Claim fail instead.
	standing *channelkinds.TriggerClaim
	claimErr error
	// claims and published are what the reconciler actually did to the surface.
	claims    int
	published []channelkinds.TriggerConclusion
}

func (*operatorTriggerKind) Name() string { return tsKindName }

// Compile-time, and NOT decoration: the registry resolves a reporter by TYPE
// ASSERTION, so a fake that stops satisfying this interface still builds and
// silently stops being one. See the same guard in pkg/agent/tool/meta.
var _ channelkinds.TriggerStatusReporter = (*operatorTriggerKind)(nil)

func (*operatorTriggerKind) TriggerSurfaceKind() string { return "a pull request's fixture check run" }

// TriggerProviderStateIn: this fixture kind writes no provider identifiers into
// its own text, so there is nothing to read back. The zero value is the complete
// answer here, not a stub — the method exists because the seam is what a capture
// asks to seed a stand-in with, and a kind that mints nothing seeds nothing.
func (*operatorTriggerKind) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}

func (k *operatorTriggerKind) TriggerSurface(
	ch *spiceboxv1alpha1.Channel, b *spiceboxv1alpha1.ChannelBinding,
	secrets channelkinds.WebhookSecrets, _ channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	// Pinned here because it is the reconciler's job to hand the kind the
	// Channel's OWN credentials — the same resolution materializeSidecarSecret
	// already does, which is why this needs no new RBAC.
	if string(secrets.Data["fixture-key"]) != "present" {
		return nil, errors.New("fixture: the Channel's credentials Secret did not reach the kind")
	}
	if ch.Name != tsChannelName || b.Key != tsBindingKey {
		return nil, errors.New("fixture: the surface was addressed for something other than this session's own trigger")
	}
	return &operatorTriggerSurface{kind: k}, nil
}

type operatorTriggerSurface struct{ kind *operatorTriggerKind }

func (*operatorTriggerSurface) Surface() string {
	return "the fixture check run on demo-org/platform#42"
}

func (s *operatorTriggerSurface) Claim(context.Context) (channelkinds.TriggerClaim, error) {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	s.kind.claims++
	if s.kind.claimErr != nil {
		return channelkinds.TriggerClaim{}, s.kind.claimErr
	}
	if s.kind.standing != nil {
		return *s.kind.standing, nil
	}
	return channelkinds.TriggerClaim{Ref: "fixture run 1"}, nil
}

func (s *operatorTriggerSurface) Conclude(_ context.Context, c channelkinds.TriggerConclusion) error {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	s.kind.published = append(s.kind.published, c)
	return nil
}

func (k *operatorTriggerKind) snapshot() (claims int, published []channelkinds.TriggerConclusion) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.claims, append([]channelkinds.TriggerConclusion(nil), k.published...)
}

// triggerKind is the process-wide registration. The channel-kind registry
// panics on a duplicate name and has no Unregister, so one instance is shared
// and its recorded calls are reset per test.
var triggerKind = func() *operatorTriggerKind {
	k := &operatorTriggerKind{}
	chregistry.Register(k)
	return k
}()

func resetTriggerKind(t *testing.T) *operatorTriggerKind {
	t.Helper()
	triggerKind.mu.Lock()
	defer triggerKind.mu.Unlock()
	triggerKind.standing = nil
	triggerKind.claimErr = nil
	triggerKind.claims = 0
	triggerKind.published = nil
	return triggerKind
}

// bootFailedSession is an AgentSession bound as INPUT to kindName, in the state
// a session is in when provisioning failed and no runner ever came up.
func bootFailedSession(kindName string) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: tsSessionName, Namespace: tsNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-reviewbot"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhasePending},
	}
	if kindName != "" {
		sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
			Name: tsChannelName, Kind: kindName, Key: tsBindingKey,
			NATSSubjectPrefix: "ap.session." + tsNS + "." + tsSessionName,
		}
	}
	return sess
}

// triggerReconciler builds a Reconciler over a cluster holding sess, its input
// Channel and that Channel's credentials Secret.
func triggerReconciler(t *testing.T, sess *spiceboxv1alpha1.AgentSession) *agentsession.Reconciler {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))

	objs := []client.Object{
		sess,
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: tsChannelName, Namespace: tsNS},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           tsKindName,
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: tsSecretName},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tsSecretName, Namespace: tsNS},
			Data:       map[string][]byte{"fixture-key": []byte("present")},
		},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	return &agentsession.Reconciler{Client: c}
}

// TestBootFailure_AnswersTheTriggerNobodyElseCan is the live failure: a session
// that died on credential resolution posted no message and left no status on
// the pull request, so from the author's side the reviewer never existed.
func TestBootFailure_AnswersTheTriggerNobodyElseCan(t *testing.T) {
	k := resetTriggerKind(t)
	sess := bootFailedSession(tsKindName)
	r := triggerReconciler(t, sess)

	_, err := r.MarkBootFailedForTest(context.Background(), sess,
		spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
		"materialize cred secret for sidecar \"gitlike\": resolve credential: secret not found")
	require.NoError(t, err)

	claims, published := k.snapshot()
	assert.Equal(t, 1, claims, "the surface is READ before it is written")
	require.Len(t, published, 1, "the pull request must be told the review is not coming")
	assert.Equal(t, channelkinds.TriggerOutcomeCouldNotFinish, published[0].Outcome,
		"could_not_finish is the only outcome a caller with no agent can honestly write")
	assert.Contains(t, published[0].Summary, spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed,
		"the framework's own failure reason is what a person reads there")
}

// TestBootFailure_NeverOverwritesAVerdictAlreadyOnTheSurface is the
// read-before-write property. Conclude is last-write-wins and find-or-create,
// so a framework cleanup that wrote unconditionally would replace a real
// review's verdict with "could not finish" — turning a delivered result into a
// failure notice on the pull request.
func TestBootFailure_NeverOverwritesAVerdictAlreadyOnTheSurface(t *testing.T) {
	k := resetTriggerKind(t)
	k.standing = &channelkinds.TriggerClaim{
		Ref: "fixture run 7", Concluded: true, Outcome: channelkinds.TriggerOutcomeProblemsFound,
	}
	sess := bootFailedSession(tsKindName)
	r := triggerReconciler(t, sess)

	_, err := r.MarkBootFailedForTest(context.Background(), sess,
		spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, "boom")
	require.NoError(t, err)

	claims, published := k.snapshot()
	assert.Equal(t, 1, claims)
	assert.Empty(t, published,
		"a trigger that already carries an answer keeps it; the framework only fills a silence")
}

// TestBootFailure_StaysOutOfWhatTheAgentOwns. The framework answers only where
// nothing else could. A session whose runner came up has a live agent — and, if
// that agent finished without concluding, the completion requirement is what
// covers it; a session already terminal is a re-reconcile of a failure that was
// answered on the pass that produced it.
func TestBootFailure_StaysOutOfWhatTheAgentOwns(t *testing.T) {
	cases := []struct {
		name string
		mut  func(s *spiceboxv1alpha1.AgentSession)
	}{
		{
			name: "the runner is Running: there is a live agent to report from inside",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
			},
		},
		{
			name: "the runner has been Ready: the agent had its turn",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Conditions = []metav1.Condition{{
					Type: spiceboxv1alpha1.AgentSessionConditionRunnerReady, Status: metav1.ConditionFalse,
					Reason: "PodPending", Message: "waiting", LastTransitionTime: metav1.Now(),
				}}
			},
		},
		{
			name: "already Failed: this pass is not the transition, it is a re-read",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
			},
		},
		{
			name: "already Succeeded: an ordinary completion is not a failure",
			mut: func(s *spiceboxv1alpha1.AgentSession) {
				s.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := resetTriggerKind(t)
			sess := bootFailedSession(tsKindName)
			tc.mut(sess)
			r := triggerReconciler(t, sess)

			_, err := r.MarkBootFailedForTest(context.Background(), sess,
				spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, "boom")
			require.NoError(t, err)

			claims, published := k.snapshot()
			assert.Zero(t, claims, "the surface must not even be read")
			assert.Empty(t, published)
		})
	}
}

// TestBootFailure_SilentWhereThereIsNoTriggerToAnswer: a kubectl-driven session
// and a session bound to a conversational kind both have nothing to report on,
// and neither may be turned into a provider call.
func TestBootFailure_SilentWhereThereIsNoTriggerToAnswer(t *testing.T) {
	cases := []struct {
		name string
		kind string
	}{
		{name: "input kind with no status surface", kind: fakekind.Kind{}.Name()},
		{name: "kubectl-driven session: no channel started it", kind: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := resetTriggerKind(t)
			sess := bootFailedSession(tc.kind)
			r := triggerReconciler(t, sess)

			_, err := r.MarkBootFailedForTest(context.Background(), sess,
				spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, "boom")
			require.NoError(t, err)

			claims, published := k.snapshot()
			assert.Zero(t, claims)
			assert.Empty(t, published)
		})
	}
}

// TestBootFailure_AProviderThatCannotBeReadIsNotOverwritten: when Claim itself
// fails, whether the trigger already carries a verdict is UNKNOWN, so nothing
// is written — the same reason read-before-write exists. The terminal
// transition still lands: reaching Failed matters more than reporting it.
func TestBootFailure_AProviderThatCannotBeReadIsNotOverwritten(t *testing.T) {
	k := resetTriggerKind(t)
	k.claimErr = errors.New("status 503")
	sess := bootFailedSession(tsKindName)
	r := triggerReconciler(t, sess)

	_, err := r.MarkBootFailedForTest(context.Background(), sess,
		spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, "boom")
	require.NoError(t, err, "a trigger that cannot be reported must not fail the reconcile that marks the session Failed")

	_, published := k.snapshot()
	assert.Empty(t, published, "an unreadable surface may already hold a real verdict")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseFailed, sess.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSidecarBootFailed, sess.Status.FailureReason)
}
