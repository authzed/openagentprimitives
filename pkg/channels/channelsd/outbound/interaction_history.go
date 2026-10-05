package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// noteInteractionHistory runs after routing and attribution have been checked.
// It records the destination conversation, including cards from delegated
// children. Client-hosted transports still need history even though channelsd
// has no sender for them. Regenerated credential links are never archived.
func noteInteractionHistory(ctx context.Context, mem memory.Memory, destination memory.Scope, sourceUID string, env channelevents.Envelope) error {
	if mem == nil {
		return nil
	}
	c := interactionhistory.Content{Source: env.Session, SourceUID: sourceUID, At: env.PublishedAt}
	category := ""
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		var p channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		c.Request = &p
		category = p.Category
	case channelevents.KindInteractionApplied:
		var p channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return err
		}
		if err := p.Validate(); err != nil {
			return err
		}
		// Surface callback URLs and minted credentials are not display history.
		p.ResponseRef = ""
		p.MintedURL = ""
		c.Applied = &p
		category = p.Category
	default:
		return nil
	}
	cat, ok := channelinteractions.Get(category)
	if !ok || cat.Resurface != channelinteractions.ResurfaceCached {
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	id := interactionhistory.IDPrefix + hex.EncodeToString(digest[:])
	if _, err = mem.Put(ctx, memory.Entry{Scope: destination, Kind: interactionhistory.KindName, ID: id, CreatedAt: c.At, Content: raw}); err != nil {
		return fmt.Errorf("record interaction history: %w", err)
	}
	return nil
}

// RetainInteractionResolution resolves the same human destination as the relay.
// Decisions persist history before a live notification can claim success.
func RetainInteractionResolution(ctx context.Context, mem memory.Memory, reader client.Reader, env channelevents.Envelope) error {
	if mem == nil || reader == nil {
		return nil
	}
	var session spiceboxv1alpha1.AgentSession
	if err := reader.Get(ctx, client.ObjectKey{Namespace: env.Session.Namespace, Name: env.Session.Name}, &session); err != nil {
		return err
	}
	target, err := spiceboxv1alpha1.ResolveHumanDirectedBinding(ctx, reader, &session, registry.DeliversToHuman)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("interaction resolution has no human destination")
	}
	return noteInteractionHistory(ctx, mem, memory.Scope{Kind: "session", ID: target.Owner.Namespace + "/" + target.Owner.Name}, string(session.UID), env)
}
