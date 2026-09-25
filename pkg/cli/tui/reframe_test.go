package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// screensFor builds one presentable screen per id, so a test can drive a whole
// sub-run rather than a single Present.
func screensFor(ids ...string) []Screen {
	out := make([]Screen, 0, len(ids))
	for _, id := range ids {
		out = append(out, &fakeScreen{id: id, group: huh.NewGroup(huh.NewNote().Title(id))})
	}
	return out
}

// headerOf is the title bar of a rendered frame: everything up to the blank
// line Chrome.Render puts between the title and the columns.
func headerOf(frame string) string { return strings.SplitN(frame, "\n", 2)[0] }

// TestReframeDrawsOnePresentationAcrossSeveralRuns is the property the
// channels pass of `oap agent install` needs, asserted at the seam that
// provides it.
//
// The shape is the real one and it is why Reframe exists rather than a merge:
// ONE driver (a shared stdin buffer is not optional), and SEVERAL runs over it,
// because the caller does uninterruptible work — a browser round trip, a
// network resolve — between them. Before Reframe every one of those runs
// inherited the chrome the driver was built with, so a caller's second unit of
// work was framed as more of the first: the first title, and the first rail.
//
// The count is what makes this a claim about ONE presentation rather than
// about two that merely agree. A frame rebuilt per unit of work can carry the
// same title and still restart the rail; a rail that spans the pass can only
// come from a chrome that outlives each run in it.
func TestReframeDrawsOnePresentationAcrossSeveralRuns(t *testing.T) {
	th := NewTheme(Caps{Width: 80})

	// The driver as the caller built it, for its OWN questions: its own title,
	// and no rail at all. This is what the second caller used to inherit.
	var frames []string
	shared := newTestTTY(t, NewChrome("oap · agent install", nil, th), func(m *chromeModel) error {
		frames = append(frames, m.chrome.Render(m.idx, "BODY"))
		m.form.State = huh.StateCompleted
		return nil
	})

	units := []Step{
		{ID: "demo-reviewbot-gh", Label: "demo-reviewbot-gh"},
		{ID: "demo-reviewbot-slack", Label: "demo-reviewbot-slack"},
	}
	pass := NewChrome("oap · agent install · channels", units, th)

	// One run per unit of work, exactly as the caller drives them.
	for _, u := range units {
		_, err := Run(context.Background(), screensFor("org", "owner"),
			Options{Driver: Reframe(shared, pass, u.ID), Theme: th})
		require.NoError(t, err, "run for %s", u.ID)
	}

	require.Len(t, frames, 4, "two units of two screens each are presented")

	headers := map[string]int{}
	for _, f := range frames {
		headers[headerOf(f)]++
	}
	assert.Len(t, headers, 1,
		"one presentation spans the pass: every frame must carry the same title bar, got %v", headers)
	assert.Contains(t, headers, "oap · agent install · channels",
		"and it must be the pass's own title, not the one the shared driver was built with")

	// The rail spans the pass: every frame lists every unit, and the one being
	// answered is the one marked active.
	for i, f := range frames {
		for _, u := range units {
			assert.Contains(t, f, u.Label, "frame %d must list every unit of the pass", i)
		}
	}
	assert.Contains(t, frames[0], activeMark+" demo-reviewbot-gh")
	assert.Contains(t, frames[0], pendingMark+" demo-reviewbot-slack")
	assert.Contains(t, frames[3], doneMark+" demo-reviewbot-gh",
		"a unit the pass has finished must read as done while a later one is answered")
	assert.Contains(t, frames[3], activeMark+" demo-reviewbot-slack")
}

// TestReframePinsTheStepRatherThanResolvingTheScreensOwnID is the other half:
// the screens presented under a unit of work are a kind's own questions, whose
// IDs the pass's chrome has never heard of. Resolving those against the rail
// would find nothing and mark every step pending, which is a rail that says
// where the operator is by saying nothing.
func TestReframePinsTheStepRatherThanResolvingTheScreensOwnID(t *testing.T) {
	th := NewTheme(Caps{Width: 80})
	var idx []int
	shared := newTestTTY(t, NewChrome("t", nil, th), func(m *chromeModel) error {
		idx = append(idx, m.idx)
		m.form.State = huh.StateCompleted
		return nil
	})
	pass := NewChrome("t", []Step{{ID: "first", Label: "First"}, {ID: "second", Label: "Second"}}, th)

	d := Reframe(shared, pass, "second")
	// Screen IDs deliberately unrelated to the rail's: they are a kind's
	// question names, and the rail's steps are the caller's channels.
	require.NoError(t, d.Present(context.Background(), "org", huh.NewGroup(huh.NewNote().Title("o"))))
	require.NoError(t, d.Present(context.Background(), "installation-id", huh.NewGroup(huh.NewNote().Title("i"))))

	assert.Equal(t, []int{1, 1}, idx,
		"every screen of a unit is drawn under that unit's step, whatever the screen is called")
}

