package pipeline_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

// testMarkerSigner is the connector identity restart-marker tests sign as. The
// operator verifies status.pendingRestart before acting on it, so a publisher
// writing an unsigned marker is a bug — NewRestartPublisher refuses one, and
// these tests supply a real signer rather than nil.
var testMarkerSigner = restartmarker.NewSigner(
	ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x3d}, ed25519.SeedSize)),
	restartmarker.Publisher,
)

// newTestScheme builds a runtime.Scheme with the spicebox API registered.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// newTestSession returns an AgentSession fixture pre-created in a fake client.
func newTestSession(t *testing.T, ns, name string) (*fake.ClientBuilder, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}
	sch := newTestScheme(t)
	b := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{})
	return b, sess
}

func TestPublishRestartTrigger_Envelope(t *testing.T) {
	var capturedSubject string
	var capturedPayload []byte
	publish := func(subject string, data []byte) error {
		capturedSubject = subject
		capturedPayload = data
		return nil
	}

	b, _ := newTestSession(t, "ns", "sess")
	pub := pipeline.NewRestartPublisher(publish, b.Build(), testMarkerSigner)

	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 7,
		NewUserText:  "edited",
		TriggeredBy: channelevents.ExternalIdentity{
			Kind: "slack", ExternalID: "U1", Email: "alice@example.com",
		},
		KindRequestRef: "view-id-abc",
	})
	require.NoError(t, err)

	assert.Contains(t, capturedSubject, "restart_trigger",
		"subject must reflect KindRestartTrigger")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(capturedPayload, &env))
	assert.Equal(t, channelevents.KindRestartTrigger, env.Kind)

	var payload channelevents.RestartTriggerPayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, int32(7), payload.CutTurnIndex)
	assert.Equal(t, "edited", payload.NewUserText)
	assert.NotEmpty(t, payload.TriggeredBy, "publisher must canonicalize TriggeredBy")
	assert.Contains(t, payload.TriggeredBy, "user:",
		"canonical subject must start with 'user:' prefix from identity.Principal.Subject()")
	assert.NotEmpty(t, payload.TargetSessionName, "publisher computes deterministic child name")
	assert.True(t, len(payload.TargetSessionName) > len("sess-fk"),
		"TargetSessionName carries fork hash suffix")
	assert.Equal(t, "view-id-abc", payload.KindRequestRef)
}

func TestPublishRestartTrigger_ValidationError(t *testing.T) {
	b, _ := newTestSession(t, "ns", "sess")
	pub := pipeline.NewRestartPublisher(func(string, []byte) error { return nil }, b.Build(), testMarkerSigner)

	// Empty NewUserText → validation error from RestartTriggerPayload.Validate().
	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 0,
		NewUserText:  "",
		TriggeredBy:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewUserText")
}

// TestPublishRestartTrigger_ForkerWithoutEmail_Errors locks the fix for the
// second instance of the email-drop bug: a forker whose identity carries no
// verified email must NOT be canonicalized to a synthetic subject. The
// SessionFork gate keys agentsession#fork on the owner's user:<email>, so a
// synthetic forker subject would fail the owner-check and deny the owner the
// right to restart their own session. The publisher must surface it as an
// error BEFORE any status patch or NATS publish — never mint the phantom.
func TestPublishRestartTrigger_ForkerWithoutEmail_Errors(t *testing.T) {
	b, _ := newTestSession(t, "ns", "sess")
	cli := b.Build()
	published := false
	pub := pipeline.NewRestartPublisher(func(string, []byte) error {
		published = true
		return nil
	}, cli, testMarkerSigner)

	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 0,
		NewUserText:  "edited",
		// No Email → must surface an error, not mint a synthetic forker subject.
		TriggeredBy: channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1"},
	})
	require.Error(t, err, "an email-less forker must surface as an error, not a synthetic subject")
	assert.Contains(t, err.Error(), "forker identity unresolved")
	assert.False(t, published, "no restart_trigger may be published when the forker is unresolved")

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sess"}, &sess))
	assert.Nil(t, sess.Status.PendingRestart, "no PendingRestart may be written for an unresolved forker")
}

func TestDeterministicForkName_TruncatesLongParent(t *testing.T) {
	parent := strings.Repeat("a", 300) // well over K8s 253-char limit
	b, _ := newTestSession(t, "ns", parent)

	var p []byte
	pub := pipeline.NewRestartPublisher(func(_ string, data []byte) error {
		p = data
		return nil
	}, b.Build(), testMarkerSigner)

	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: parent},
		CutTurnIndex: 0,
		NewUserText:  "x",
		TriggeredBy:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	})
	require.NoError(t, err)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(p, &env))
	var payload channelevents.RestartTriggerPayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.LessOrEqual(t, len(payload.TargetSessionName), 253, "fork name must fit within K8s 253-char name limit")
}

func TestPublishRestartTrigger_DeterministicTargetName(t *testing.T) {
	b, _ := newTestSession(t, "ns", "sess")
	c := b.Build()

	var p1, p2 []byte
	pub := pipeline.NewRestartPublisher(func(_ string, data []byte) error {
		if p1 == nil {
			p1 = data
		} else {
			p2 = data
		}
		return nil
	}, c, testMarkerSigner)

	trig := channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 7,
		NewUserText:  "edited",
		TriggeredBy:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	}
	require.NoError(t, pub(context.Background(), trig))
	require.NoError(t, pub(context.Background(), trig))

	var e1, e2 channelevents.Envelope
	require.NoError(t, json.Unmarshal(p1, &e1))
	require.NoError(t, json.Unmarshal(p2, &e2))
	var pl1, pl2 channelevents.RestartTriggerPayload
	require.NoError(t, json.Unmarshal(e1.Payload, &pl1))
	require.NoError(t, json.Unmarshal(e2.Payload, &pl2))
	assert.Equal(t, pl1.TargetSessionName, pl2.TargetSessionName,
		"same (parent, cut, text) → same deterministic target name")
}

func TestPublishRestartTrigger_NoSession_Errors(t *testing.T) {
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	// No AgentSession created — Get will return NotFound.

	publishCalls := 0
	publish := func(_ string, _ []byte) error { publishCalls++; return nil }
	pub := pipeline.NewRestartPublisher(publish, c, testMarkerSigner)

	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "missing"},
		CutTurnIndex: 1,
		NewUserText:  "x",
		TriggeredBy:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get session")
	assert.Equal(t, 0, publishCalls, "NATS publish must not fire when session lookup failed")
}

func TestPublishRestartTrigger_PatchesAgentSessionStatus(t *testing.T) {
	ctx := context.Background()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseIdle},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()

	publish := func(_ string, _ []byte) error { return nil }
	pub := pipeline.NewRestartPublisher(publish, c, testMarkerSigner)

	err := pub(context.Background(), channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: "ns", Name: "sess"},
		CutTurnIndex: 5,
		NewUserText:  "edited",
		TriggeredBy:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "sess"}, &got))
	require.NotNil(t, got.Status.PendingRestart)
	assert.Equal(t, int32(5), got.Status.PendingRestart.CutTurnIndex)
	assert.Equal(t, "edited", got.Status.PendingRestart.NewUserText)
	assert.NotEmpty(t, got.Status.PendingRestart.TargetSessionName)
	assert.Contains(t, got.Status.PendingRestart.TriggeredBy, "user:", "canonicalized subject")
}
