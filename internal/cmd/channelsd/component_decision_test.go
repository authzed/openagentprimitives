package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComponentDecisionIngressBindsTheBrokerSubject(t *testing.T) {
	for _, tc := range []struct {
		name, subject string
		kind          channelevents.Kind
		session       string
		want          bool
	}{
		{"verified transport", "ap.component.interaction_decision.team.source", channelevents.KindInteractionDecision, "source", true},
		{"runner tree", "ap.session.team.source.in.interaction_decision", channelevents.KindInteractionDecision, "source", false},
		{"forged target", "ap.component.interaction_decision.team.source", channelevents.KindInteractionDecision, "victim", false},
		{"wrong kind", "ap.component.interaction_decision.team.source", channelevents.KindUserMessage, "source", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := componentDecisionHandler(testLogger(t), func(ctx context.Context, _ channelevents.Envelope) error {
				called = true
				assert.True(t, channelevents.ComponentDecisionIngress(ctx))
				return nil
			})
			env := channelevents.Envelope{Kind: tc.kind, Session: channelevents.SessionRef{Namespace: "team", Name: tc.session}}
			raw, err := json.Marshal(env)
			require.NoError(t, err)
			handler(&nats.Msg{Subject: tc.subject, Data: raw})
			assert.Equal(t, tc.want, called)
		})
	}
}
