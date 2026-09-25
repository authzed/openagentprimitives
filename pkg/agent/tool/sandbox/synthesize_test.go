package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

func TestSynthesizeProducesOneToolPerSpec(t *testing.T) {
	bundle := spiceboxv1alpha1.ToolBundle{
		Name:      "code",
		Toolspecs: []string{"git-readonly", "gh-readonly"},
	}
	classTools := []spiceboxv1alpha1.SpiceboxTool{
		{Name: "git", Command: []string{"/usr/bin/git"}},
		{Name: "gh", Command: []string{"/usr/bin/gh"}},
	}
	specs := []*spec.Spec{
		{Name: "git-readonly", Toolkit: spec.ToolkitRef{Name: "git", Revision: "2026-04-24"}, AllowSubcommands: []string{"log", "diff"}},
		{Name: "gh-readonly", Toolkit: spec.ToolkitRef{Name: "gh", Revision: "2026-04-27"}, AllowSubcommands: []string{"api"}},
	}

	tools, err := sandbox.Synthesize(bundle, specs, nil, nil, classTools)
	require.NoError(t, err, "Synthesize must succeed")
	require.Len(t, tools, 2, "tools count")
	names := map[string]bool{tools[0].Name(): true, tools[1].Name(): true}
	assert.True(t, names["code_git"], "expected tool name code_git in %v", names)
	assert.True(t, names["code_gh"], "expected tool name code_gh in %v", names)
}

func TestSynthesizeRejectsToolspecCollision(t *testing.T) {
	// Two toolspecs that target the same class tool would produce two
	// Tools with the same name — synthesizer must reject.
	bundle := spiceboxv1alpha1.ToolBundle{
		Name: "code", Toolspecs: []string{"git-readonly", "git-write"},
	}
	classTools := []spiceboxv1alpha1.SpiceboxTool{
		{Name: "git", Command: []string{"/usr/bin/git"}},
	}
	specs := []*spec.Spec{
		{Name: "git-readonly", Toolkit: spec.ToolkitRef{Name: "git", Revision: "X"}},
		{Name: "git-write", Toolkit: spec.ToolkitRef{Name: "git", Revision: "X"}},
	}
	_, err := sandbox.Synthesize(bundle, specs, nil, nil, classTools)
	require.Error(t, err, "expected collision error")
}

func TestSynthesizeMissingClassTool(t *testing.T) {
	bundle := spiceboxv1alpha1.ToolBundle{
		Name: "code", Toolspecs: []string{"gh-readonly"},
	}
	classTools := []spiceboxv1alpha1.SpiceboxTool{
		{Name: "git", Command: []string{"/usr/bin/git"}},
	}
	specs := []*spec.Spec{
		{Name: "gh-readonly", Toolkit: spec.ToolkitRef{Name: "gh", Revision: "X"}},
	}
	_, err := sandbox.Synthesize(bundle, specs, nil, nil, classTools)
	require.Error(t, err, "expected missing-class-tool error")
}

func TestSynthesizeUsesRenderDescribeWhenToolkitProvided(t *testing.T) {
	builtins := toolkits.All()
	var gitTk *toolkit.Toolkit
	for i := range builtins {
		if builtins[i].Name == "git" {
			gitTk = &builtins[i]
			break
		}
	}
	if gitTk == nil {
		t.Skip("git builtin toolkit not registered")
	}

	bundle := spiceboxv1alpha1.ToolBundle{
		Name:      "code",
		Toolspecs: []string{"git-readonly"},
	}
	classTools := []spiceboxv1alpha1.SpiceboxTool{
		{Name: "git", Command: []string{"/usr/bin/git"}},
	}
	specs := []*spec.Spec{
		{Name: "git-readonly", Intent: "read-only git", Toolkit: spec.ToolkitRef{Name: "git", Revision: "2026-04-24"}, AllowSubcommands: []string{"log"}},
	}
	tools, err := sandbox.Synthesize(bundle, specs, []*toolkit.Toolkit{gitTk}, nil, classTools)
	require.NoError(t, err, "Synthesize must succeed")
	require.NotEmpty(t, tools, "tools must be produced")
	desc := tools[0].Description()
	// The render.Describe text format mentions the toolkit binary or subcommand list;
	// any of these substrings indicate render.Describe was used (vs the fallback).
	assert.True(t, strings.Contains(desc, "log"),
		"description should reference the allowed subcommand 'log'; got %q", desc)
}

// The description surfaces the per-subcommand budget so the agent knows how
// long it has before a call is cut off. The git toolkit declares clone: 10m.
func TestSynthesizeDescriptionSurfacesSubcommandTimeout(t *testing.T) {
	builtins := toolkits.All()
	var gitTk *toolkit.Toolkit
	for i := range builtins {
		if builtins[i].Name == "git" {
			gitTk = &builtins[i]
			break
		}
	}
	if gitTk == nil {
		t.Skip("git builtin toolkit not registered")
	}
	bundle := spiceboxv1alpha1.ToolBundle{Name: "code", Toolspecs: []string{"git-rw"}}
	classTools := []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}}
	specs := []*spec.Spec{
		{Name: "git-rw", Intent: "git rw", Toolkit: spec.ToolkitRef{Name: "git", Revision: "2026-04-24"}, AllowSubcommands: []string{"clone", "log"}},
	}
	tools, err := sandbox.Synthesize(bundle, specs, []*toolkit.Toolkit{gitTk}, nil, classTools)
	require.NoError(t, err, "Synthesize must succeed")
	require.NotEmpty(t, tools, "tools must be produced")
	desc := tools[0].Description()
	assert.Contains(t, desc, "clone (≤10m)", "description should annotate clone's declared budget; got %q", desc)
	// log declares no timeout, so it stays bare (no budget suffix).
	assert.Contains(t, desc, "log", "description should still list undecorated subcommands; got %q", desc)
}

