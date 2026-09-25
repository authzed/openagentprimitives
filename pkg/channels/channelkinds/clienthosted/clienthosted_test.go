package clienthosted

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// The kind name is what the shared plumbing is parameterised by, so every test
// here uses a fixture name and asserts it reaches the surfaced error — a failure
// must name the surface the user is looking at, not this package.
const testKind = "demo-surface"

func TestSingleUserAudience(t *testing.T) {
	a := SingleUserAudience{Kind: testKind}
	assert.Equal(t, channelkinds.CapabilitySingleUser, a.AudienceCapability())

	subs, err := a.ResolveAudience(context.Background(),
		channelkinds.SessionInfo{SessionInitiator: identity.CanonicalFromTrusted("user:operator", "test fixture")})
	require.NoError(t, err)
	assert.Equal(t, []string{"user:operator"}, subs)

	_, err = a.ResolveAudience(context.Background(), channelkinds.SessionInfo{})
	require.Error(t, err, "no initiator is a loud error, not an empty audience")
	assert.Contains(t, err.Error(), testKind)
}

func TestListener_StartStop_NoOp(t *testing.T) {
	l := &Listener{Kind: testKind}
	require.NoError(t, l.Start(context.Background()))
	require.NoError(t, l.Stop(context.Background()))
}

// A missing transport must be a loud error naming the kind, never a nil-pointer
// panic — these are the wiring mistakes a host makes, and all four Submit paths
// are reachable before the host has finished coming up.
func TestListener_MissingTransport_FailsLoudlyNamingTheKind(t *testing.T) {
	l := &Listener{Kind: testKind, Namespace: "default", SessionName: "s1"}
	cases := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "SubmitUserMessage with no NATSRequest: names the kind and the transport",
			call: func() error {
				_, err := l.SubmitUserMessage(context.Background(), channelkinds.ExternalIdentity{}, "hi", "")
				return err
			},
			want: "no NATS request transport wired",
		},
		{
			name: "SubmitInteractionDecision with no NATSPublish: names the kind and the transport",
			call: func() error {
				return l.SubmitInteractionDecision(context.Background(), channelkinds.ExternalIdentity{}, "default", "s1", "tool_approval", "req-1", "approve")
			},
			want: "no NATS publish wired",
		},
		{
			name: "SubmitInterrupt with no NATSPublish: names the kind and the transport",
			call: func() error {
				return l.SubmitInterrupt(context.Background(), channelkinds.ExternalIdentity{}, "default", "s1", "req-1")
			},
			want: "no NATS publish wired",
		},
		{
			name: "SubmitResurface with no NATSPublish: names the kind and the transport",
			call: func() error {
				return l.SubmitResurface(context.Background(), "default", "s1")
			},
			want: "no NATS publish wired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), testKind, "the error must name the surface, not this package")
		})
	}
}

// newPublishingListener returns a Listener whose NATSPublish captures the one
// envelope the test publishes.
func newPublishingListener(t *testing.T, subj *string, body *[]byte) *Listener {
	t.Helper()
	return &Listener{
		Kind: testKind,
		Deps: channelkinds.Deps{
			NATSPublish: func(s string, b []byte) error {
				*subj, *body = s, b
				return nil
			},
		},
		Ext: channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"},
		Via: "urn:ap:view:demo",
	}
}

func TestListener_SubmitInteractionDecision_PublishesEnvelope(t *testing.T) {
	var subj string
	var body []byte
	l := newPublishingListener(t, &subj, &body)

	require.NoError(t, l.SubmitInteractionDecision(context.Background(), l.Ext, "default", "s1", "tool_approval", "req-1", "approve"))
	assert.Equal(t, "ap.session.default.s1.in.interaction_decision", subj)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "tool_approval", pl.Category, "category and requestRef must not be transposed")
	assert.Equal(t, "req-1", pl.RequestRef)
	assert.Equal(t, "approve", pl.ActionID)
	assert.Equal(t, "default", pl.AgentSessionRef.Namespace)
	assert.Equal(t, "s1", pl.AgentSessionRef.Name)
	assert.Equal(t, "alice@example.com", pl.Decider.Email.String())
}

// TestListener_SubmitInteractionDecision_StampsCallerExtNotListenerExt is the
// C1 regression: the Decider must come from the ext ARGUMENT, never from
// l.Ext — a *Listener value is reused across calls from different subjects
// (pkg/web/webui/chat's Registry re-attaches one entry's listener to whichever
// subject currently holds standing, not only whoever built it), so reading
// l.Ext here would silently misattribute every decision to whoever the
// listener happened to be constructed for.
func TestListener_SubmitInteractionDecision_StampsCallerExtNotListenerExt(t *testing.T) {
	var body []byte
	l := newPublishingListener(t, new(string), &body)
	l.Ext = channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "bob@example.com", Email: "bob@example.com"}

	caller := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"}
	require.NoError(t, l.SubmitInteractionDecision(context.Background(), caller, "default", "s1", "tool_approval", "req-1", "approve"))

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "alice@example.com", pl.Decider.Email.String(),
		"the Decider must be the ext argument (the caller), not l.Ext (bob@example.com, whoever built this Listener value)")
}

func TestListener_SubmitInterrupt_PublishesRequest(t *testing.T) {
	var subj string
	var body []byte
	l := newPublishingListener(t, &subj, &body)

	require.NoError(t, l.SubmitInterrupt(context.Background(), l.Ext, "default", "s1", "req-1"))
	assert.Equal(t, "ap.session.default.s1.in.interrupt_request", subj)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	var pl channelevents.InterruptRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "req-1", pl.RequestID)
	assert.Equal(t, "default/s1", pl.SessionRef)
	assert.Equal(t, "alice@example.com", pl.Requester.ExternalID.String())
}