// TestReframeLeavesTheOriginalDriverAlone: the caller keeps presenting its own
// questions over the driver it built, and a sub-run must not retitle them.
func TestReframeLeavesTheOriginalDriverAlone(t *testing.T) {
	th := NewTheme(Caps{Width: 80})
	var frames []string
	shared := newTestTTY(t, NewChrome("oap · agent install", nil, th), func(m *chromeModel) error {
		frames = append(frames, m.chrome.Render(m.idx, "BODY"))
		m.form.State = huh.StateCompleted
		return nil
	})
	pass := NewChrome("oap · agent install · channels", steps("One"), th)

	reframed := Reframe(shared, pass, "one")
	require.NoError(t, reframed.Present(context.Background(), "x", huh.NewGroup(huh.NewNote().Title("x"))))
	require.NoError(t, shared.Present(context.Background(), "y", huh.NewGroup(huh.NewNote().Title("y"))))

	require.Len(t, frames, 2)
	assert.Equal(t, "oap · agent install · channels", headerOf(frames[0]))
	assert.Equal(t, "oap · agent install", headerOf(frames[1]),
		"reframing is a new driver over the same terminal, not a mutation of the one it came from")
}

// TestReframeIsANoOpForADriverThatDrawsNoChrome pins the deliberate
// pass-through, and it is not merely tidiness: the fail-closed driver names
// the SCREEN nobody answered, and handing it a caller's unit of work to name
// instead would replace `--answer <key>` advice with a channel name that
// answers nothing.
func TestReframeIsANoOpForADriverThatDrawsNoChrome(t *testing.T) {
	th := NewTheme(Caps{})
	pass := NewChrome("t", steps("One"), th)

	plain := Plain(strings.NewReader(""), nil, th)
	assert.Same(t, plain, Reframe(plain, pass, "one"), "the line-oriented driver renders no frame")

	failClosed := NonInteractive()
	assert.Equal(t, failClosed, Reframe(failClosed, pass, "one"), "neither does the fail-closed one")

	// And a caller with no frame to give gets its driver back untouched,
	// rather than a driver whose chrome is nil and whose Render would panic.
	tty := newTestTTY(t, NewChrome("t", nil, th), func(m *chromeModel) error { return nil })
	assert.Same(t, Driver(tty), Reframe(tty, nil, "one"))
	assert.Nil(t, Reframe(nil, pass, "one"), "and a nil driver stays nil rather than being wrapped")
}

// TestRailLabelCutsToTheRailsOwnBudget: the labels now come from two unrelated
// vocabularies — a bundle author's prompt and a declared Channel's name — and
// one unbounded label costs EVERY step its rail rather than overflowing its
// own row.
func TestRailLabelCutsToTheRailsOwnBudget(t *testing.T) {
	long := "demo-reviewbot-github-pull-requests"
	require.Greater(t, len(long), MaxRailLabelColumns, "precondition: the fixture must exceed the budget")

	cut := RailLabel(long)
	assert.Equal(t, MaxRailLabelColumns, len([]rune(cut)), "a long label is cut to the budget")
	assert.True(t, strings.HasPrefix(long, cut), "and cut from the end, so the distinguishing head survives")

	short := "demo-gh"
	assert.Equal(t, short, RailLabel(short), "a label that already fits is untouched, byte for byte")

	// The budget's whole reason: at the cap, a rail still fits beside a usable
	// body column on an ordinary terminal.
	th := NewTheme(Caps{Width: 80})
	c := NewChrome("t", []Step{{ID: "a", Label: RailLabel(long)}}, th)
	assert.True(t, c.showsRail(80), "a capped label must not take the rail away from every step")
}
