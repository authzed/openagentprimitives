package channelkinds

import (
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// PublishResurfaceRequest asks channelsd to re-deliver whatever prompt the
// session is currently parked on. A view surface calls it the moment it
// attaches: the browser chat when a tab opens its websocket, the TUI once its
// outbound relay is subscribed.
//
// It exists because a parked prompt is delivered by a ONE-SHOT live publish
// that its own dedup guarantees is never repeated — credential_link stamps
// CredentialRequestPublished and the watcher skips the session forever after.
// A surface that attaches later therefore shows a durably-parked session with
// no card, no error, and no spinner, and the only other trigger for the
// re-send machinery is an inbound user message the waiting user has no reason
// to send. Both view surfaces have that gap for the same reason — each creates
// the session, waits for it, and only then subscribes — so this is the ONE
// helper both call.
//
// Fire-and-forget: unlike view_message there is no reply to wait for. The
// caller logs a failure (the user may miss a card) but must not fail the
// attach over it.
func PublishResurfaceRequest(deps Deps, ns, name, via string) error {
	if deps.NATSPublish == nil {
		return errors.New("channelkinds: PublishResurfaceRequest: no NATS publish wired")
	}
	if err := channelevents.PublishIn(deps.NATSPublish, ns, name,
		channelevents.KindResurfaceRequest,
		channelevents.ResurfaceRequestPayload{Via: via},
	); err != nil {
		return fmt.Errorf("channelkinds: publish resurface_request: %w", err)
	}
	return nil
}
