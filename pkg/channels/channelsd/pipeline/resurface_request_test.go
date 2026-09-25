package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// resurfaceRequestEnvelope builds the IN envelope a freshly-attached surface
// publishes, the same way channelsd's subscription hands it to the handler.
func resurfaceRequestEnvelope(t *testing.T, ns, name, via string) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindResurfaceRequest,
		channelevents.ResurfaceRequestPayload{Via: via})
	require.NoError(t, err, "build resurface_request envelope")
	return env
}

// TestHandleResurfaceRequest is the regression test for a credential prompt
// lost to a late subscriber: delivery of a parked prompt is a one-shot live
// publish, and a browser tab that attaches AFTER it went out saw nothing at
// all — with the CredentialRequestPublished dedup guaranteeing it would never
// be re-sent. A surface that just attached asks for the parked prompt to be
// re-surfaced; the handler runs the SAME resurfacePending machinery an inbound
// user message already triggers, so every registered park category is covered
// with no per-category branch here.
func TestHandleResurfaceRequest(t *testing.T) {
	const ns, name = "ns", "s"
	sessKey := client.ObjectKey{Namespace: ns, Name: name}

	// newSessOn seeds a session at phase and returns a pipeline wired to a
	// cached credential_link-shaped (regenerate) prompt for it.
	newSessOn := func(t *testing.T, phase string) (*Pipeline, *fakeNATS, *int) {
		t.Helper()
		c := registerRegenerateCategory(t)
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, c.Name, "cred-req-1",
			channelevents.InteractionRequestPayload{Lead: "Connect your accounts"})
		calls := 0
		channelinteractions.BindRegenerator(c.Name, func(context.Context, *spiceboxv1alpha1.AgentSession, *channelevents.InteractionRequestPayload) error {
			calls++
			return nil
		})
		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
		}
		p, _, _, nats, _ := newPipeline(t, sess)
		p.Mem = mem
		return p, nats, &calls
	}

	t.Run("parked session: the parked prompt is re-surfaced to the newly-attached surface", func(t *testing.T) {
		p, _, calls := newSessOn(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)

		err := p.HandleResurfaceRequest(context.Background(), resurfaceRequestEnvelope(t, ns, name, "urn:ap:view:chat"))

		require.NoError(t, err)
		assert.Equal(t, 1, *calls, "the parked category's regenerator must run so the prompt is re-delivered")
	})

	t.Run("running session: nothing is re-surfaced", func(t *testing.T) {
		p, nats, calls := newSessOn(t, spiceboxv1alpha1.AgentSessionPhaseRunning)

		err := p.HandleResurfaceRequest(context.Background(), resurfaceRequestEnvelope(t, ns, name, "urn:ap:view:chat"))

		require.NoError(t, err)
		assert.Zero(t, *calls, "a session that is not parked has no prompt to re-surface")
		assert.Empty(t, nats.subjects)
	})

	t.Run("unknown session: returns an error rather than silently doing nothing", func(t *testing.T) {
		p, _, _ := newSessOn(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)

		err := p.HandleResurfaceRequest(context.Background(), resurfaceRequestEnvelope(t, ns, "nope", ""))

		require.Error(t, err)
	})

	t.Run("malformed via: rejected fail-closed, nothing re-surfaced", func(t *testing.T) {
		p, _, calls := newSessOn(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)

		err := p.HandleResurfaceRequest(context.Background(), resurfaceRequestEnvelope(t, ns, name, "not-a-view-urn"))

		require.Error(t, err)
		assert.Zero(t, *calls)
	})

	t.Run("wrong envelope kind: rejected", func(t *testing.T) {
		p, _, _ := newSessOn(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)
		env := resurfaceRequestEnvelope(t, ns, name, "")
		env.Kind = channelevents.KindUserMessage

		require.Error(t, p.HandleResurfaceRequest(context.Background(), env))
	})
}
