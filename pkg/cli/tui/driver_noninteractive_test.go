package tui

import (
	"context"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNonInteractiveRefusesAnUnansweredScreen(t *testing.T) {
	scr := &preSeededScreen{id: "agent", key: "agent"}
	_, err := Run(context.Background(), []Screen{scr},
		Options{Theme: NewTheme(Caps{}), Title: "test", NonInteractive: true})

	require.Error(t, err, "an unanswered screen must fail closed, never default")
	assert.ErrorIs(t, err, ErrUnanswered)
	assert.Contains(t, err.Error(), "agent", "the error must name the unanswered screen")
}

func TestNonInteractiveAcceptsAFullySeededRun(t *testing.T) {
	seeded := NewState()
	seeded.Set("agent", "demo-agent")

	st, err := RunWith(context.Background(), []Screen{&preSeededScreen{id: "agent", key: "agent"}},
		Options{Theme: NewTheme(Caps{}), Title: "test", NonInteractive: true}, seeded)

	require.NoError(t, err, "a fully seeded run needs no prompting")
	assert.Equal(t, "demo-agent", st.Get("agent"))
}

func TestNonInteractiveNeverPresents(t *testing.T) {
	// Sanity: the driver has no IO at all, so it cannot block on a missing TTY.
	err := NonInteractive().Present(context.Background(), "x", huh.NewGroup(huh.NewNote()))
	assert.ErrorIs(t, err, ErrUnanswered)
}
