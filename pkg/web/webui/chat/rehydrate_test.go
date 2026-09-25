package chat

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// chatSessionCR builds the AgentSession a prior webd process would have left
// behind: the labels + started-by annotation browsersession.Create stamps,
// the first user message as spec.prompt.inline, and a phase. ns is a
// parameter (not the fixed newChatSessionNamespace) so a test can place two
// same-named sessions in two different namespaces — see handlers_test.go's
// TestChatPlaneAddressesTheNamespaceItWasGiven.
func chatSessionCR(ns, name, owner, class, firstMsg, phase string, created time.Time) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: name + "-chan",
				spiceboxv1alpha1.LabelChannelKind: browser.KindName,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: owner,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  class,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: firstMsg},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: name + "-chan", Kind: browser.KindName,
			},
		},
	}
	s.Status.Phase = phase
	return s
}

// chatChannelCR is the browser Channel a chat AgentSession binds to. rehydrate
// loads it to wire the Host, so a rehydrate test needs it present.
func chatChannelCR(ns, sessionName string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName + "-chan", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: browser.KindName, AgentClass: "demo-agent",
			Role: spiceboxv1alpha1.ChannelRoleBoth, SessionScope: "user",
		},
	}
}

// TestAuthorize_RehydratesDormantSession is the other half of the restart fix:
// listing a conversation the user cannot open would trade one silent loss for a
// confusing one. Before this, authorize resolved only through the in-memory
// map, so after a restart the detail, transcript, websocket and message
// endpoints all 404'd — a bookmarked session URL included.
func TestAuthorize_RehydratesDormantSession(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-aaaa1111"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "resume me",
			spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	key := sessionKey{Namespace: newChatSessionNamespace, Name: name}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)

	entry, err := reg.authorize(context.Background(), key, owner)

	require.NoError(t, err, "a dormant conversation must be re-attachable")
	require.NotNil(t, entry)
	assert.Equal(t, owner, entry.owner, "ownership is restored from the CR annotation")
	assert.Equal(t, "resume me", entry.title)
	assert.NotNil(t, entry.listener, "a rehydrated entry must be able to accept messages")
	assert.NotNil(t, entry.senders, "a rehydrated entry must be reachable by the outbound relay")
	t.Cleanup(func() { reg.teardown(key, terminalNotice{Reason: "test"}) })
}

// TestAuthorize_RehydrateThreadsTheRequestedNamespace is I3's regression:
// every OTHER rehydrate test in this file uses newChatSessionNamespace, so
// nothing else in the suite proves rehydrate wires the entry — and the
// browser Host and boundListener inside it — with the namespace the URL
// actually named, rather than a re-hardcoded default. A wrong namespace here
// is a silent-failure class of its own: the in-process Host would publish an
// inbound on the wrong session's NATS subject (channelsd finds no such
// session, the message vanishes), and the health watcher would poll the
// wrong AgentSession (NotFound, firing a bogus "session removed" teardown).
func TestAuthorize_RehydrateThreadsTheRequestedNamespace(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-bbbb2222"
	const ns = "other-ns"
	k8s := newFakeK8sClient(t,
		chatSessionCR(ns, name, owner, "demo-agent", "resume me",
			spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatChannelCR(ns, name),
	)
	key := sessionKey{Namespace: ns, Name: name}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	t.Cleanup(func() { reg.teardown(key, terminalNotice{Reason: "test"}) })

	entry, err := reg.authorize(context.Background(), key, owner)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, ns, entry.ns, "the entry must be wired with the URL's namespace, not a re-hardcoded default")
	bl, ok := entry.listener.(*boundListener)
	require.True(t, ok, "a rehydrated entry's listener is a *boundListener")
	assert.Equal(t, ns, bl.ns,
		"boundListener must carry the URL's namespace too — it's what SubmitInterrupt/SubmitDecision address channelsd with")
}

// TestAuthorize_RehydrateRefusesUnauthorizedSubject: standing after a
// restart rests entirely on CheckInteract (interactableChatSession), so it
// gets its own test — a subject CheckInteract refuses must not rehydrate.
func TestAuthorize_RehydrateRefusesUnauthorizedSubject(t *testing.T) {
	const name = "demo-agent-aaaa1111"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, "user:bob@example.com", "demo-agent", "bob's",
			spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	key := sessionKey{Namespace: newChatSessionNamespace, Name: name}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:bob@example.com")}, newFakeBuilder().build)

	_, err := reg.authorize(context.Background(), key, "user:alice@example.com")

	assert.ErrorIs(t, err, ErrForbidden)
}