// TestListener_SubmitInterrupt_StampsCallerExtNotListenerExt is New-2's
// interrupt leg: the Requester must come from the ext ARGUMENT, never from
// l.Ext — see TestListener_SubmitInteractionDecision_StampsCallerExtNotListenerExt's
// doc for why. TestListener_SubmitInterrupt_PublishesRequest above passes
// l.Ext as the argument, so it cannot tell the two apart; this test uses a
// DIFFERENT identity for each so only the argument can satisfy the
// assertion.
func TestListener_SubmitInterrupt_StampsCallerExtNotListenerExt(t *testing.T) {
	var body []byte
	l := newPublishingListener(t, new(string), &body)
	l.Ext = channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "bob@example.com", Email: "bob@example.com"}

	caller := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"}
	require.NoError(t, l.SubmitInterrupt(context.Background(), caller, "default", "s1", "req-1"))

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	var pl channelevents.InterruptRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "alice@example.com", pl.Requester.ExternalID.String(),
		"the Requester must be the ext argument (the caller), not l.Ext (bob@example.com, whoever built this Listener value)")
}

func TestListener_SubmitResurface_PublishesRequestCarryingVia(t *testing.T) {
	var subj string
	var body []byte
	l := newPublishingListener(t, &subj, &body)

	require.NoError(t, l.SubmitResurface(context.Background(), "default", "s1"))
	assert.Equal(t, "ap.session.default.s1.in.resurface_request", subj)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, channelevents.KindResurfaceRequest, env.Kind)
	var pl channelevents.ResurfaceRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "urn:ap:view:demo", pl.Via, "the request names the surface that attached")
}

// SubmitUserMessage addresses the session from Namespace/SessionName, which are
// distinct from the Channel's own name, and forwards the caller's idempotency key
// verbatim so channelsd can collapse a retried delivery.
func TestListener_SubmitUserMessage_AddressesSessionAndForwardsRequestID(t *testing.T) {
	var gotSubject string
	var gotPayload channelevents.ViewMessagePayload
	l := &Listener{
		Kind:        testKind,
		Namespace:   "default",
		SessionName: "sess-1",
		Via:         "urn:ap:view:demo",
		Ext:         channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"},
		Deps: channelkinds.Deps{
			NATSRequest: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
				gotSubject = subject
				var env channelevents.Envelope
				require.NoError(t, json.Unmarshal(data, &env))
				require.NoError(t, json.Unmarshal(env.Payload, &gotPayload))
				return json.Marshal(channelevents.ViewMessageResultPayload{
					Outcome: channelkinds.OutcomeRouted.String(),
				})
			},
		},
	}

	dec, err := l.SubmitUserMessage(context.Background(), l.Ext, "hello agent", "idem-7")
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, "ap.session.default.sess-1.in.view_message", gotSubject)
	assert.Equal(t, "hello agent", gotPayload.Text)
	assert.Equal(t, "idem-7", gotPayload.RequestID, "the client's idempotency key must reach channelsd")
	assert.Equal(t, "urn:ap:view:demo", gotPayload.Via)
	assert.Equal(t, "alice@example.com", gotPayload.Author.Email.String())
}

// TestListener_SubmitUserMessage_StampsCallerExtNotListenerExt is New-2's
// message leg — C1's own headline scenario ("Alice's message published as
// Bob's"), proven at the exact hop where the bug lived. The Author must
// come from the ext ARGUMENT, never from l.Ext; see
// TestListener_SubmitInteractionDecision_StampsCallerExtNotListenerExt's doc
// for why a *Listener value is reused across subjects.
// TestListener_SubmitUserMessage_AddressesSessionAndForwardsRequestID above
// passes l.Ext as the argument, so it cannot tell the two apart; this test
// uses a DIFFERENT identity for each so only the argument can satisfy the
// assertion.
func TestListener_SubmitUserMessage_StampsCallerExtNotListenerExt(t *testing.T) {
	var gotPayload channelevents.ViewMessagePayload
	l := &Listener{
		Kind: testKind, Namespace: "default", SessionName: "sess-1",
		Ext: channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "bob@example.com", Email: "bob@example.com"},
		Deps: channelkinds.Deps{
			NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
				var env channelevents.Envelope
				require.NoError(t, json.Unmarshal(data, &env))
				require.NoError(t, json.Unmarshal(env.Payload, &gotPayload))
				return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: channelkinds.OutcomeRouted.String()})
			},
		},
	}

	caller := channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"}
	_, err := l.SubmitUserMessage(context.Background(), caller, "hi", "")
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", gotPayload.Author.Email.String(),
		"the Author must be the ext argument (the caller), not l.Ext (bob@example.com, whoever built this Listener value)")
}

// A permission denial is a decision the surface renders, not a transport error.
func TestListener_SubmitUserMessage_PropagatesDenyDecisionWithoutError(t *testing.T) {
	l := &Listener{
		Kind: testKind, Namespace: "default", SessionName: "s1",
		Deps: channelkinds.Deps{
			NATSRequest: func(string, []byte, time.Duration) ([]byte, error) {
				return json.Marshal(channelevents.ViewMessageResultPayload{
					Outcome: channelkinds.OutcomeDeniedByPermission.String(),
					Notice: &channelevents.NoticeWire{
						Category: "not_authorized",
						Tone:     "warning",
						Lead:     "you do not have interact on this session",
					},
				})
			},
		},
	}
	dec, err := l.SubmitUserMessage(context.Background(), l.Ext, "hi", "")
	require.NoError(t, err, "a permission denial is a decision, not a transport error")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	assert.Equal(t, "you do not have interact on this session", dec.Notice.Args().Lead)
}
