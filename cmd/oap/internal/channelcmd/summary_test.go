package channelcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelwizard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// TestRunNotes_RendersWhatTheRunRecordedThenWhatTheResultDeclared pins the
// order the two sources are merged in. They exist because a kind runs no code
// of its own during the run: what THIS PACKAGE recorded (a question's own
// note, what runHandoff learned) is in State, and what the KIND decided
// arrives on WizardOutput.Summary — and the operator reads them as one block.
func TestRunNotes_RendersWhatTheRunRecordedThenWhatTheResultDeclared(t *testing.T) {
	st := tui.NewState()
	st.Note("From the run", "recorded as the questions were answered")

	got := channelwizard.RunNotes(st, channelkinds.WizardOutput{
		Summary: []channelkinds.SummaryNote{{Label: "From the result", Value: "declared as data"}},
	})
	assert.Equal(t, []tui.Note{
		{Label: "From the run", Value: "recorded as the questions were answered"},
		{Label: "From the result", Value: "declared as data"},
	}, got)
}
