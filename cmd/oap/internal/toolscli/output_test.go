package toolscli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

// pipedTheme is what every renderer here is handed when `oap tools …` output is
// piped or --no-color is passed: Color false, so Theme.Render returns each
// string verbatim and substrings can be asserted without stripping escapes.
// It replaces a package-level lipgloss.SetColorProfile(0) — with the theme
// carrying the decision, no test needs to mutate a global to be readable.
func pipedTheme() *tui.Theme { return tui.NewTheme(tui.Caps{Width: 80}) }

func TestRenderRows_Empty(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, RenderRows(&b, pipedTheme(), nil), "RenderRows(nil) must succeed")
	assert.Contains(t, b.String(), "no tools", "empty rows should render 'no tools' message")
}

// Three render-and-assert-contains tests share the same shape: invoke a
// renderer, then check the output contains a set of substrings.
func TestRenders_OutputContainsExpectedSubstrings(t *testing.T) {
	cases := []struct {
		name     string
		render   func(io.Writer) error
		contains []string
	}{
		{
			name: "RenderRows with both kinds: header + names + summary present",
			render: func(w io.Writer) error {
				return RenderRows(w, pipedTheme(), []contract.Row{
					{Kind: "MCPServer", Name: "linear", Namespace: "ns", Status: "Valid=True", Summary: "https://x · 2 tool(s)"},
					{Kind: "SpiceboxToolspec", Name: "git", Namespace: "", Status: "Valid=True", Summary: "toolkit=git@1"},
				})
			},
			contains: []string{"KIND", "MCPServer", "SpiceboxToolspec", "linear", "git", "https://x"},
		},
		{
			name: "RenderDetail: header + namespace + section titles and bodies",
			render: func(w io.Writer) error {
				return RenderDetail(w, pipedTheme(), contract.Detail{
					Kind: "MCPServer", Name: "linear", Namespace: "ns",
					Sections: []contract.Section{
						{Title: "Server", Body: "https://x (streamable-http)"},
						{Title: "Tools", Body: "- a\n- b"},
					},
				})
			},
			contains: []string{"MCPServer/linear", "namespace ns", "Server", "https://x", "Tools", "- a", "- b"},
		},
		{
			name: "RenderProbeResult: title + section title + body",
			render: func(w io.Writer) error {
				return RenderProbeResult(w, pipedTheme(), contract.ProbeResult{
					Title: "MCP probe",
					Sections: []contract.Section{
						{Title: "https://x — 1 tool(s)", Body: "- search"},
					},
				})
			},
			contains: []string{"MCP probe", "https://x", "- search"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			require.NoError(t, tc.render(&b), "render must succeed")
			out := b.String()
			for _, want := range tc.contains {
				assert.Contains(t, out, want, "missing substring %q\n---\n%s\n---", want, out)
			}
		})
	}
}

func TestRenderDiagnostics(t *testing.T) {
	cases := []struct {
		name     string
		diags    []contract.Diagnostic
		wantAny  bool
		contains []string
	}{
		{
			name:     "no diagnostics: anyErr=false, prints OK",
			diags:    nil,
			wantAny:  false,
			contains: []string{"OK"},
		},
		{
			name: "error + warning: anyErr=true, both levels and messages rendered",
			diags: []contract.Diagnostic{
				{Severity: "warning", Path: "x", Message: "small"},
				{Severity: "error", Path: "y", Message: "big"},
			},
			wantAny:  true,
			contains: []string{"ERROR", "WARN", "big", "small"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			got := RenderDiagnostics(&b, pipedTheme(), tc.diags)
			assert.Equal(t, tc.wantAny, got, "anyErr return value")
			out := b.String()
			for _, want := range tc.contains {
				assert.Contains(t, out, want, "missing %q in output", want)
			}
		})
	}
}

// renderers is every `oap tools` output surface, each reduced to "write yourself
// to w using th". The two tests below are the same claim from both sides, so
// they share the list rather than each naming four renderers.
var renderers = []struct {
	name   string
	render func(io.Writer, *tui.Theme)
}{
	{
		name: "RenderRows",
		render: func(w io.Writer, th *tui.Theme) {
			_ = RenderRows(w, th, []contract.Row{
				{Kind: "MCPServer", Name: "demo-server", Namespace: "ns", Status: "Valid=True", Summary: "2 tool(s)"},
			})
		},
	},
	{
		name: "RenderDetail",
		render: func(w io.Writer, th *tui.Theme) {
			_ = RenderDetail(w, th, contract.Detail{
				Kind: "MCPServer", Name: "demo-server",
				Sections: []contract.Section{{Title: "Server", Body: "streamable-http"}},
			})
		},
	},
	{
		name: "RenderProbeResult",
		render: func(w io.Writer, th *tui.Theme) {
			_ = RenderProbeResult(w, th, contract.ProbeResult{
				Title:    "demo probe",
				Sections: []contract.Section{{Title: "Tools", Body: "- search"}},
			})
		},
	},
	{
		name: "RenderDiagnostics",
		render: func(w io.Writer, th *tui.Theme) {
			RenderDiagnostics(w, th, []contract.Diagnostic{{Severity: "error", Path: "x", Message: "bad"}})
		},
	},
}

// TestRenderersStayByteCleanUnderAColorlessTheme is the piped-output contract:
// `oap tools list | grep`, `oap tools validate` in CI, and --no-color all resolve
// to a colorless theme, and none of them may see an escape code. RenderRows is
// additionally held to whitespace-separated columns, because a box border would
// put a box-drawing character in field one of any pipeline reading the table.
func TestRenderersStayByteCleanUnderAColorlessTheme(t *testing.T) {
	for _, r := range renderers {
		t.Run(r.name+": no escape codes, no box-drawing", func(t *testing.T) {
			var b bytes.Buffer
			r.render(&b, pipedTheme())
			out := b.String()
			assert.NotContains(t, out, "\x1b[", "a colorless theme must emit no escape codes:\n%q", out)
			assert.NotContains(t, out, "│", "no box-drawing border")
		})
	}
}

// TestRenderersColorizeUnderAColorEnabledTheme is the other half: it proves the
// theme parameter is load-bearing rather than accepted and ignored.
//
// The cmd/oap-side tests cannot make this claim — tui.Detect turns color on only
// for a TTY and a bytes.Buffer never is one, so from there a renderer that
// dropped the theme entirely would look identical. Here the capabilities are
// stated outright, so a renderer that stopped consulting th fails.
func TestRenderersColorizeUnderAColorEnabledTheme(t *testing.T) {
	for _, r := range renderers {
		t.Run(r.name+": emits the theme's escape codes", func(t *testing.T) {
			var b bytes.Buffer
			r.render(&b, tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80}))
			assert.True(t, strings.Contains(b.String(), "\x1b["),
				"a color-enabled theme must reach the output:\n%q", b.String())
		})
	}
}
