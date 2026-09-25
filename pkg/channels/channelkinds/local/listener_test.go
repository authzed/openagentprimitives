package local

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// newListener builds this kind's Listener directly, bypassing the Host, with the
// shared plumbing pre-bound to this kind's name — so a test wires only the
// Deps/identity it actually exercises.
func newListener(l clienthosted.Listener) *localListener {
	l.Kind = KindName
	return &localListener{Listener: l}
}

func testChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1ObjectMeta("default", "chat-local"),
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
}

func TestListener_SubmitInterrupt_PublishesRequest(t *testing.T) {
	var gotSubj string
	var gotBody []byte
	l := newListener(clienthosted.Listener{
		Deps: channelkinds.Deps{
			Channel: testChannel(),
			NATSPublish: func(subj string, body []byte) error {
				gotSubj, gotBody = subj, body
				return nil
			},
		},
		Ext: channelkinds.ExternalIdentity{Kind: "local", ExternalID: "local-user"},
	})
	err := l.SubmitInterrupt(context.Background(), l.Listener.Ext, "default", "s1", "req-1")
	require.NoError(t, err)
	assert.Contains(t, gotSubj, "ap.session.default.s1.in.interrupt_request")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(gotBody, &env))
	assert.Equal(t, channelevents.KindInterruptRequest, env.Kind)
	var pl channelevents.InterruptRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "req-1", pl.RequestID)
	assert.Equal(t, "default/s1", pl.SessionRef)
	assert.Equal(t, "local-user", pl.Requester.ExternalID.String())
}

// TestListener_SubmitResurface_PublishesRequest: the TUI's relay only
// subscribes after the session is created and waited on, so it asks for the
// parked prompt the moment it can receive one. The envelope's ns/name IS the
// session; the payload carries only the surface's view URN.
func TestListener_SubmitResurface_PublishesRequest(t *testing.T) {
	var gotSubj string
	var gotBody []byte
	l := newListener(clienthosted.Listener{
		Deps: channelkinds.Deps{
			Channel: testChannel(),
			NATSPublish: func(subj string, body []byte) error {
				gotSubj, gotBody = subj, body
				return nil
			},
		},
		Ext: channelkinds.ExternalIdentity{Kind: "local", ExternalID: "local-user"},
		Via: "urn:ap:view:tui",
	})
	require.NoError(t, l.SubmitResurface(context.Background(), "default", "s1"))
	assert.Contains(t, gotSubj, "ap.session.default.s1.in.resurface_request")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(gotBody, &env))
	assert.Equal(t, channelevents.KindResurfaceRequest, env.Kind)
	var pl channelevents.ResurfaceRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "urn:ap:view:tui", pl.Via, "the request names the surface that attached")
}

func TestListener_SubmitResurface_WithoutTransportFailsLoudly(t *testing.T) {
	l := newListener(clienthosted.Listener{Deps: channelkinds.Deps{}})
	err := l.SubmitResurface(context.Background(), "default", "s1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NATS publish wired")
}

func TestListener_SubmitInterrupt_WithoutTransportFailsLoudly(t *testing.T) {
	l := newListener(clienthosted.Listener{Deps: channelkinds.Deps{}})
	err := l.SubmitInterrupt(context.Background(), l.Listener.Ext, "default", "s1", "req-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NATS publish wired")
}

func TestListener_StartStop_NoOp(t *testing.T) {
	l := newListener(clienthosted.Listener{Deps: channelkinds.Deps{Channel: testChannel()}})
	require.NoError(t, l.Start(context.Background()))
	require.NoError(t, l.Stop(context.Background()))
}

func metav1ObjectMeta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name}
}

func TestSubmitUserMessageRequestsChannelsdAndReturnsDecision(t *testing.T) {
	var gotSubject string
	l := newListener(clienthosted.Listener{
		Deps: channelkinds.Deps{
			NATSRequest: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
				gotSubject = subject
				var env channelevents.Envelope
				require.NoError(t, json.Unmarshal(data, &env))
				var pl channelevents.ViewMessagePayload
				require.NoError(t, json.Unmarshal(env.Payload, &pl))
				assert.Equal(t, "hello agent", pl.Text)
				assert.Equal(t, "alice@example.com", pl.Author.Email.String())
				return json.Marshal(channelevents.ViewMessageResultPayload{
					Outcome: channelkinds.OutcomeRouted.String(),
				})
			},
		},
		Ext:         channelkinds.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"},
		ChannelKey:  "local:sess-1",
		Namespace:   "default",
		SessionName: "sess-1",
	})

	dec, err := l.SubmitUserMessage(context.Background(), "hello agent")
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Equal(t, "ap.session.default.sess-1.in.view_message", gotSubject)
}

// The listener must NOT hold an in-process pipeline. Regression guard for the
// audit-forgery hole: a nil Inbound is now normal, and a nil NATSRequest must
// be a loud wiring error rather than a nil-pointer panic.
func TestSubmitUserMessageWithoutTransportFailsLoudly(t *testing.T) {
	l := newListener(clienthosted.Listener{Deps: channelkinds.Deps{}, Namespace: "default", SessionName: "s"})
	_, err := l.SubmitUserMessage(context.Background(), "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NATS request transport wired")
}

func TestSubmitUserMessagePropagatesDenyMessage(t *testing.T) {
	l := newListener(clienthosted.Listener{
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
		Namespace: "default", SessionName: "s",
	})
	dec, err := l.SubmitUserMessage(context.Background(), "hi")
	require.NoError(t, err, "a permission denial is a decision, not a transport error")
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	assert.Equal(t, "you do not have interact on this session", dec.Notice.Args().Lead)
}

func TestSubmitUserMessageSendsHostVia(t *testing.T) {
	var gotVia string
	l := newListener(clienthosted.Listener{
		Deps: channelkinds.Deps{
			NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
				var env channelevents.Envelope
				require.NoError(t, json.Unmarshal(data, &env))
				var pl channelevents.ViewMessagePayload
				require.NoError(t, json.Unmarshal(env.Payload, &pl))
				gotVia = pl.Via
				return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: channelkinds.OutcomeRouted.String()})
			},
		},
		Namespace: "default", SessionName: "s", Via: "urn:ap:view:tui",
	})
	_, err := l.SubmitUserMessage(context.Background(), "hi")
	require.NoError(t, err)
	assert.Equal(t, "urn:ap:view:tui", gotVia)
}
