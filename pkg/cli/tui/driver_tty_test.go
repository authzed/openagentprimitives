package tui

import (
	"context"
	"errors"
	"io"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestTTY builds the terminal driver with the bubbletea program replaced
// by run, so Present's sequencing is exercised without a pseudo-terminal.
func newTestTTY(t *testing.T, ch *Chrome, run func(m *chromeModel) error) *ttyDriver {
	t.Helper()
	d := TTY(TTYOpts{
		Out:    io.Discard,
		Theme:  NewTheme(Caps{TTY: true, Color: false, Width: 80}),
		Chrome: ch,
	}).(*ttyDriver)
	d.runProgram = func(_ context.Context, m *chromeModel) error { return run(m) }
	return d
}

func TestChromeModelCtrlCAbortsTheFormAndQuits(t *testing.T) {
	m := &chromeModel{
		form:   huh.NewForm(huh.NewGroup(huh.NewInput().Key("k").Title("K"))),
		chrome: NewChrome("t", steps("K"), NewTheme(Caps{Width: 80})),
	}
	require.Equal(t, huh.StateNormal, m.form.State, "precondition: a fresh form is unanswered")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	// An aborted form must stay distinguishable from a completed one after
	// the program exits: huh signals the difference ONLY through this field,
	// so anything reading the quit alone cannot tell them apart.
	assert.Equal(t, huh.StateAborted, m.form.State, "ctrl+c aborts rather than completing the form")
	require.NotNil(t, cmd, "an aborted form must quit the program")
	assert.IsType(t, tea.QuitMsg{}, cmd(), "the quit is what releases the alt-screen")
}

func TestTTYPresentReportsCancellationAsAnError(t *testing.T) {
	boom := errors.New("terminal exploded")

	cases := []struct {
		name    string
		run     func(m *chromeModel) error
		wantErr error
	}{
		{
			name: "user pressed ctrl+c: huh.ErrUserAborted, never a silent success",
			run: func(m *chromeModel) error {
				m.form.State = huh.StateAborted
				return nil
			},
			wantErr: huh.ErrUserAborted,
		},
		{
			name: "program interrupted: the same cancellation by the other route",
			run: func(m *chromeModel) error {
				return tea.ErrInterrupted
			},
			wantErr: huh.ErrUserAborted,
		},
		{
			name: "program failed: the cause is preserved",
			run: func(m *chromeModel) error {
				return boom
			},
			wantErr: boom,
		},
		{
			name: "user answered: no error",
			run: func(m *chromeModel) error {
				m.form.State = huh.StateCompleted
				return nil
			},
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestTTY(t, NewChrome("t", steps("Agent"), NewTheme(Caps{Width: 80})), tc.run)

			err := d.Present(context.Background(), "agent",
				huh.NewGroup(huh.NewInput().Key("agent").Title("Agent")))

			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), "agent", "the failing screen must be named")
		})
	}
}

func TestRunStopsAtTheScreenTheUserCanceled(t *testing.T) {
	// The whole point of C1: a canceled screen must not be applied, and the
	// screens after it must never be reached.
	applied, laterPrepared := 0, 0
	d := newTestTTY(t, nil, func(m *chromeModel) error {
		m.form.State = huh.StateAborted
		return nil
	})

	st, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "agent", group: huh.NewGroup(huh.NewNote().Title("a")), applied: &applied,
			onApply: func(s *State) { s.Set("agent", "half-answered") }},
		&fakeScreen{id: "review", group: huh.NewGroup(huh.NewNote().Title("r")), prepared: &laterPrepared},
	}, Options{Driver: d, Theme: NewTheme(Caps{})})

	require.Error(t, err, "a canceled screen must abort the run")
	assert.ErrorIs(t, err, huh.ErrUserAborted, "the cause must survive for errors.Is")
	assert.Equal(t, 0, applied, "a canceled screen must NOT Apply")
	assert.Equal(t, 0, laterPrepared, "no screen after the canceled one may be prepared")
	assert.Empty(t, st.Get("agent"), "no partial answer may be recorded")
}

func TestTTYPresentHighlightsThePresentedScreensOwnRailStep(t *testing.T) {
	// The real branching shape: one screen skips its branch (ErrSkip) and one
	// does work without asking (nil group), so the count of screens PRESENTED
	// diverges from their positions in the rail. Resolving the rail position
	// from the screen's own ID is what keeps the highlight on the step the
	// user is actually answering.
	screens := []Screen{
		&fakeScreen{id: "agent", label: "Agent", group: huh.NewGroup(huh.NewNote().Title("a"))},
		&fakeScreen{id: "slack-app", label: "Slack app", group: huh.NewGroup(huh.NewNote().Title("s"))},
		&fakeScreen{id: "manifest", label: "Manifest", prepErr: ErrSkip},
		&fakeScreen{id: "tokens", label: "Tokens", group: huh.NewGroup(huh.NewNote().Title("t"))},
		&fakeScreen{id: "validate", label: "Validate", group: nil},
		&fakeScreen{id: "review", label: "Review", group: huh.NewGroup(huh.NewNote().Title("r"))},
	}

	th := NewTheme(Caps{Width: 80})
	var rendered []string
	d := newTestTTY(t, NewChrome("t", Steps(screens), th), func(m *chromeModel) error {
		rendered = append(rendered, m.chrome.Render(m.idx, "BODY"))
		m.form.State = huh.StateCompleted
		return nil
	})

	_, err := Run(context.Background(), screens, Options{Driver: d, Theme: th})
	require.NoError(t, err)

	want := []string{"Agent", "Slack app", "Tokens", "Review"}
	require.Len(t, rendered, len(want), "only the screens with a group are presented")
	for i, label := range want {
		assert.Contains(t, rendered[i], activeMark+" "+label,
			"presentation %d must highlight the rail step of the screen being presented", i)
	}
}

// TestChromeModelRendersNothingOnceTheFormHasQuit pins the frame that outlives
// the program. bubbletea writes the model's LAST view as the final frame, and
// an inline run leaves that frame in the terminal's ordinary buffer instead of
// discarding it with the alt screen. huh renders an empty view once its form
// has quit, so a chrome drawn unconditionally leaves a bare title bar and a
// blank line behind for EVERY question a command asks over one inline driver —
// three questions, three stacked title bars in the scrollback.
func TestChromeModelRendersNothingOnceTheFormHasQuit(t *testing.T) {
	cases := []struct {
		name      string
		state     huh.FormState
		wantFrame bool
	}{
		{name: "unanswered: the chrome still frames the form", state: huh.StateNormal, wantFrame: true},
		{name: "completed: nothing is left in the scrollback", state: huh.StateCompleted},
		{name: "aborted: nothing is left in the scrollback", state: huh.StateAborted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &chromeModel{
				form:   huh.NewForm(huh.NewGroup(huh.NewInput().Key("k").Title("K"))),
				chrome: NewChrome("oap · repro", steps("K"), NewTheme(Caps{Width: 80})),
			}
			m.form.State = tc.state

			got := m.View()

			if tc.wantFrame {
				assert.Contains(t, got, "oap · repro", "a form still being answered must be framed")
				return
			}
			assert.Empty(t, got, "a form that has quit must render nothing at all")
		})
	}
}
