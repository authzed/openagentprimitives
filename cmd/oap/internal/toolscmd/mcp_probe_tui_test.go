package toolscmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func TestProbeBadges(t *testing.T) {
	th := aptest.PlainTheme() // no color → plain text

	cases := []struct {
		name string
		tool probe.Tool
		want string
	}{
		{
			name: "no annotations: empty string",
			tool: probe.Tool{Name: "x"},
			want: "",
		},
		{
			name: "read-only + idempotent: both badges, leading space",
			tool: probe.Tool{Annotations: probe.Annotations{ReadOnlyHint: true, IdempotentHint: true}},
			want: " [read-only] [idempotent]",
		},
		{
			name: "destructive + open-world",
			tool: probe.Tool{Annotations: probe.Annotations{DestructiveHint: true, OpenWorldHint: true}},
			want: " [destructive] [open-world]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probeBadges(tc.tool, th))
		})
	}
}

func TestProbeListView(t *testing.T) {
	th := aptest.PlainTheme()
	tools := []probe.Tool{
		{Name: "alpha", Annotations: probe.Annotations{ReadOnlyHint: true}},
		{Name: "bravo"},
		{Name: "charlie"},
		{Name: "delta"},
	}

	t.Run("cursor row gets '> ' prefix, others '  '", func(t *testing.T) {
		out := probeListView(tools, 1, 0, 4, 40, th)
		lines := strings.Split(out, "\n")
		assert.Contains(t, lines[0], "  alpha", "non-cursor row indented with two spaces")
		assert.Contains(t, lines[1], "> bravo", "cursor row marked with '> '")
	})

	t.Run("scroll window: offset and rows bound the visible slice", func(t *testing.T) {
		out := probeListView(tools, 3, 2, 2, 40, th)
		assert.NotContains(t, out, "alpha", "row before offset is hidden")
		assert.NotContains(t, out, "bravo", "row before offset is hidden")
		assert.Contains(t, out, "charlie", "row at offset is visible")
		assert.Contains(t, out, "delta", "row within window is visible")
	})

	t.Run("empty tool list: placeholder line", func(t *testing.T) {
		out := probeListView(nil, 0, 0, 4, 40, th)
		assert.Contains(t, out, "(no tools advertised)")
	})

	t.Run("badge survives on a row that fits within width", func(t *testing.T) {
		// alpha has ReadOnlyHint — the badge must not be dropped by
		// the single-line row styling.
		out := probeListView(tools, 0, 0, 4, 40, th)
		lines := strings.Split(out, "\n")
		assert.Contains(t, lines[0], "[read-only]", "badge text is preserved on the rendered row")
	})

	t.Run("over-width row stays a single line (no wrap)", func(t *testing.T) {
		long := []probe.Tool{{Name: strings.Repeat("x", 60), Annotations: probe.Annotations{ReadOnlyHint: true}}}
		out := probeListView(long, 0, 0, 4, 30, th)
		assert.NotContains(t, out, "\n", "an over-width row must not wrap onto a second line")
	})
}

func TestProbeDetailContent(t *testing.T) {
	th := aptest.PlainTheme()
	tool := probe.Tool{
		Name:         "search_owners",
		Description:  "Lists owners.",
		InputSchema:  []byte(`{"type":"object","properties":{"ownerIds":{"type":"array"}}}`),
		OutputSchema: []byte(`{"type":"object","properties":{"resolved":{"type":"integer"}}}`),
		Annotations:  probe.Annotations{ReadOnlyHint: true, Title: "Search Owners"},
	}

	t.Run("collapsed: name + description + hint, no schemas", func(t *testing.T) {
		out := probeDetailContent(tool, false, 60, th)
		assert.Contains(t, out, "search_owners", "tool name")
		assert.Contains(t, out, "Lists owners.", "description")
		assert.Contains(t, out, "press enter", "expand hint")
		assert.NotContains(t, out, "input schema:", "schemas hidden when collapsed")
		assert.NotContains(t, out, "ownerIds", "schemas hidden when collapsed")
	})

	t.Run("expanded: annotations + input + output schema", func(t *testing.T) {
		out := probeDetailContent(tool, true, 60, th)
		assert.Contains(t, out, "annotations:", "annotations label")
		assert.Contains(t, out, "Search Owners", "annotation title value")
		assert.Contains(t, out, "input schema:", "input schema label")
		assert.Contains(t, out, "\"ownerIds\"", "input schema body")
		assert.Contains(t, out, "output schema:", "output schema label")
		assert.Contains(t, out, "\"resolved\"", "output schema body")
		assert.NotContains(t, out, "press enter", "no expand hint when already expanded")
	})

	t.Run("expanded with no schemas: (none) placeholders", func(t *testing.T) {
		bare := probe.Tool{Name: "bare", Description: "Bare tool."}
		out := probeDetailContent(bare, true, 60, th)
		assert.Contains(t, out, "input schema:\n(none)", "missing input schema → (none)")
		assert.Contains(t, out, "output schema:\n(none)", "missing output schema → (none)")
	})
}

