package runner

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/stretchr/testify/require"
)

func TestTimeoutPublisherCannotBypassDecisionPipeline(t *testing.T) {
	envelope, err := channelevents.BuildEnvelope("team", "session", channelevents.KindInteractionApplied, channelevents.InteractionAppliedPayload{Category: "tool_approval", RequestRef: "request", Outcome: channelevents.OutcomeExpired})
	require.NoError(t, err)
	var subjects []string
	signed := ""
	err = PublishTimeoutApplied(func(subject string, raw []byte) error {
		subjects = append(subjects, subject)
		var got channelevents.Envelope
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, envelope.Kind, got.Kind)
		return nil
	}, func(subject string, _ *channelevents.Envelope) error { signed = subject; return nil }, "team", "session", envelope)
	require.NoError(t, err)
	expected := channelevents.SubjectIn(channelevents.SubjectPrefix("team", "session"), envelope.Kind)
	require.Equal(t, []string{expected}, subjects, "the UI must receive its winner from channelsd, never directly from the runner")
	require.Equal(t, expected, signed)
	require.ErrorContains(t, PublishTimeoutApplied(func(string, []byte) error { return errors.New("bus unavailable") }, nil, "team", "session", envelope), "bus unavailable")
	require.ErrorContains(t, PublishTimeoutApplied(func(string, []byte) error { t.Fatal("unsigned publish"); return nil }, func(string, *channelevents.Envelope) error { return errors.New("signing failed") }, "team", "session", envelope), "signing failed")
}
