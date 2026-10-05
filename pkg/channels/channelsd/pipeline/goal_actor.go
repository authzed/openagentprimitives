package pipeline

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GoalActorRecorder is a trusted component write, separate from runner turns.
type GoalActorRecorder func(context.Context, *v1.AgentSession, *v1.AgentClass, identity.CanonicalUserID) error

// recordGoalActor runs before a message can wake the runner. A new session
// racing this write cannot use goals until its first trusted proof exists.
func (p *Pipeline) recordGoalActor(ctx context.Context, sess *v1.AgentSession, ev channelkinds.InboundEvent) error {
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
	if err := p.RecordGoalActor(ctx, sess, &class, canonicalID(ev.ExternalIDs)); err != nil {
		return fmt.Errorf("record goal actor: %w", err)
	}
	return nil
}
