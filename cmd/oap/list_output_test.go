package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// runAPCommand runs one of oap's commands through the assembled root tree over
// a fake cluster and returns everything it wrote. Going through NewRootCmd is
// the point: it covers the wiring as well as the command.
func runAPCommand(t *testing.T, objs []client.Object, args ...string) string {
	t.Helper()
	b := aptest.NewBundle(t, objs...)
	return aptest.Run(t, NewRootCmdWithGlobals(aptest.GlobalsFor(b)), args...)
}

func agentClass(name string, bundles int, model *spiceboxv1alpha1.ModelConfig) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{Model: model},
	}
	for i := 0; i < bundles; i++ {
		ac.Spec.ToolBundles = append(ac.Spec.ToolBundles, spiceboxv1alpha1.ToolBundle{Name: "bundle", Class: "demo-class"})
	}
	return ac
}

// TestListCommandsRenderPlainColumnsOnANonTerminalStream covers every list
// command in this batch at once: the properties asserted (no escape codes, a
// header, a fixture cell, no box-drawing) are the same for all of them, and the
// only thing that varies is the fixture and the argv.
func TestListCommandsRenderPlainColumnsOnANonTerminalStream(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		objs   []client.Object
		header string
		cell   string
	}{
		{
			name:   "oap agent list: NAME header, no escape codes, no box border",
			args:   []string{"agent", "list"},
			objs:   []client.Object{agentClass("demo-agent", 2, &spiceboxv1alpha1.ModelConfig{Provider: "acme", Name: "swiftmodel"})},
			header: "BUNDLES",
			cell:   "acme/swiftmodel",
		},
		{
			name: "oap agent sessions: CLASS header, no escape codes, no box border",
			args: []string{"agent", "sessions"},
			objs: []client.Object{&spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
				Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
			}},
			header: "TURNS",
			cell:   "demo-session",
		},
		{
			name: "oap class list: TOOLS header, no escape codes, no box border",
			args: []string{"class", "list"},
			objs: []client.Object{&spiceboxv1alpha1.SpiceboxClass{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-class"},
				Spec: spiceboxv1alpha1.SpiceboxClassSpec{
					Image: "registry.invalid/demo/sandbox:v1",
					Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "read_file"}, {Name: "write_file"}},
				},
			}},
			header: "TOOLS",
			cell:   "read_file, write_file",
		},
		{
			name: "oap identity list: CREDS header, no escape codes, no box border",
			args: []string{"identity", "list"},
			objs: []client.Object{&spiceboxv1alpha1.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-identity", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentIdentitySpec{
					Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "api-token", Type: "static"}},
				},
			}},
			header: "CREDS",
			cell:   "demo-identity",
		},
		{
			name: "oap sandbox list: SANDBOX header, no escape codes, no box border",
			args: []string{"sandbox", "list"},
			objs: []client.Object{&spiceboxv1alpha1.SpiceboxSession{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-sandbox", Namespace: "default"},
				Spec:       spiceboxv1alpha1.SpiceboxSessionSpec{Class: "demo-class", Agent: "demo-agent"},
				Status: spiceboxv1alpha1.SpiceboxSessionStatus{
					Sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/demo-sandbox"},
				},
			}},
			header: "SANDBOX",
			cell:   "pod:default/demo-sandbox",
		},
		{
			name: "oap tools toolcall list: PHASE header, no escape codes, no box border",
			args: []string{"tools", "toolcall", "list"},
			objs: []client.Object{&spiceboxv1alpha1.ToolCall{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-toolcall", Namespace: "default"},
				Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "demo-sandbox", Tool: "read_file"},
			}},
			header: "PHASE",
			cell:   "read_file",
		},
		{
			name: "oap tools toolkit list: SUBCMDS header, no escape codes, no box border",
			args: []string{"tools", "toolkit", "list"},
			objs: []client.Object{&spiceboxv1alpha1.SpiceboxToolkit{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-toolkit", Namespace: "default"},
				Spec:       spiceboxv1alpha1.SpiceboxToolkitSpec{ToolkitRevision: "r7"},
			}},
			header: "SUBCMDS",
			cell:   "demo-toolkit",
		},
		{
			name: "oap user-identity list: CREDS header, no escape codes, no box border",
			args: []string{"user-identity", "list"},
			objs: []client.Object{&spiceboxv1alpha1.UserIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-user", Namespace: "default"},
				Spec: spiceboxv1alpha1.UserIdentitySpec{
					Subject:     "user:demo",
					Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "api-token", Type: "static"}},
				},
			}},
			header: "CREDS",
			cell:   "user:demo",
		},
		{
			name: "oap skill list: PINNED header, no escape codes, no box border",
			args: []string{"skill", "list"},
			objs: []client.Object{&spiceboxv1alpha1.Skill{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-skill", Namespace: "default"},
				Spec:       spiceboxv1alpha1.SkillSpec{CanonicalName: "demo/skill"},
			}},
			header: "PINNED",
			cell:   "demo/skill",
		},
		{
			name: "oap skill source list: PROBLEMS header, no escape codes, no box border",
			args: []string{"skill", "source", "list"},
			objs: []client.Object{&spiceboxv1alpha1.SkillSource{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "default"},
				Spec:       spiceboxv1alpha1.SkillSourceSpec{RepoURL: "https://git.invalid/org/repo", Ref: "main"},
			}},
			header: "PROBLEMS",
			cell:   "https://git.invalid/org/repo",
		},
		{
			name: "oap tools list: STATUS header, no escape codes, no box border",
			args: []string{"tools", "list"},
			objs: []client.Object{&spiceboxv1alpha1.MCPServer{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-server", Namespace: "default"},
				Spec: spiceboxv1alpha1.MCPServerSpec{
					Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.invalid/sse"},
				},
			}},
			header: "STATUS",
			cell:   "demo-server",
		},
		{
			name: "oap session operations: TOOLCALL header, no escape codes, no box border",
			args: []string{"session", "operations", "demo-session"},
			objs: []client.Object{
				&spiceboxv1alpha1.AgentSession{
					ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
					Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-agent"},
				},
				&spiceboxv1alpha1.ToolCall{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "demo-toolcall",
						Namespace: "default",
						Labels:    map[string]string{"agentsession": "demo-session", "ap.operation": "op-1"},
					},
					Spec: spiceboxv1alpha1.ToolCallSpec{Session: "demo-sandbox", Tool: "read_file"},
				},
			},
			header: "TOOLCALL",
			cell:   "Operation op-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runAPCommand(t, tc.objs, tc.args...)

			assert.False(t, aptest.HasANSI(out), "a non-terminal stream must receive no escape codes:\n%q", out)
			assert.Contains(t, out, tc.header, "header row")
			assert.Contains(t, out, tc.cell, "fixture row")
			// Whitespace-separated columns are what makes the output usable
			// from awk/cut; a box border would put "|" in field one of a
			// pipeline that has been reading these tables for a while.
			assert.NotContains(t, out, "│", "no box-drawing border")
		})
	}
}

