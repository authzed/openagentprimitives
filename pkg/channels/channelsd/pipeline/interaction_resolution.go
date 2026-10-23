package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
)

func (p *Pipeline) deliverInteractionResolution(ctx context.Context, record parkedprompt.Content, live ...channelevents.InteractionAppliedPayload) error {
	var env channelevents.Envelope
	if err := json.Unmarshal(record.Resolution, &env); err != nil {
		return err
	}
	if env.Kind != channelevents.KindInteractionApplied {
		return fmt.Errorf("invalid retained interaction resolution")
	}
	if err := outbound.RetainInteractionResolution(ctx, p.Mem, p.K8s, env); err != nil {
		return err
	}
	if len(live) > 0 {
		var canonical channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &canonical); err != nil {
			return err
		}
		// Only transient callbacks may differ from the frozen outcome.
		candidate := live[0]
		candidate.ResponseRef, candidate.MintedURL = "", ""
		if reflect.DeepEqual(candidate, canonical) {
			canonical.ResponseRef, canonical.MintedURL = live[0].ResponseRef, live[0].MintedURL
		}
		raw, err := json.Marshal(canonical)
		if err != nil {
			return err
		}
		env.Payload = raw
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	prefix := channelevents.SubjectPrefix(env.Session.Namespace, env.Session.Name)
	if err := p.NATS.Publish(channelevents.SubjectIn(prefix, env.Kind), raw); err != nil {
		return err
	}
	if err := p.NATS.Publish(channelevents.SubjectOut(prefix, env.Kind), raw); err != nil {
		return err
	}
	return parkedprompt.ResolutionPublished(ctx, p.Mem, promptScope(env.Session.Namespace, env.Session.Name), record)
}

// RecoverInteractionResolutions retries delivery after a storage outage or
// process restart. The decision handler is never invoked during recovery.
func (p *Pipeline) RecoverInteractionResolutions(ctx context.Context, session *spiceboxv1alpha1.AgentSession) error {
	if p.Mem == nil || p.NATS == nil {
		return nil
	}
	records, err := parkedprompt.PendingResolutions(ctx, p.Mem, promptScope(session.Namespace, session.Name))
	if err != nil {
		return err
	}
	var failures []error
	for _, record := range records {
		unlock := p.lockInteraction(session.Namespace, session.Name, record.RequestRef)
		if err := p.deliverInteractionResolution(ctx, record); err != nil {
			failures = append(failures, err)
		}
		unlock()
	}
	return errors.Join(failures...)
}