// TestAuthorize_RehydrateAdmitsParticipantNotOnlyStarter proves the CR
// annotation comparison this package used to perform is really gone from
// the resume path: a subject who is NOT the CR's started-by annotation, but
// whom CheckInteract admits, must still rehydrate successfully.
func TestAuthorize_RehydrateAdmitsParticipantNotOnlyStarter(t *testing.T) {
	const starter = "user:alice@example.com"
	const participant = "user:bob@example.com"
	const name = "demo-agent-aaaa1111"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, starter, "demo-agent", "alice started this",
			spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	key := sessionKey{Namespace: newChatSessionNamespace, Name: name}
	// CheckInteract admits the participant, not just the CR's own starter —
	// proving the old `StartedBySubject != subject` refusal is gone.
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor(participant)}, newFakeBuilder().build)
	t.Cleanup(func() { reg.teardown(key, terminalNotice{Reason: "test"}) })

	entry, err := reg.authorize(context.Background(), key, participant)

	require.NoError(t, err, "a participant CheckInteract admits must be able to rehydrate a conversation it did not start")
	require.NotNil(t, entry)
	assert.Equal(t, starter, entry.owner, "the durable owner record stays the CR's starter, not the reopening participant")
}

// TestAuthorize_RehydrateSkipsTerminalSessions: a finished conversation lists as
// ended and stays readable, but must not be re-attached — wiring a health
// watcher onto it would immediately tear it down again.
func TestAuthorize_RehydrateSkipsTerminalSessions(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "done-1111"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "finished",
			spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	key := sessionKey{Namespace: newChatSessionNamespace, Name: name}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)

	_, err := reg.authorize(context.Background(), key, owner)

	assert.ErrorIs(t, err, ErrSessionNotFound)
}

// TestAuthorize_RehydrateIsIdempotent: two requests for the same dormant
// session must converge on ONE entry. Two would mean two health watchers and
// two Hosts against one conversation.
func TestAuthorize_RehydrateIsIdempotent(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-aaaa1111"
	k8s := newFakeK8sClient(t,
		chatSessionCR(newChatSessionNamespace, name, owner, "demo-agent", "resume me",
			spiceboxv1alpha1.AgentSessionPhaseIdle, time.Now()),
		chatChannelCR(newChatSessionNamespace, name),
	)
	key := sessionKey{Namespace: newChatSessionNamespace, Name: name}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	t.Cleanup(func() { reg.teardown(key, terminalNotice{Reason: "test"}) })

	first, err := reg.authorize(context.Background(), key, owner)
	require.NoError(t, err)
	second, err := reg.authorize(context.Background(), key, owner)
	require.NoError(t, err)

	assert.Same(t, first, second, "a second authorize must reuse the rehydrated entry")
}

// TestAuthorize_ReadFailureIsUnavailableNotNotFound is the same
// verdict-vs-failed-read split browsersession.Create makes, at the door a
// viewer actually knocks on.
//
// This is the concrete failure the cluster-wide Channel grant was added for,
// and the one that survives it in every OTHER form: a read that fails for a
// reason that is not NotFound — an RBAC refusal in a namespace this webd
// cannot read, a throttled apiserver — must not be reported as "no such
// session". The transcript renders (it comes from operator memory), so the
// viewer would be looking at their conversation beside a 404 saying it does
// not exist, and an operator would go looking for a deleted session.
//
// The status assertion is the load-bearing half: ErrSessionNotFound and
// ErrSessionUnavailable are both errors, and a test that only required "an
// error" would have held before the split.
func TestAuthorize_ReadFailureIsUnavailableNotNotFound(t *testing.T) {
	const owner = "user:alice@example.com"
	const name = "demo-agent-bbbb2222"
	key := sessionKey{Namespace: "team-a", Name: name}

	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.AgentSession); ok {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "agentsessions"},
					name, errors.New("webd cannot read this namespace"))
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)

	_, err := reg.authorize(context.Background(), key, owner)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionUnavailable,
		"a failed read is not evidence the conversation is gone")
	assert.NotErrorIs(t, err, ErrSessionNotFound,
		"reporting it as not-found is the readable-but-unopenable failure: the transcript renders beside a 404 denying the session exists")

	status, msg := mapSubmitError(logr.Discard(), err)
	assert.Equal(t, http.StatusServiceUnavailable, status, "unavailable is retryable; not-found is not")
	assert.NotContains(t, msg, "cannot read this namespace",
		"the browser-facing message must not carry the control-plane cause")
	assert.NotContains(t, msg, "agentsessions", "nor any Kubernetes vocabulary")
}
