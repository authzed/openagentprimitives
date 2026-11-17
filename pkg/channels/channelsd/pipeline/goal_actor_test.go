package pipeline

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalactor"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGoalActorWrittenBeforeAcceptedTurnAndWake(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "attestation unavailable"}[fail], func(t *testing.T) {
			ch := newChannel("c1")
			sess := existingSession(t, "c1-abc", "", v1.AgentSessionPhaseRunning)
			p, _, mem, nats, k8s := newPipeline(t, ch, sess)
			var class v1.AgentClass
			ctx := context.Background()
			require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ac1"}, &class))
			class.Spec.Capabilities = map[string]apiext.JSON{"goals": {Raw: []byte(`{}`)}}
			require.NoError(t, k8s.Update(ctx, &class))
			calls := 0
			var recordedRef string
			p.RecordGoalActor = func(_ context.Context, s *v1.AgentSession, c *v1.AgentClass, u identity.CanonicalUserID, inboundRef string) error {
				calls++
				recordedRef = inboundRef
				assert.Empty(t, mem.appends)
				assert.Empty(t, nats.subjects)
				assert.Equal(t, sess.Name, s.Name)
				assert.Equal(t, class.Name, c.Name)
				assert.False(t, u.IsZero())
				if fail {
					return errors.New("attestation service unavailable")
				}
				return nil
			}
			d, err := p.Deliver(ctx, channelkinds.InboundEvent{Channel: ch, ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"}, ChannelKey: "thread:C1:1", MessageText: "Remember my goal"})
			require.Equal(t, 1, calls)
			if fail {
				require.Error(t, err)
				assert.Equal(t, channelkinds.OutcomeInternalError, d.Outcome)
				assert.Empty(t, mem.appends)
				assert.Empty(t, nats.subjects)
			} else {
				require.NoError(t, err)
				assert.Equal(t, channelkinds.OutcomeRouted, d.Outcome)
				require.Len(t, mem.appends, 1)
				assert.NotEmpty(t, nats.subjects)
				// The attestation and the inbox item the runner drains carry
				// the same per-inbound ref, so the runner can name the
				// attestation that started its turn.
				assert.NotEmpty(t, recordedRef)
				assert.Equal(t, recordedRef, mem.appends[0].turn.InboundRef)
			}
		})
	}
}

// The browser's opening prompt is already on spec.prompt.inline. Its actor
// must be attested without creating a second turn, and authorization must be
// checked again even when a publisher reuses a request ID.
func TestOpeningViewActorAttestation(t *testing.T) {
	for _, mode := range []string{"accepted", "denied", "check error", "write error", "no authz"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			p, mem := newTestPipelineWithSession(t, "default", "sess")
			var sess v1.AgentSession
			require.NoError(t, p.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sess"}, &sess))
			sess.Spec.Class = "ac1"
			require.NoError(t, p.K8s.Update(ctx, &sess))
			var class v1.AgentClass
			require.NoError(t, p.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ac1"}, &class))
			class.Spec.Capabilities = map[string]apiext.JSON{"goals": {Raw: []byte(`{}`)}}
			require.NoError(t, p.K8s.Update(ctx, &class))
			az := p.Authz.(*fakeAuthz)
			switch mode {
			case "denied":
				az.checkResult = false
			case "check error":
				az.checkErr = errors.New("authorization unavailable")
			case "no authz":
				p.Authz = nil
			}
			calls := 0
			p.RecordGoalActor = func(_ context.Context, _ *v1.AgentSession, _ *v1.AgentClass, u identity.CanonicalUserID, inboundRef string) error {
				calls++
				// The opening prompt is the runner's turn 0, which no inbox
				// item carries; it is named by the fixed initial-prompt ref.
				assert.Equal(t, goalactor.InitialPromptRef, inboundRef)
				assert.Equal(t, canonOf("alice@example.com"), u.String())
				if mode == "write error" {
					return errors.New("memory unavailable")
				}
				return nil
			}
			env, err := channelevents.BuildEnvelope("default", "sess", channelevents.KindViewMessage, channelevents.ViewMessagePayload{AttestOnly: true, RequestID: "same", Author: channelevents.ExternalIdentity{Kind: "idp", Email: "alice@example.com"}})
			require.NoError(t, err)
			res, err := p.HandleViewMessage(ctx, env)
			switch mode {
			case "accepted":
				require.NoError(t, err)
				assert.Equal(t, "routed", res.Outcome)
				assert.Equal(t, 1, calls)
				az.checkResult = false
				res, err = p.HandleViewMessage(ctx, env)
				require.NoError(t, err)
				assert.Equal(t, channelkinds.OutcomeDeniedByPermission.String(), res.Outcome)
				assert.Equal(t, 1, calls)
			case "denied":
				require.NoError(t, err)
				assert.Equal(t, channelkinds.OutcomeDeniedByPermission.String(), res.Outcome)
				assert.Zero(t, calls)
			default:
				require.Error(t, err)
				assert.NotEmpty(t, res.Error)
			}
			assert.Empty(t, mem.appends, "opening prompt must not be delivered twice")
			assert.Empty(t, p.NATS.(*fakeNATS).subjects, "attestation must not wake the runner")
		})
	}
}

// Each inbound gets its own ref: two messages in one session must be
// distinguishable, or a turn could select a later speaker's attestation.
func TestGoalActorRefIsPerInbound(t *testing.T) {
	ch := newChannel("c1")
	sess := existingSession(t, "c1-abc", "", v1.AgentSessionPhaseRunning)
	p, _, mem, _, k8s := newPipeline(t, ch, sess)
	ctx := context.Background()
	var class v1.AgentClass
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ac1"}, &class))
	class.Spec.Capabilities = map[string]apiext.JSON{"goals": {Raw: []byte(`{}`)}}
	require.NoError(t, k8s.Update(ctx, &class))
	var refs []string
	p.RecordGoalActor = func(_ context.Context, _ *v1.AgentSession, _ *v1.AgentClass, _ identity.CanonicalUserID, inboundRef string) error {
		refs = append(refs, inboundRef)
		return nil
	}
	for _, text := range []string{"first", "second"} {
		_, err := p.Deliver(ctx, channelkinds.InboundEvent{Channel: ch, ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"}, ChannelKey: "thread:C1:1", MessageText: text})
		require.NoError(t, err)
	}
	require.Len(t, refs, 2)
	require.Len(t, mem.appends, 2)
	assert.NotEqual(t, refs[0], refs[1])
	assert.Equal(t, refs[0], mem.appends[0].turn.InboundRef)
	assert.Equal(t, refs[1], mem.appends[1].turn.InboundRef)
}
