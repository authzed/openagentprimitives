package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
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
