package plangate

import (
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsentAuthoritySurvivesDurableReplay(t *testing.T) {
	p, problems := FreezeFrom([]AuthoredPhase{{ID: "schedule", Label: "Schedule reminders", Consents: []json.RawMessage{json.RawMessage(`{"requestRef":"one","dueAt":"2030-01-01T12:00:00Z","recipient":"alice"}`)}}}, nil, nil)
	require.Empty(t, problems)
	declaration := PhaseAuthorityRecord(p, 0, nil)
	declaration.Event, declaration.PlanDigest = plangateaudit.EventPlanApproved, p.Digest()
	grant := declaration
	grant.Event = plangateaudit.EventPhaseApproved
	encoded, err := json.Marshal([]plangateaudit.Content{declaration, grant})
	require.NoError(t, err)
	var replay []plangateaudit.Content
	require.NoError(t, json.Unmarshal(encoded, &replay))
	restored, ok := PlanForDigest(replay, p.Digest())
	require.True(t, ok)
	assert.Equal(t, p.Digest(), restored.Digest())
	assert.Equal(t, p.Phases[0].AuthorityKey(), restored.Phases[0].AuthorityKey())
	state, err := Fold(restored, replay)
	require.NoError(t, err)
	assert.True(t, state.PhaseApproved(0))
	for _, tc := range []struct{ name, request string }{
		{"new request needs approval", `{"requestRef":"two","dueAt":"2030-01-01T12:00:00Z","recipient":"alice"}`},
		{"new time needs approval", `{"requestRef":"one","dueAt":"2030-01-01T13:00:00Z","recipient":"alice"}`},
		{"new recipient needs approval", `{"requestRef":"one","dueAt":"2030-01-01T12:00:00Z","recipient":"bob"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := Plan{Phases: []Phase{p.Phases[0]}}
			changed.Phases[0].Consents = []json.RawMessage{json.RawMessage(tc.request)}
			assert.NotEqual(t, p.Digest(), changed.Digest())
			assert.NotEqual(t, p.Phases[0].AuthorityKey(), changed.Phases[0].AuthorityKey())
			assert.False(t, CarriesOver(changed, 0, replay))
			state, err := Fold(changed, replay)
			require.NoError(t, err)
			assert.False(t, state.PhaseApproved(0))
		})
	}
}

func TestConsentFreezeCopiesCanonicalAuthority(t *testing.T) {
	raw := json.RawMessage(`{ "requestRef": "one", "instructions": "<private> & exact" }`)
	p, problems := FreezeFrom([]AuthoredPhase{{ID: "one", Consents: []json.RawMessage{raw}}}, nil, nil)
	require.Empty(t, problems)
	digest := p.Digest()
	raw[0] = '['
	assert.Equal(t, digest, p.Digest(), "authored buffers cannot rewrite frozen authority")
	encoded, err := json.Marshal(p)
	require.NoError(t, err)
	var restored Plan
	require.NoError(t, json.Unmarshal(encoded, &restored))
	assert.Equal(t, digest, restored.Digest())
	changed := p
	changed.Phases = append([]Phase(nil), p.Phases...)
	changed.Phases[0].Consents = append([]json.RawMessage{json.RawMessage(`{"requestRef":"two"}`)}, p.Phases[0].Consents...)
	assert.NotEqual(t, digest, changed.Digest())
	key := changed.Phases[0].AuthorityKey()
	changed.Phases[0].Consents[0], changed.Phases[0].Consents[1] = changed.Phases[0].Consents[1], changed.Phases[0].Consents[0]
	assert.NotEqual(t, key, changed.Phases[0].AuthorityKey(), "request ordering is reviewed authority")
}
