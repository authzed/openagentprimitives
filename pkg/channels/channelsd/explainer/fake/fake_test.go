package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer"
)

// TestFake_ReturnsScriptedResponse pins the canned-happy-path
// contract: tests set Response, drive Explain, and the same Output
// comes back.
func TestFake_ReturnsScriptedResponse(t *testing.T) {
	want := explainer.Output{
		What: []string{"GitHub", "Linear OAuth"},
		Why:  []string{"To file the issue and update the ticket."},
	}
	e := &Explainer{Response: want}

	got, err := e.Explain(context.Background(), explainer.Input{
		InitiatingMessage: "x",
		Credentials:       []explainer.CredentialInfo{{Name: "c", Provider: "P"}},
	})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestFake_ReturnsScriptedError pins the error-path contract: when ResponseErr
// is non-nil it shadows Response and surfaces verbatim, which is how the
// credential-request tests exercise the static-explanation fallback.
func TestFake_ReturnsScriptedError(t *testing.T) {
	wantErr := errors.New("scripted explainer failure")
	e := &Explainer{
		Response:    explainer.Output{What: []string{"should-be-ignored"}},
		ResponseErr: wantErr,
	}
	got, err := e.Explain(context.Background(), explainer.Input{})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, explainer.Output{}, got,
		"Response is ignored when ResponseErr is set")
}

// TestFake_RecordsCalls pins the Calls observability contract: every Input
// passed to Explain is appended in order, which is how the prompt-injection
// boundary tests assert no agent output leaked into the explainer.
func TestFake_RecordsCalls(t *testing.T) {
	e := &Explainer{}
	in1 := explainer.Input{InitiatingMessage: "first"}
	in2 := explainer.Input{InitiatingMessage: "second"}

	_, err := e.Explain(context.Background(), in1)
	require.NoError(t, err)
	_, err = e.Explain(context.Background(), in2)
	require.NoError(t, err)

	calls := e.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "first", calls[0].InitiatingMessage)
	assert.Equal(t, "second", calls[1].InitiatingMessage)
}

// TestFake_CallsReturnsCopy verifies the defensive-copy contract: a
// test that mutates the returned slice must NOT affect future Calls()
// returns.
func TestFake_CallsReturnsCopy(t *testing.T) {
	e := &Explainer{}
	_, _ = e.Explain(context.Background(), explainer.Input{InitiatingMessage: "first"})

	calls := e.Calls()
	require.Len(t, calls, 1)
	calls[0].InitiatingMessage = "MUTATED"

	again := e.Calls()
	require.Len(t, again, 1)
	assert.Equal(t, "first", again[0].InitiatingMessage,
		"Calls() must return a defensive copy")
}
