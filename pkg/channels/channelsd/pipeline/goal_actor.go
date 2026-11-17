package pipeline

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GoalActorRecorder is a trusted component write, separate from runner turns.
// inboundRef names the inbound being attested (see newInboundRef).
type GoalActorRecorder func(ctx context.Context, sess *v1.AgentSession, class *v1.AgentClass, owner identity.CanonicalUserID, inboundRef string) error

// newInboundRef mints the per-inbound ref shared by a message's goal_actor
// attestation and the inbox item the runner drains for it. Minted here rather
// than derived from the transport: the transcript index is not assigned until
// after the attestation is written, and most channel kinds report no stable
// message ID (InboundEvent.MsgRef is empty for them). The ref selects an
// attestation; it is never itself evidence of who spoke.
func newInboundRef() string { return rand.Text() }

// recordGoalActor runs before a message can wake the runner. A new session
// racing this write cannot use goals until its first trusted proof exists.
// inboundRef must be the same value carried on the inbox item appended for
// this message, or goalactor.InitialPromptRef for a session's opening prompt.
func (p *Pipeline) recordGoalActor(ctx context.Context, sess *v1.AgentSession, ev channelkinds.InboundEvent, inboundRef string) error {
	if p.RecordGoalActor == nil || !ev.ExternalIDs.HasIdentity() || canonicalID(ev.ExternalIDs).IsZero() || sess.Spec.Parent != nil {
		return nil
	}
	var class v1.AgentClass
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		return fmt.Errorf("record goal actor: class lookup: %w", err)
	}
	grant, err := agentcaps.GrantOf(&class, "goals")
	if err != nil {
		return err
	}
	if !agentcaps.Active(false, grant) {
		return nil
	}
	if err := p.RecordGoalActor(ctx, sess, &class, canonicalID(ev.ExternalIDs), inboundRef); err != nil {
		return fmt.Errorf("record goal actor: %w", err)
	}
	return nil
}