// asProbeModel asserts that a tea.Model is a probeTUIModel and returns
// it, failing the test cleanly rather than panicking on a bad type.
func asProbeModel(t *testing.T, m tea.Model) probeTUIModel {
	t.Helper()
	pm, ok := m.(probeTUIModel)
	require.True(t, ok, "Update must return a probeTUIModel")
	return pm
}

func TestProbeTUIModelUpdate(t *testing.T) {
	tools := []probe.Tool{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	// A sized model: WindowSizeMsg initializes layout + viewport.
	base := newProbeTUIModel("title", tools, true)
	sized, _ := base.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := asProbeModel(t, sized)
	assert.True(t, m.ready, "WindowSizeMsg marks the model ready")

	t.Run("down increments cursor", func(t *testing.T) {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		assert.Equal(t, 1, asProbeModel(t, next).cursor)
	})

	t.Run("up at top clamps to 0", func(t *testing.T) {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		assert.Equal(t, 0, asProbeModel(t, next).cursor)
	})

	t.Run("down past end clamps to last tool", func(t *testing.T) {
		cur := m
		for i := 0; i < 10; i++ {
			n, _ := cur.Update(tea.KeyMsg{Type: tea.KeyDown})
			cur = asProbeModel(t, n)
		}
		assert.Equal(t, 2, cur.cursor, "cursor never exceeds len(tools)-1")
	})

	t.Run("enter opens detail focus and does not toggle back off", func(t *testing.T) {
		e1, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		em := asProbeModel(t, e1)
		assert.True(t, em.expanded, "enter opens the detail pane")
		// A second enter while in detail focus is delegated to the
		// viewport, not a collapse toggle — expanded stays true.
		e2, _ := em.Update(tea.KeyMsg{Type: tea.KeyEnter})
		assert.True(t, asProbeModel(t, e2).expanded, "enter in detail focus does not collapse")
	})

	t.Run("esc returns from detail focus to the list", func(t *testing.T) {
		e1, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		back, _ := asProbeModel(t, e1).Update(tea.KeyMsg{Type: tea.KeyEsc})
		assert.False(t, asProbeModel(t, back).expanded, "esc collapses back to the list")
	})

	t.Run("left arrow returns from detail focus to the list", func(t *testing.T) {
		e1, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		back, _ := asProbeModel(t, e1).Update(tea.KeyMsg{Type: tea.KeyLeft})
		assert.False(t, asProbeModel(t, back).expanded, "left arrow collapses back to the list")
	})

	t.Run("in detail focus, down scrolls the viewport and does not move the list cursor", func(t *testing.T) {
		e1, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		em := asProbeModel(t, e1)
		require.Equal(t, 0, em.cursor, "precondition: cursor at 0")
		scrolled, _ := em.Update(tea.KeyMsg{Type: tea.KeyDown})
		assert.Equal(t, 0, asProbeModel(t, scrolled).cursor, "down in detail focus must not move the list cursor")
		assert.True(t, asProbeModel(t, scrolled).expanded, "still in detail focus after scrolling")
	})

	t.Run("esc in list focus quits", func(t *testing.T) {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		assert.NotNil(t, cmd, "esc at the list level quits")
	})

	t.Run("j/k navigate like down/up", func(t *testing.T) {
		down, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		jm := asProbeModel(t, down)
		assert.Equal(t, 1, jm.cursor, "j moves cursor down")
		up, _ := jm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		assert.Equal(t, 0, asProbeModel(t, up).cursor, "k moves cursor up")
	})

	t.Run("q returns a quit command", func(t *testing.T) {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		assert.NotNil(t, cmd, "q produces a command (tea.Quit)")
	})
}

func TestProbeTUIModelView(t *testing.T) {
	tools := []probe.Tool{
		{Name: "fetch_data"},
		{Name: "write_record"},
		{Name: "delete_item"},
	}
	const title = "My MCP Server"

	// Before any WindowSizeMsg the model is not ready and View returns "loading…".
	unready := newProbeTUIModel(title, tools, true)
	assert.Equal(t, "loading…", unready.View(), "unsized model must return loading placeholder")

	// After a WindowSizeMsg the model is ready and View composes the full UI.
	sized, _ := unready.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m := asProbeModel(t, sized)

	out := m.View()
	assert.Contains(t, out, title, "rendered view must contain the title")
	assert.Contains(t, out, tools[0].Name, "rendered view must contain the first tool's name")
	assert.Contains(t, out, "enter open", "list-focus footer hints how to open a tool")
	assert.Greater(t, strings.Count(out, "\n"), 0, "rendered view must span multiple lines")

	// After entering detail focus the footer switches to scroll/back hints.
	detail := asProbeModel(t, mustUpdate(m, tea.KeyMsg{Type: tea.KeyEnter}))
	detailOut := detail.View()
	assert.Contains(t, detailOut, "esc/← back", "detail-focus footer hints how to back out")
	assert.NotContains(t, detailOut, "enter open", "detail-focus footer drops the list hint")
}

// mustUpdate runs one Update and returns the resulting model, discarding
// the command — a convenience for view tests that only care about state.
func mustUpdate(m probeTUIModel, msg tea.Msg) tea.Model {
	next, _ := m.Update(msg)
	return next
}
