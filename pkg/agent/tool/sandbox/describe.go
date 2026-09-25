package sandbox

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// describeSpec renders a compact, LLM-facing summary of the toolspec. The
// shape is deliberately tight (one line) so the agent's tool list stays
// readable when many tools/specs are loaded; verbose ToolSpec details are
// available via `oap tools toolspec show` for human auditing.
//
// Always includes the explicit list of allowed subcommands so the agent
// doesn't try unsupported subcommands or flags and waste turns on
// "denied by parse" rejections (the parser is strict; unknown flags fail
// even on otherwise-allowed subcommands).
func describeSpec(ts *spec.Spec, tk *toolkit.Toolkit) string {
	intent := strings.TrimSpace(ts.Intent)
	if intent == "" {
		intent = "(no intent declared)"
	}
	subs := "none"
	if len(ts.AllowSubcommands) > 0 {
		// Annotate any subcommand that declares a non-default per-call budget
		// (e.g. "clone (≤10m)") so the agent knows the time it has before the
		// call is cut off and can plan around the slow ones.
		labels := make([]string, len(ts.AllowSubcommands))
		for i, name := range ts.AllowSubcommands {
			labels[i] = name
			if tk != nil {
				if sc := subcommandByAllowName(tk, name); sc != nil && sc.Timeout != "" {
					labels[i] = name + " (≤" + sc.Timeout + ")"
				}
			}
		}
		subs = strings.Join(labels, ", ")
	}
	parts := []string{
		fmt.Sprintf("%s (toolspec=%s).", intent, ts.Name),
		fmt.Sprintf("Allowed subcommands: %s.", subs),
	}

	// Hard stops the agent should know about.
	var stops []string
	if ts.Deny.Effects.Destructive {
		stops = append(stops, "destructive ops blocked")
	}
	if len(ts.Deny.Effects.Reads) > 0 {
		stops = append(stops, "reads from "+strings.Join(ts.Deny.Effects.Reads, "/")+" blocked")
	}
	if len(ts.Deny.Effects.Writes) > 0 {
		stops = append(stops, "writes to "+strings.Join(ts.Deny.Effects.Writes, "/")+" blocked")
	}
	if ts.Deny.Effects.Creds.Writes {
		stops = append(stops, "credential mutation blocked")
	}
	if len(stops) > 0 {
		parts = append(parts, "Hard stops: "+strings.Join(stops, "; ")+".")
	}

	// Sensitive flags warning. Pulled from the toolkit when available, since
	// the toolspec only stores names; the toolkit knows the human-readable
	// description.
	if tk != nil && len(ts.Sensitive.Flags) > 0 {
		parts = append(parts, "Sensitive flags refused: "+strings.Join(ts.Sensitive.Flags, ", ")+".")
	}

	parts = append(parts, "Argv-only; flags must be in the allowed set for the chosen subcommand. Never embed credentials in args — they are injected automatically.")
	return strings.Join(parts, " ")
}

// subcommandByAllowName resolves an AllowSubcommands entry (a space-joined
// subcommand path, e.g. "repo clone") to the toolkit's matching Subcommand, or
// nil when none matches.
func subcommandByAllowName(tk *toolkit.Toolkit, name string) *toolkit.Subcommand {
	for i := range tk.Subcommands {
		if strings.Join(tk.Subcommands[i].Path, " ") == name {
			return &tk.Subcommands[i]
		}
	}
	return nil
}
