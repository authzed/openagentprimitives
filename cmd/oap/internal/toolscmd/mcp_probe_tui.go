package toolscmd

// This file renders the interactive `oap tools mcp probe` TUI. Every
// value shown here — tool names, descriptions, annotations, input and
// output schemas — comes solely from the live MCP `tools/list`
// response carried in probe.Tool. No MCPServer CR fields are read or
// displayed; keep it that way.
//
// What it takes from pkg/cli/tui, and what it does not:
//
//   - Colors, the pane frame and the selected-row highlight all come from
//     tui.Theme, resolved once at construction.
//   - The title obeys tui.Truncate at the terminal's live width — Chrome's
//     rule that no rendered line may overflow. A probe title is a server's own
//     name, so it is exactly the kind of value that outruns a narrow terminal;
//     a wrapped title would shift both panes down a row past the heights
//     layout() computed.
//   - tui.Chrome itself is NOT used. Its width comes from Theme.Caps, fixed
//     when the run is built, with no way to be told a new one — but this model
//     owns the alternate screen and re-lays-out on every tea.WindowSizeMsg. Its
//     step rail has no analogue either: the left pane is a selection among
//     peers, not progress through a sequence.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// probeBadges renders the annotation hint badges for a tool, each
// color-coded, with a single leading space. Returns "" when the tool
// declares no hints.
func probeBadges(t probe.Tool, th *tui.Theme) string {
	var parts []string
	if t.Annotations.ReadOnlyHint {
		parts = append(parts, th.Render(th.Success, "[read-only]"))
	}
	if t.Annotations.DestructiveHint {
		parts = append(parts, th.Render(th.Err, "[destructive]"))
	}
	if t.Annotations.IdempotentHint {
		parts = append(parts, th.Render(th.Subtle, "[idempotent]"))
	}
	if t.Annotations.OpenWorldHint {
		parts = append(parts, th.Render(th.Subtle, "[open-world]"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// probeAnnotationsBlock renders the four MCP hint booleans (and the
// optional title) as an indented block for the expanded detail pane.
func probeAnnotationsBlock(a probe.Annotations) string {
	lines := []string{
		fmt.Sprintf("  read-only:   %v", a.ReadOnlyHint),
		fmt.Sprintf("  destructive: %v", a.DestructiveHint),
		fmt.Sprintf("  idempotent:  %v", a.IdempotentHint),
		fmt.Sprintf("  open-world:  %v", a.OpenWorldHint),
	}
	if a.Title != "" {
		lines = append(lines, "  title:       "+a.Title)
	}
	return strings.Join(lines, "\n")
}

// probeListView renders the left-pane tool rows. cursor is the
// selected index; offset is the index of the first visible row; rows
// is how many rows fit; width is the inner pane width. The cursor row
// is prefixed "> " (and reverse-styled when color is on); other rows
// are prefixed "  ".
func probeListView(tools []probe.Tool, cursor, offset, rows, width int, th *tui.Theme) string {
	if len(tools) == 0 {
		return th.Render(th.Subtle, "(no tools advertised)")
	}
	// Inline forces single-line rendering (no word-wrap); Width pads a
	// short row to the full pane width so the selected-row highlight
	// fills it; MaxWidth truncates a long row (ANSI-aware) instead of
	// wrapping the overflow onto a dropped second line.
	rowStyle := lipgloss.NewStyle().Inline(true).Width(width).MaxWidth(width)
	end := offset + rows
	if end > len(tools) {
		end = len(tools)
	}
	var b strings.Builder
	for i := offset; i < end; i++ {
		prefix := "  "
		if i == cursor {
			prefix = "> "
		}
		line := rowStyle.Render(prefix + tools[i].Name + probeBadges(tools[i], th))
		if i == cursor {
			line = th.Selected.Render(line)
		}
		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// probeDetailContent renders the right-pane content for one tool.
// Collapsed: name + description + a one-line expand hint. Expanded:
// name + description + annotations + input schema + output schema.
// width is the inner pane width; long text is wrapped to it.
func probeDetailContent(t probe.Tool, expanded bool, width int, th *tui.Theme) string {
	wrap := lipgloss.NewStyle().Width(width)
	var b strings.Builder
	b.WriteString(th.Render(th.Title, t.Name))
	b.WriteString("\n\n")
	desc := t.Description
	if desc == "" {
		desc = "(no description)"
	}
	b.WriteString(wrap.Render(desc))
	if !expanded {
		b.WriteString("\n\n")
		b.WriteString(th.Render(th.Subtle, "press enter to show annotations and schemas"))
		return b.String()
	}
	b.WriteString("\n\n")
	b.WriteString(th.Render(th.Title, "annotations:"))
	b.WriteString("\n")
	b.WriteString(probeAnnotationsBlock(t.Annotations))
	b.WriteString("\n\n")
	b.WriteString(th.Render(th.Title, "input schema:"))
	b.WriteString("\n")
	b.WriteString(indentedSchema(t.InputSchema))
	b.WriteString("\n\n")
	b.WriteString(th.Render(th.Title, "output schema:"))
	b.WriteString("\n")
	b.WriteString(indentedSchema(t.OutputSchema))
	return b.String()
}

// probeTUIModel is the bubbletea model for the interactive probe view:
// a left list pane and a right detail pane. The detail pane is
// collapsed (description only) until the user presses enter, then
// shows annotations + input + output schema via a scrollable
// viewport. The model is a thin shell — all rendering goes through the
// pure helpers above.
type probeTUIModel struct {
	title  string
	tools  []probe.Tool
	cursor int // selected tool index
	offset int // first visible row in the left list

	expanded bool           // detail pane collapsed/expanded toggle
	vp       viewport.Model // scrolls the detail pane content

	width  int
	height int
	ready  bool // set once the first WindowSizeMsg arrives

	// computed by layout() from width/height:
	listInnerW   int
	detailInnerW int
	paneInnerH   int

	// th is resolved once, at construction. Every keypress re-renders both
	// panes, so re-detecting capabilities per frame would put a terminal probe
	// and a full style build in the hot path.
	th *tui.Theme
}

// newProbeTUIModel builds the model for a real run. Capabilities are read off
// os.Stdout because that is the stream bubbletea draws on whatever writer the
// command was given, so --no-color, NO_COLOR and a non-terminal stdout each
// land on the colorless theme here exactly as they do elsewhere.
func newProbeTUIModel(title string, tools []probe.Tool, noColor bool) probeTUIModel {
	return newProbeTUIModelWithTheme(title, tools, tui.NewTheme(tui.Detect(os.Stdout, noColor)))
}

// newProbeTUIModelWithTheme builds the model against a caller-supplied theme.
// Layout/viewport are zero until the first tea.WindowSizeMsg arrives.
//
// It exists so the rendering can be exercised at capabilities this process does
// not have: a test binary's stdout is never a terminal, so newProbeTUIModel can
// only ever produce a colorless theme, and a render path that quietly stopped
// consulting the theme would look identical.
func newProbeTUIModelWithTheme(title string, tools []probe.Tool, th *tui.Theme) probeTUIModel {
	return probeTUIModel{
		title: title,
		tools: tools,
		th:    th,
	}
}

func (m probeTUIModel) Init() tea.Cmd { return nil }

// layout recomputes pane dimensions from the terminal size. Each pane
// is drawn with a rounded border (1 char per side), and the chrome
// outside the panes is: title line, blank line, footer line.
func (m *probeTUIModel) layout() {
	// Minimum usable width is ~48 cols: a terminal narrower than
	// (24+2)+(20+2) renders the right pane past the screen edge. We
	// accept that rather than degrade to a single-pane layout.
	listInnerW := m.width / 3
	if listInnerW < 24 {
		listInnerW = 24
	}
	if listInnerW > 48 {
		listInnerW = 48
	}
	// Total horizontal budget = (listInnerW+2) + (detailInnerW+2).
	detailInnerW := m.width - listInnerW - 4
	if detailInnerW < 20 {
		detailInnerW = 20
	}
	paneInnerH := m.height - 3 - 2 // title+blank+footer, then border top/bottom
	if paneInnerH < 3 {
		paneInnerH = 3
	}
	m.listInnerW = listInnerW
	m.detailInnerW = detailInnerW
	m.paneInnerH = paneInnerH

	m.vp.Width = detailInnerW
	m.vp.Height = paneInnerH
}

// syncOffset slides the left-list scroll window so the cursor stays
// visible.
func (m *probeTUIModel) syncOffset() {
	rows := m.paneInnerH
	if rows < 1 {
		rows = 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
}

// refreshDetail re-renders the detail pane content for the current
// selection + expanded state into the viewport.
func (m *probeTUIModel) refreshDetail() {
	if len(m.tools) == 0 {
		m.vp.SetContent(m.th.Render(m.th.Subtle, "(no tools advertised)"))
		return
	}
	m.vp.SetContent(probeDetailContent(m.tools[m.cursor], m.expanded, m.vp.Width, m.th))
}

func (m probeTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.vp.Width == 0 && m.vp.Height == 0 {
			m.vp = viewport.New(1, 1)
		}
		m.layout()
		m.ready = true
		m.syncOffset()
		m.refreshDetail()
		return m, nil

	case tea.KeyMsg:
		key := msg.String()
		// ctrl+c and q always quit, from either focus.
		if key == "ctrl+c" || key == "q" {
			return m, tea.Quit
		}
		if m.expanded {
			// Detail focus: esc / left back out to the list; every
			// other key (↑/↓, pgup/pgdn, …) scrolls the viewport.
			switch key {
			case "esc", "left", "h":
				m.expanded = false
				m.refreshDetail()
				return m, nil
			default:
				var cmd tea.Cmd
				m.vp, cmd = m.vp.Update(msg)
				return m, cmd
			}
		}
		// List focus.
		switch key {
		case "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.syncOffset()
				m.refreshDetail()
				m.vp.GotoTop()
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.tools)-1 {
				m.cursor++
				m.syncOffset()
				m.refreshDetail()
				m.vp.GotoTop()
			}
			return m, nil
		case "enter":
			// Enter focuses the detail pane (expanded view); arrow
			// keys then scroll it until esc/left backs out.
			m.expanded = true
			m.refreshDetail()
			m.vp.GotoTop()
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

func (m probeTUIModel) View() string {
	if !m.ready {
		return "loading…"
	}
	listBody := probeListView(m.tools, m.cursor, m.offset, m.paneInnerH, m.listInnerW, m.th)
	left := m.th.Frame.Width(m.listInnerW).Height(m.paneInnerH).Render(listBody)
	right := m.th.Frame.Width(m.detailInnerW).Height(m.paneInnerH).Render(m.vp.View())
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	// Chrome's rule, applied at the width this model actually has: a title
	// that overflowed would wrap, taking a row layout() already handed to the
	// panes and pushing the footer off-screen.
	title := tui.Truncate(m.th.Render(m.th.Title, m.title), m.width)
	hint := "↑/↓ move · enter open · q quit"
	if m.expanded {
		hint = "↑/↓ scroll · pgup/pgdn page · esc/← back · q quit"
	}
	footer := m.th.Render(m.th.Subtle, hint)
	return lipgloss.JoinVertical(lipgloss.Left, title, "", panes, footer)
}

// runProbeTUI runs the interactive probe view to completion. ctx
// cancellation (e.g. SIGINT upstream) tears the program down.
func runProbeTUI(ctx context.Context, title string, tools []probe.Tool, noColor bool) error {
	m := newProbeTUIModel(title, tools, noColor)
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen()).Run()
	return err
}
