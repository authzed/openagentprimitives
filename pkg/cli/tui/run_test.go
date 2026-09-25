package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeScreen is a Screen whose behavior is fully specified by its fields, so
// each test states only what it cares about.
type fakeScreen struct {
	id       string
	label    string
	group    *huh.Group
	prepErr  error
	applyErr error
	prepared *int
	applied  *int
	onApply  func(*State)
}

func (f *fakeScreen) ID() string { return f.id }

// Label falls back to the ID so tests that don't care about the rail need
// not spell one out.
func (f *fakeScreen) Label() string {
	if f.label == "" {
		return f.id
	}
	return f.label
}

func (f *fakeScreen) Prepare(_ context.Context, _ *State) (*huh.Group, error) {
	if f.prepared != nil {
		*f.prepared++
	}
	return f.group, f.prepErr
}

func (f *fakeScreen) Apply(_ context.Context, st *State) error {
	if f.applied != nil {
		*f.applied++
	}
	if f.onApply != nil {
		f.onApply(st)
	}
	return f.applyErr
}

// countingDriver records which screen IDs were presented.
type countingDriver struct{ presented []string }

func (d *countingDriver) Present(_ context.Context, id string, _ *huh.Group) error {
	d.presented = append(d.presented, id)
	return nil
}

func testOptions(d Driver) Options {
	return Options{Driver: d, Theme: NewTheme(Caps{}), Title: "test"}
}

func TestRunWithoutAThemeFailsAndStillReturnsAUsableState(t *testing.T) {
	st, err := Run(context.Background(), nil, Options{Driver: &countingDriver{}})

	require.Error(t, err, "the theme every driver and renderer needs is required")
	assert.ErrorContains(t, err, "Theme")
	require.NotNil(t, st, "a caller inspecting state after a validation error must not panic")
	assert.Empty(t, st.Notes())
}

func TestRunPresentsAndAppliesInOrder(t *testing.T) {
	d := &countingDriver{}
	st, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "one", group: huh.NewGroup(huh.NewNote().Title("one")),
			onApply: func(s *State) { s.Set("one", "1") }},
		&fakeScreen{id: "two", group: huh.NewGroup(huh.NewNote().Title("two")),
			onApply: func(s *State) { s.Set("two", "2") }},
	}, testOptions(d))

	require.NoError(t, err, "Run must succeed when every screen succeeds")
	assert.Equal(t, []string{"one", "two"}, d.presented)
	assert.Equal(t, "1", st.Get("one"))
	assert.Equal(t, "2", st.Get("two"))
}

func TestRunNilGroupSkipsPresentationButStillApplies(t *testing.T) {
	d := &countingDriver{}
	applied := 0
	st, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "work-only", group: nil, applied: &applied,
			onApply: func(s *State) { s.Set("derived", "yes") }},
	}, testOptions(d))

	require.NoError(t, err)
	assert.Empty(t, d.presented, "a nil group must not reach the driver")
	assert.Equal(t, 1, applied, "a work-only screen must still Apply")
	assert.Equal(t, "yes", st.Get("derived"))
}

func TestRunErrSkipSkipsEntirely(t *testing.T) {
	d := &countingDriver{}
	applied := 0
	_, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "not-taken", prepErr: ErrSkip, applied: &applied},
		&fakeScreen{id: "taken", group: huh.NewGroup(huh.NewNote().Title("t"))},
	}, testOptions(d))

	require.NoError(t, err, "ErrSkip is a branch decision, not a failure")
	assert.Equal(t, []string{"taken"}, d.presented)
	assert.Equal(t, 0, applied, "a skipped screen must NOT Apply")
}

func TestRunPrepareErrorAbortsAndNamesTheScreen(t *testing.T) {
	d := &countingDriver{}
	neverPrepared := 0
	boom := errors.New("cluster unreachable")
	_, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "list-agents", prepErr: boom},
		&fakeScreen{id: "never-reached", group: huh.NewGroup(huh.NewNote()), prepared: &neverPrepared},
	}, testOptions(d))

	require.Error(t, err)
	assert.ErrorIs(t, err, boom, "the cause must be preserved for errors.Is")
	assert.Contains(t, err.Error(), "list-agents", "the failing screen must be named")
	assert.Empty(t, d.presented, "the loop must stop before the next screen is presented")
	assert.Equal(t, 0, neverPrepared, "the loop must stop before the next screen's Prepare runs")
}

func TestRunApplyErrorAbortsAndNamesTheScreen(t *testing.T) {
	d := &countingDriver{}
	neverPrepared := 0
	boom := errors.New("bad token")
	_, err := Run(context.Background(), []Screen{
		&fakeScreen{id: "validate", group: huh.NewGroup(huh.NewNote()), applyErr: boom},
		&fakeScreen{id: "never-reached", group: huh.NewGroup(huh.NewNote()), prepared: &neverPrepared},
	}, testOptions(d))

	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "validate")
	assert.Equal(t, []string{"validate"}, d.presented, "the loop must stop before the next screen is presented")
	assert.Equal(t, 0, neverPrepared, "the loop must stop before the next screen's Prepare runs")
}

