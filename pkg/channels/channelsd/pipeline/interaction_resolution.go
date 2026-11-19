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

// retainResolution freezes applied as the parked prompt's canonical outcome and
// resolves the prompt with it, returning the record for
// deliverInteractionResolution. The transient per-click callbacks (ResponseRef,
// MintedURL) are stripped: the frozen copy is what every later reader sees.
func (p *Pipeline) retainResolution(ctx context.Context, applied channelevents.InteractionAppliedPayload) (parkedprompt.Content, error) {
	ns, name := applied.AgentSessionRef.Namespace, applied.AgentSessionRef.Name
	applied.ResponseRef, applied.MintedURL = "", ""
	canonical, err := channelevents.BuildEnvelope(ns, name, channelevents.KindInteractionApplied, applied)
	if err != nil {
		return parkedprompt.Content{}, err
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return parkedprompt.Content{}, err
	}
	return parkedprompt.ResolveWithOutcome(ctx, p.Mem, promptScope(ns, name), applied.RequestRef, raw)
}

// resolvedAsExpired reports whether a resolved parked prompt's retained
// outcome is an expiry, so a later click can be told the request expired
// rather than that someone else decided it. A record with no retained outcome
// (a plain tombstone) is not an expiry.
func resolvedAsExpired(record parkedprompt.Content) (bool, error) {
	if len(record.Resolution) == 0 {
		return false, nil
	}
	var env channelevents.Envelope
	if err := json.Unmarshal(record.Resolution, &env); err != nil {
		return false, err
	}
	var applied channelevents.InteractionAppliedPayload
	if err := json.Unmarshal(env.Payload, &applied); err != nil {
		return false, err
	}
	return applied.Outcome == channelevents.OutcomeExpired, nil
}

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
