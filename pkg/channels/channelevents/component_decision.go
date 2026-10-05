package channelevents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
)

type componentDecisionIngress struct{}

// WithComponentDecisionIngress is called only after channelsd has matched a
// component-only broker subject against the envelope's session. It carries no
// credential and cannot be set through an envelope's JSON.
func WithComponentDecisionIngress(ctx context.Context) context.Context {
	return context.WithValue(ctx, componentDecisionIngress{}, true)
}

func ComponentDecisionIngress(ctx context.Context) bool {
	verified, _ := ctx.Value(componentDecisionIngress{}).(bool)
	return verified
}

func PublishComponentDecision(publish PublishFunc, ns, name string, payload InteractionDecisionPayload) error {
	if publish == nil {
		return fmt.Errorf("component decision publisher unavailable")
	}
	env, err := BuildEnvelope(ns, name, KindInteractionDecision, payload)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return publish(subjects.ComponentDecision(ns, name), raw)
}
