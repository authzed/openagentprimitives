package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories" // registers the notice row
)

var sampleBypass = completion.Bypass{
	Reason: "the html render never came back ready; a degraded review is still a review",
	Unmet: []completion.Unmet{{
		Key:     "artifact-delivered",
		Title:   "every file the agent produced reaches you",
		Missing: "1 artifact(s) you rendered were never delivered: ar-review-1-abc",
	}},
}

// capturingPublish records every published envelope.
type capturingPublish struct {
	subjects []string
	payloads [][]byte
	err      error
}

func (c *capturingPublish) fn(subject string, data []byte) error {
	if c.err != nil {
		return c.err
	}
	c.subjects = append(c.subjects, subject)
	c.payloads = append(c.payloads, data)
	return nil
}

func decodeInteraction(t *testing.T, raw []byte) channelevents.InteractionRequestPayload {
	t.Helper()
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(raw, &env), "envelope must be valid JSON")
	require.Equal(t, channelevents.KindInteractionRequest, env.Kind,
		"a notice rides the interaction-request wire kind, like every other one-way message")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "payload must decode")
	return pl
}

func TestCompletionBypassRecorder_RecordsOnStatusAndTellsTheUser(t *testing.T) {
	sp := runner.LocalStatusPatcher()
	pub := &capturingPublish{}
	rec := runner.CompletionBypassRecorder(sp, pub.fn, nil,
		channelevents.SessionRef{Namespace: "default", Name: "review-1"}, nil)

	require.NoError(t, rec(context.Background(), sampleBypass))

	// Half one: the durable record an operator reads back.
	got := sp.LocalCompletionBypasses()
	require.Len(t, got, 1, "the override must be recorded, not merely permitted")
	assert.Equal(t, sampleBypass.Reason, got[0].Reason)
	assert.Equal(t, []string{"artifact-delivered"}, got[0].Requirements,
		"the keys let an operator tell WHICH obligation was skipped without reading prose")
	assert.Equal(t, []string{sampleBypass.Unmet[0].Missing}, got[0].Details)
	assert.False(t, got[0].Time.IsZero(), "the writer stamps the observation")

	// Half two: the person the round was for finds out.
	require.Len(t, pub.payloads, 1, "a bypass nobody sees is an off switch")
	pl := decodeInteraction(t, pub.payloads[0])
	assert.Equal(t, categories.CompletionRequirementBypassed, pl.Category)
	assert.NotEmpty(t, pl.Lead)
	assert.NotEmpty(t, pl.NextStep, "a degraded-tone notice that names no action leaves the reader stuck")
	assert.Contains(t, pl.Body, sampleBypass.Unmet[0].Title,
		"the reader is told WHAT was skipped, in the requirement's own human phrasing")

	// The agent's reason is UNTRUSTED text, so it travels in the excerpt every
	// surface renders inert — never interpolated into Lead/Body/NextStep, which
	// surfaces draw as live markup.
	require.NotNil(t, pl.Excerpt, "the agent's reason must ride the inert field")
	assert.Equal(t, sampleBypass.Reason, pl.Excerpt.Content)
	for _, trusted := range []string{pl.Lead, pl.Body, pl.NextStep} {
		assert.NotContains(t, trusted, sampleBypass.Reason,
			"agent-authored text must not reach a field surfaces render as markup")
	}
	// Nor may the internal object names the model needed leak onto a chat surface.
	assert.NotContains(t, pl.Body, "ar-review-1-abc")
}

func TestCompletionBypassRecorder_FailurePropagatesSoTheGateCanRefuse(t *testing.T) {
	cases := []struct {
		name string
		pub  *capturingPublish
		want string
	}{
		{
			name: "publish fails: recorder errors so the tool refuses the bypass",
			pub:  &capturingPublish{err: errors.New("nats unreachable")},
			want: "nats unreachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runner.CompletionBypassRecorder(runner.LocalStatusPatcher(), tc.pub.fn, nil,
				channelevents.SessionRef{Namespace: "default", Name: "review-1"}, nil)
			err := rec(context.Background(), sampleBypass)
			require.Error(t, err, "a half-recorded bypass must not read as recorded")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestCompletionBypassRecorder_NoChannelRecordsAndSaysSo(t *testing.T) {
	// A kubectl-driven session has no second surface. Status is then the only
	// record there is, which is correct — but the absence of a user-facing
	// notice must be attributable to a decision, not to silence.
	sp := runner.LocalStatusPatcher()
	var logged []string
	rec := runner.CompletionBypassRecorder(sp, nil, nil,
		channelevents.SessionRef{Namespace: "default", Name: "review-1"},
		func(msg string, _ ...any) { logged = append(logged, msg) })

	require.NoError(t, rec(context.Background(), sampleBypass))
	assert.Len(t, sp.LocalCompletionBypasses(), 1, "status still records it")
	require.Len(t, logged, 1, "and the missing surface is logged, never silent")
	assert.Contains(t, strings.ToLower(logged[0]), "no channel")
}

func TestCompletionBypassRecorder_NoStatusPatcherRefuses(t *testing.T) {
	rec := runner.CompletionBypassRecorder(nil, (&capturingPublish{}).fn, nil,
		channelevents.SessionRef{Namespace: "default", Name: "review-1"}, nil)
	require.Error(t, rec(context.Background(), sampleBypass),
		"with nowhere durable to write it, the bypass has not been recorded and must not be granted")
}
