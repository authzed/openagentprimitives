package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// subagentInteractionPublish returns the publisher the SubagentRequest
// controller uses to ASK a human about a data slot's disclosure.
//
// IN, not OUT — the same routing the runner's own interaction requests take
// (see internal/cmd/runner's InteractionRequestPublish). channelsd's
// HandleInteractionRequest subscribes to in.interaction_request, parks the
// session, writes the durable approval record, and re-emits on OUT for the
// renderer. Publishing on OUT directly would render a card with no park and no
// durable record, so a channelsd restart between showing it and the click
// would lose the request the decision refers to.
//
// A nil connection returns a nil publisher rather than a func that errors on
// every call. The controller's own nil-check then logs once, at the point it
// would have asked, saying nobody will be asked and the request will expire —
// which is the sentence an operator needs. A publisher that failed per-call
// would say it N times and never say why.
func subagentInteractionPublish(nc *nats.Conn) func(context.Context, string, string, channelevents.Envelope) error {
	if nc == nil {
		return nil
	}
	return func(_ context.Context, envNS, envName string, env channelevents.Envelope) error {
		body, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("marshal data-slot disclosure envelope: %w", err)
		}
		subject := channelevents.SubjectIn(channelevents.SubjectPrefix(envNS, envName), env.Kind)
		return nc.Publish(subject, body)
	}
}
