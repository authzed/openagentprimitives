package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

// HandleResurfaceRequest services ap.session.<ns>.<name>.in.resurface_request —
// a view of the session announcing that it has just attached and asking for
// whatever the session is parked on to be re-delivered.
//
// It exists because delivery of a parked prompt is a one-shot live publish.
// The publisher (the credential-link watcher, a runner approval gate) fires
// once, the outbound relay fans it to whoever is subscribed at that instant,
// and a per-prompt dedup — credential_link's CredentialRequestPublished
// condition — guarantees it is never sent again. A surface that attaches after
// that moment therefore shows a durably-parked session with NO card, no error
// and no spinner: the exact silent hang AGENTS.md's no-silent-errors rule
// exists to prevent. Until this handler, the only trigger for the re-send
// machinery was an inbound user message (resurfacePending, called from
// Deliver), which the user waiting on the prompt has no reason to send.
//
// The work itself is entirely resurfacePending's — the same call Deliver
// makes, with the same per-category Park/Resurface handling and the same
// durable stale-guards. There is deliberately no per-category branch here: a
// category that resurfaces on re-interaction resurfaces on attach, and a new
// one is a row in the registry, not an edit to this file. A session that is
// not parked (or has nothing cached) is a silent no-op inside resurfacePending.
//
// On authorization, see ResurfaceRequestPayload's doc: the request carries no
// actor claim and gates on none, because it confers nothing on its publisher —
// the re-published prompt goes to the audience the prompt itself names. WHICH
// session is resurfaced is still gated, though: env.Session is trustworthy here
// only because internal/cmd/channelsd/main.go's envelopeHandler cross-checked it against
// the NATS subject first and dropped any mismatch.
func (p *Pipeline) HandleResurfaceRequest(ctx context.Context, env channelevents.Envelope) error {
	if env.Kind != channelevents.KindResurfaceRequest {
		return fmt.Errorf("HandleResurfaceRequest: unexpected kind %q", env.Kind)
	}
	ns, name := env.Session.Namespace, env.Session.Name
	var pl channelevents.ResurfaceRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("resurface_request: decode payload (session %s/%s): %w", ns, name, err)
	}
	// Via is server-minted by webd/ap, but re-validate fail-closed here for the
	// same reason view_message does: a malformed Via on the wire is a crafted
	// publish, and Via is logged. Empty is tolerated (surfaces with no URN).
	if pl.Via != "" {
		if _, err := viewurn.Parse(pl.Via); err != nil {
			return fmt.Errorf("resurface_request: invalid via %q (session %s/%s): %w", pl.Via, ns, name, err)
		}
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return fmt.Errorf("resurface_request: get session %s/%s: %w", ns, name, err)
	}

	// One line per attach, carrying the surface and the phase — this is what
	// makes both a missing card and a resurface storm diagnosable. It says a
	// request arrived, not that anything was re-sent: resurfacePending is a
	// no-op unless the session is parked with a matching cached prompt.
	log.FromContext(ctx).Info("resurface_request received from a newly-attached surface",
		"session", ns+"/"+name, "phase", sess.Status.Phase, "via", pl.Via)
	p.resurfacePending(ctx, &sess)
	return nil
}