func TestRunContextCanceledBeforeFirstScreenAbortsWithoutPreparing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prepared := 0
	_, err := Run(ctx, []Screen{
		&fakeScreen{id: "unreachable", group: huh.NewGroup(huh.NewNote()), prepared: &prepared},
	}, testOptions(&countingDriver{}))

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "the cause must be preserved for errors.Is")
	assert.Equal(t, 0, prepared, "a canceled context must stop the loop before Prepare runs")
}

func TestRunWithSeededStateLetsScreensSkipAsking(t *testing.T) {
	// The non-interactive shape: State is pre-seeded, so Prepare returns nil.
	d := &countingDriver{}
	seeded := NewState()
	seeded.Set("agent", "demo-agent")

	preSeeded := &preSeededScreen{id: "agent", key: "agent"}
	st, err := RunWith(context.Background(), []Screen{preSeeded}, testOptions(d), seeded)

	require.NoError(t, err)
	assert.Empty(t, d.presented, "an already-answered screen asks nothing")
	assert.Equal(t, "demo-agent", st.Get("agent"))
}

// preSeededScreen is the canonical shape every real screen follows: consult
// State first, ask only when unanswered.
type preSeededScreen struct {
	id  string
	key string
	val string
}

func (p *preSeededScreen) ID() string { return p.id }

func (p *preSeededScreen) Label() string { return "Agent" }

func (p *preSeededScreen) Prepare(_ context.Context, st *State) (*huh.Group, error) {
	if st.Has(p.key) {
		return nil, nil
	}
	return huh.NewGroup(huh.NewInput().Key(p.key).Title("Agent").Value(&p.val)), nil
}

func (p *preSeededScreen) Apply(_ context.Context, st *State) error {
	if !st.Has(p.key) {
		st.Set(p.key, p.val)
	}
	return nil
}

// TestRunWith_RecordsWhetherAHumanIsPresent: a screen whose behavior differs
// between a watched run and an unattended one has to be able to ask. Without
// it, the only signal is the fail-closed driver's refusal, which arrives too
// late and says nothing about why the screen wanted to know.
func TestRunWith_RecordsWhetherAHumanIsPresent(t *testing.T) {
	cases := []struct {
		name           string
		nonInteractive bool
		want           bool
	}{
		{name: "interactive run: NonInteractive() is false", nonInteractive: false, want: false},
		{name: "--non-interactive run: NonInteractive() is true", nonInteractive: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			probe := &fakeScreen{
				id:      "probe",
				label:   "Probe",
				onApply: func(st *State) { seen = st.NonInteractive() },
			}
			st, err := RunWith(context.Background(), []Screen{probe}, Options{
				Theme:          NewTheme(Caps{}),
				In:             strings.NewReader(""),
				Out:            io.Discard,
				NonInteractive: tc.nonInteractive,
			}, NewState())
			require.NoError(t, err, "a screen that asks nothing must run in both modes")

			assert.Equal(t, tc.want, seen, "the screen's view during the run")
			assert.Equal(t, tc.want, st.NonInteractive(), "the answered State's view afterwards")
		})
	}
}

// TestRunWith_ModeCannotBeSpoofedByASeededAnswer: the mode is a fact about the
// run, not an answer, so a caller pre-seeding the key must not change it.
func TestRunWith_ModeCannotBeSpoofedByASeededAnswer(t *testing.T) {
	seeded := NewState()
	seeded.SetBool(KeyNonInteractive, true)

	probe := &fakeScreen{id: "probe", label: "Probe"}
	st, err := RunWith(context.Background(), []Screen{probe}, Options{
		Theme: NewTheme(Caps{}),
		In:    strings.NewReader(""),
		Out:   io.Discard,
	}, seeded)
	require.NoError(t, err)

	assert.False(t, st.NonInteractive(), "the run's own mode overwrites a seeded value")
}

func TestPlainDriverReadsScriptedInputAndWritesNoANSI(t *testing.T) {
	var out bytes.Buffer
	in := strings.NewReader("demo-agent\n")
	th := NewTheme(Caps{TTY: false, Color: false, Width: 80})

	scr := &preSeededScreen{id: "agent", key: "agent"}
	st, err := Run(context.Background(), []Screen{scr},
		Options{Theme: th, Title: "test", In: in, Out: &out})

	require.NoError(t, err, "the plain driver must drive huh accessible mode")
	assert.Equal(t, "demo-agent", st.Get("agent"))
	assert.False(t, hasANSI(out.String()), "plain driver output must be byte-clean")
}