// TestListCommandsAcceptNoColorAndStayPlain asserts what a non-terminal test
// can: --no-color is a flag these commands accept, and the run it produces is
// byte-clean.
//
// It deliberately does NOT claim to prove the flag is threaded into the theme.
// tui.Detect turns color on only for a TTY, and a bytes.Buffer never is one, so
// no assertion reachable from here can tell apcmd.DetectCaps(out, g.NoColor) from
// apcmd.DetectCaps(out, false) — both yield a colorless theme. What it does catch is a
// command that builds its theme from capabilities it invented rather than from
// the stream, which is the mistake that puts escape codes down a pipe.
func TestListCommandsAcceptNoColorAndStayPlain(t *testing.T) {
	out := runAPCommand(t,
		[]client.Object{agentClass("demo-agent", 1, nil)},
		"agent", "list", "--no-color")
	assert.False(t, aptest.HasANSI(out), "--no-color output must carry no escape codes")
	assert.Contains(t, out, "demo-agent")
}

// TestAgentListAlignsColumnsAcrossWideAndOverlongCells pins the display-column
// rule for a cell whose byte length, rune count and display width all differ.
//
// TestAgentListSizesTheModelColumnInDisplayColumns is the measure test: it
// renders the same list twice, changing only whether the widest MODEL cell is
// wide-rune text or ASCII of the *same display width*, and requires the two
// layouts to be identical.
//
// Stated as a comparison rather than a hardcoded column number because the
// arithmetic hides a trap: the table pads with `max - Width(cell) + gap`, so a
// column's start is `max + gap` for every row *whatever unit max was measured
// in*. Rows therefore stay aligned with each other even under a byte measure —
// an alignment-only assertion cannot see the bug at all. What a wrong measure
// actually does is size the column to the wrong number of columns, which is
// visible only against a reference of known display width.
//
// "供应商/模型" is 11 display columns, 16 bytes, 6 runes. "abcde/efghi" is 11
// of each. Under a byte measure MODEL is sized 16 and everything right of it
// shifts; under a rune measure it is sized 6, the pad goes negative, and the
// render panics.
func TestAgentListSizesTheModelColumnInDisplayColumns(t *testing.T) {
	const ageCell = "<unknown>"
	render := func(provider, name string) int {
		out := runAPCommand(t,
			[]client.Object{agentClass("demo-agent", 1, &spiceboxv1alpha1.ModelConfig{Provider: provider, Name: name})},
			"agent", "list")
		return aptest.DisplayColumnOf(t, aptest.LineContaining(t, out, "demo-agent"), ageCell)
	}

	assert.Equal(t, render("abcde", "efghi"), render("供应商", "模型"),
		"a MODEL cell 11 display columns wide must size the column the same whether it is wide-rune or ASCII")
}

