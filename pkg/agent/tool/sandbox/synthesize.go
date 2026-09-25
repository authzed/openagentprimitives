package sandbox

import (
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// Synthesize produces one Tool per toolspec in the bundle. The Tool's name is
// "<bundle>_<class-tool-name>" where the class-tool-name comes from the
// SpiceboxClass.spec.tools catalog (matched by toolspec.toolkit.name).
//
// toolkits and credentials are parallel to specs, one entry per index. A nil
// toolkit entry falls back to the plain-text description; a nil credentials
// entry means no credential descriptors are stamped on that tool's ToolCalls.
//
// Returns an error if:
//   - Two toolspecs target the same class tool (would collide on Name).
//   - A toolspec's toolkit.name doesn't match any tool in classTools.
func Synthesize(
	bundle spiceboxv1alpha1.ToolBundle,
	specs []*spec.Spec,
	toolkits []*toolkit.Toolkit,
	credentials [][]spiceboxv1alpha1.CredentialDescriptor,
	classTools []spiceboxv1alpha1.SpiceboxTool,
) ([]tool.Tool, error) {
	classByName := map[string]spiceboxv1alpha1.SpiceboxTool{}
	for _, t := range classTools {
		classByName[t.Name] = t
	}

	entries := make([]synthesize.Entry, 0, len(specs))
	used := map[string]string{} // class-tool-name → toolspec name (for collision diagnostics)

	for i, ts := range specs {
		var tk *toolkit.Toolkit
		if i < len(toolkits) {
			tk = toolkits[i]
		}
		var creds []spiceboxv1alpha1.CredentialDescriptor
		if i < len(credentials) {
			creds = credentials[i]
		}
		classTool, ok := classByName[ts.Toolkit.Name]
		if !ok {
			return nil, fmt.Errorf("synthesize: toolspec %q targets toolkit %q which is not in the SpiceboxClass tool catalog",
				ts.Name, ts.Toolkit.Name)
		}
		if prior, exists := used[classTool.Name]; exists {
			return nil, fmt.Errorf("synthesize: bundle %q has two toolspecs targeting class tool %q (%q and %q)",
				bundle.Name, classTool.Name, prior, ts.Name)
		}
		used[classTool.Name] = ts.Name

		// Capture for closure.
		ts := ts
		ct := classTool
		tkRef := tk
		credsRef := creds
		// A toolspec narrowed to exactly one allowed subcommand that the toolkit
		// declares stream/interactive gets that Subcommand on SandboxOpts, so
		// Execute routes through the bridge path — which is how a toolBundle
		// surfaces a long-running streaming subcommand without a separate
		// per-subcommand class-tool entry. Sync narrowings leave Subcommand nil
		// and fall back to toolkit-level resolved permissions.
		subRef := pickStreamingSubcommand(ts, tkRef)
		entries = append(entries, synthesize.Entry{
			Name: ct.Name,
			Factory: func(_ string) tool.Tool {
				return NewSandboxTool(SandboxOpts{
					BundleName:   bundle.Name,
					Suffix:       ct.Name,
					Description:  describeSpec(ts, tkRef),
					ToolspecName: ts.Name,
					Timeout:      timeoutForSubcommand(subRef),
					Toolkit:      tkRef,
					Spec:         ts,
					Subcommand:   subRef,
					Credentials:  credsRef,
				})
			},
		})
	}
	return synthesize.Build(synthesize.Slice{P: bundle.Name, E: entries})
}

// timeoutForSubcommand picks the ToolCall spec.timeout. A subcommand's own
// sc.Timeout wins; otherwise the mode default applies. Streaming subcommands get
// a 30-minute backstop because ModeStream's deadline IS spec.timeout and they
// run long agentic processes. Sync subcommands get 5 minutes: a `git clone` of a
// non-trivial repo routinely exceeds 60s (a too-short budget once left a stale
// .git/index.lock from an interrupted clone), while fast commands finish in well
// under a second. Toolkit authors tune outliers per-subcommand.
func timeoutForSubcommand(sc *toolkit.Subcommand) time.Duration {
	if d, ok := subcommandTimeout(sc); ok {
		return d
	}
	if sc != nil && (sc.Mode == toolkit.SubcommandModeStream || sc.Mode == toolkit.SubcommandModeInteractive) {
		return 30 * time.Minute
	}
	return 5 * time.Minute
}

// subcommandTimeout returns the budget a subcommand explicitly declares and
// true when it declares one. Returns (0, false) for a nil subcommand or an
// unset Timeout, so callers apply the mode default themselves. Bad durations
// are rejected at toolkit load (toolkit.validate); a stray invalid value here
// is treated as undeclared rather than silently clamped to zero.
func subcommandTimeout(sc *toolkit.Subcommand) (time.Duration, bool) {
	if sc == nil || sc.Timeout == "" {
		return 0, false
	}
	d, err := time.ParseDuration(sc.Timeout)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// pickStreamingSubcommand returns the toolkit's matching Subcommand when the
// toolspec narrows it to exactly one allowed subcommand AND that subcommand's
// Mode is stream or interactive; nil for every other shape, so sync stays the
// default. A match is a single-segment subcommand path (`run`) or the bare
// binary (path: [], allow-list key ""). Nested paths (`gh pr list`) stay
// conservative and never flip to streaming off a one-element AllowSubcommands.
func pickStreamingSubcommand(ts *spec.Spec, tk *toolkit.Toolkit) *toolkit.Subcommand {
	if ts == nil || tk == nil {
		return nil
	}
	if len(ts.AllowSubcommands) != 1 {
		return nil
	}
	want := ts.AllowSubcommands[0]
	for i := range tk.Subcommands {
		sc := &tk.Subcommands[i]
		if sc.Mode != toolkit.SubcommandModeStream && sc.Mode != toolkit.SubcommandModeInteractive {
			continue
		}
		// Match either a single-segment subcommand path ("run") or the
		// bare binary (path: [], whose allow-list key is the empty
		// string "" — e.g. the claude toolkit). Nested paths like
		// "pr list" still won't match a one-element allowSubcommands.
		switch len(sc.Path) {
		case 0:
			if want == "" {
				return sc
			}
		case 1:
			if sc.Path[0] == want {
				return sc
			}
		}
	}
	return nil
}