func TestTimeoutForSubcommand_DeclaredWinsOverModeDefault(t *testing.T) {
	cases := []struct {
		name string
		sc   *toolkit.Subcommand
		want time.Duration
	}{
		{
			name: "unset stream -> 30m mode default",
			sc:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeStream},
			want: 30 * time.Minute,
		},
		{
			name: "unset interactive -> 30m mode default",
			sc:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeInteractive},
			want: 30 * time.Minute,
		},
		{
			name: "unset sync -> 5m mode default",
			sc:   &toolkit.Subcommand{Mode: ""},
			want: 5 * time.Minute,
		},
		{
			name: "nil -> 5m sync default",
			sc:   nil,
			want: 5 * time.Minute,
		},
		{
			name: "declared timeout wins over sync default",
			sc:   &toolkit.Subcommand{Mode: "", Timeout: "10m"},
			want: 10 * time.Minute,
		},
		{
			name: "declared timeout wins over stream default",
			sc:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeStream, Timeout: "45m"},
			want: 45 * time.Minute,
		},
		{
			name: "declared short timeout shrinks sync default",
			sc:   &toolkit.Subcommand{Mode: "", Timeout: "30s"},
			want: 30 * time.Second,
		},
		{
			name: "unparseable timeout falls back to mode default",
			sc:   &toolkit.Subcommand{Mode: "", Timeout: "nope"},
			want: 5 * time.Minute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sandbox.TimeoutForSubcommand(tc.sc))
		})
	}
}

// TestResolveCallTimeout_PerInvokedSubcommand proves the ToolCall budget is
// resolved from the subcommand the argv actually selects — so clone and status
// on one multi-subcommand `git` tool get different budgets, including past a
// leading global flag (`git -C <dir> clone`).
func TestResolveCallTimeout_PerInvokedSubcommand(t *testing.T) {
	tk := &toolkit.Toolkit{
		Name:            "git",
		Version:         "1",
		ToolkitRevision: "2026-04-24",
		Target:          toolkit.Target{Binary: "git"},
		Parser:          toolkit.ParserConfig{Kind: "declarative"},
		GlobalFlags:     []toolkit.Flag{{Short: "C", Type: "path"}},
		Subcommands: []toolkit.Subcommand{
			{Path: []string{"clone"}, Timeout: "10m"},
			{Path: []string{"status"}, Timeout: "30s"},
			{Path: []string{"log"}}, // no declared timeout
		},
	}
	st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "code", Suffix: "git",
		Timeout: 5 * time.Minute, // synthesis-time sync default
		Toolkit: tk,
	})

	cases := []struct {
		name string
		args []string
		want time.Duration
	}{
		{name: "clone gets its declared 10m", args: []string{"clone", "https://example.com/r"}, want: 10 * time.Minute},
		{name: "status gets its declared 30s", args: []string{"status"}, want: 30 * time.Second},
		{name: "clone past a leading -C global still gets 10m", args: []string{"-C", "/repo", "clone", "https://example.com/r"}, want: 10 * time.Minute},
		{name: "log (undeclared) keeps the synthesis default", args: []string{"log"}, want: 5 * time.Minute},
		{name: "unmatched argv keeps the synthesis default", args: []string{"bogus"}, want: 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, st.ResolveCallTimeout(tc.args))
		})
	}
}

// A streaming tool is pinned to one subcommand; its synthesis-time budget
// (opts.Timeout) is kept regardless of argv.
func TestResolveCallTimeout_StreamingKeepsSynthesisBudget(t *testing.T) {
	tk := &toolkit.Toolkit{Subcommands: []toolkit.Subcommand{{Path: nil, Mode: toolkit.SubcommandModeStream}}}
	st := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "code", Suffix: "claude",
		Timeout:    30 * time.Minute,
		Toolkit:    tk,
		Subcommand: &tk.Subcommands[0],
	})
	assert.Equal(t, 30*time.Minute, st.ResolveCallTimeout([]string{"--print", "hello"}))
}

func TestPickStreamingSubcommand(t *testing.T) {
	cases := []struct {
		name  string
		specs []string // AllowSubcommands
		subs  []toolkit.Subcommand
		want  bool // a subcommand is returned
	}{
		{
			name:  "bare-binary stream subcommand matches empty allow key",
			specs: []string{""},
			subs:  []toolkit.Subcommand{{Path: nil, Mode: toolkit.SubcommandModeStream}},
			want:  true,
		},
		{
			name:  "bare-binary interactive subcommand still matches",
			specs: []string{""},
			subs:  []toolkit.Subcommand{{Path: nil, Mode: toolkit.SubcommandModeInteractive}},
			want:  true,
		},
		{
			name:  "single-segment stream subcommand matches",
			specs: []string{"run"},
			subs:  []toolkit.Subcommand{{Path: []string{"run"}, Mode: toolkit.SubcommandModeStream}},
			want:  true,
		},
		{
			name:  "plain (sync) subcommand is not picked",
			specs: []string{""},
			subs:  []toolkit.Subcommand{{Path: nil, Mode: ""}},
			want:  false,
		},
		{
			name:  "two allowed subcommands → nil",
			specs: []string{"a", "b"},
			subs:  []toolkit.Subcommand{{Path: []string{"a"}, Mode: toolkit.SubcommandModeStream}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := &spec.Spec{AllowSubcommands: tc.specs}
			tk := &toolkit.Toolkit{Subcommands: tc.subs}
			got := sandbox.PickStreamingSubcommand(ts, tk)
			if tc.want {
				require.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}