// TestAgentListAlignsColumnsAcrossWideAndOverlongCells guards the rendered
// shape rather than the measure (which the test above owns): every row's AGE
// cell starts at one column, whatever the rows to either side of it contain.
// A dropped trailing pad, a row rendered to its own cell count, or a pad
// computed from the wrong row all break this.
//
// AGE is the cell measured because every row's is the same literal and no
// substring of it can occur inside an escape sequence; a one-character cell
// like BUNDLES would be found inside "\x1b[37m" if this output were ever
// colored, and the assertion would then be measuring the wrong thing.
func TestAgentListAlignsColumnsAcrossWideAndOverlongCells(t *testing.T) {
	objs := []client.Object{
		agentClass("wide-model-agent", 1, &spiceboxv1alpha1.ModelConfig{Provider: "供应商", Name: "模型"}),
		agentClass("ascii-model-agent", 1, &spiceboxv1alpha1.ModelConfig{Provider: "acme", Name: "swiftmodel"}),
		agentClass("long-model-agent", 1, &spiceboxv1alpha1.ModelConfig{Provider: "acme", Name: strings.Repeat("x", 60)}),
	}
	out := runAPCommand(t, objs, "agent", "list")

	// The fixtures carry no creation timestamp, so every AGE cell is this.
	const ageCell = "<unknown>"
	wide := aptest.DisplayColumnOf(t, aptest.LineContaining(t, out, "wide-model-agent"), ageCell)
	ascii := aptest.DisplayColumnOf(t, aptest.LineContaining(t, out, "ascii-model-agent"), ageCell)
	long := aptest.DisplayColumnOf(t, aptest.LineContaining(t, out, "long-model-agent"), ageCell)

	assert.Equal(t, ascii, wide, "AGE must start at the same display column for a wide-rune MODEL cell")
	assert.Equal(t, ascii, long, "AGE must start at the same display column for an over-long MODEL cell")
}
