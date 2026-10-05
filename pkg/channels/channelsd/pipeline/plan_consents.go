package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// All child requests are preflighted before any handler commits a decision.
// A partial downstream failure keeps the parent pending; children retry using
// their durable decision identities, without extending existing grants.
func (p *Pipeline) decideWithConsents(ctx context.Context, session channelevents.SessionRef, payload channelevents.InteractionDecisionPayload, request *channelevents.InteractionRequestPayload, handler channelinteractions.DecisionHandler) (channelinteractions.Outcome, error) {
	if request != nil && len(request.Consents) > 0 {
		if !channelevents.ComponentDecisionIngress(ctx) {
			return channelinteractions.Outcome{}, fmt.Errorf("plan consent requires verified component ingress")
		}
		cat, ok := channelinteractions.Get(payload.Category)
		if !ok || !cat.IncludesConsents {
			return channelinteractions.Outcome{}, fmt.Errorf("category cannot include plan consents")
		}
		if payload.ActionID != "approve" && payload.ActionID != "deny" {
			return channelinteractions.Outcome{}, fmt.Errorf("invalid plan consent action")
		}
		if request.AgentSessionRef != session || request.Category != payload.Category || request.RequestRef != payload.RequestRef {
			return channelinteractions.Outcome{}, fmt.Errorf("plan consent request identity mismatch")
		}
		if request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) {
			return channelinteractions.Outcome{}, fmt.Errorf("plan consent approval expired")
		}
		if err := request.ValidatePlanConsents(); err != nil {
			return channelinteractions.Outcome{}, err
		}
		for _, child := range request.Consents {
			cat, ok := channelinteractions.Get(child.Category)
			if !ok || !cat.PlanConsent || cat.Deciders != channelinteractions.DecideRequester || child.Audience.Requester == nil {
				return channelinteractions.Outcome{}, fmt.Errorf("unsupported plan consent category")
			}
			owner, err := child.Audience.Requester.Principal().Canonical()
			if err != nil {
				return channelinteractions.Outcome{}, err
			}
			decider, err := payload.Decider.Principal().Canonical()
			if err != nil {
				return channelinteractions.Outcome{}, err
			}
			if owner != decider {
				return channelinteractions.Outcome{}, fmt.Errorf("plan consent requires its addressed human")
			}
			validator, bound := channelinteractions.ValidatorFor(child.Category)
			if !bound {
				return channelinteractions.Outcome{}, fmt.Errorf("plan consent validator unavailable")
			}
			if _, bound := channelinteractions.HandlerFor(child.Category); !bound {
				return channelinteractions.Outcome{}, fmt.Errorf("plan consent handler unavailable")
			}
			decision := payload
			decision.Category, decision.RequestRef = child.Category, child.RequestRef
			if err := validator(ctx, channelinteractions.Decision{Session: session, Payload: decision, Request: &child}); err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf("validate included consent: %w", err)
			}
		}
		for _, child := range request.Consents {
			childHandler, _ := channelinteractions.HandlerFor(child.Category)
			decision := payload
			decision.Category, decision.RequestRef = child.Category, child.RequestRef
			out, err := childHandler(ctx, channelinteractions.Decision{Session: session, Payload: decision, Request: &child})
			if err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf("included consent: %w", err)
			}
			if err := out.Validate(); err != nil {
				return channelinteractions.Outcome{}, err
			}
			expected := channelevents.OutcomeDenied
			if payload.ActionID == "approve" {
				expected = channelevents.OutcomeApproved
			}
			if out.Suppressed || out.Result != expected {
				return channelinteractions.Outcome{}, fmt.Errorf("included consent was not applied")
			}
		}
	}
	return handler(ctx, channelinteractions.Decision{Session: session, Payload: payload, Request: request})
}
