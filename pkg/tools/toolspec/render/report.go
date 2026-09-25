package render

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// report is the structured intermediate representation used by all three
// output formats (text, markdown, JSON). buildReport produces it from the
// (spec, toolkit) pair; the per-format renderers in format_*.go take it
// as their only input.
type report struct {
	// Spec is the spec's name.
	Spec string `json:"spec"`
	// Intent is the spec's own one-line statement of purpose.
	Intent string `json:"intent,omitempty"`
	// Source is who authored the spec: "user" for hand-written, otherwise the
	// generator's Generation.Source tag.
	Source string `json:"source"`
	// GeneratedAt is the authoring timestamp; empty for a hand-written spec.
	GeneratedAt string `json:"generatedAt,omitempty"`
	// Allows is one entry per permitted subcommand.
	Allows []allowEntry `json:"allows,omitempty"`
	// Constraints is the prose justification of each CEL gate, never the CEL.
	Constraints []constraintEntry `json:"constraints,omitempty"`
	// HardStops are the deny rules that refuse a call outright.
	HardStops []ruleEntry `json:"hardStops,omitempty"`
	// Bounds are the allow rules that cap what a permitted call may reach.
	Bounds []ruleEntry `json:"bounds,omitempty"`
	// Network is the rendered summary of the network bound.
	Network string `json:"network"`
	// Creds is the rendered summary of the credential bound.
	Creds string `json:"creds"`
	// Warnings are the author's own caveats about the spec.
	Warnings []string `json:"warnings,omitempty"`
	// Unmatched are parts of the intent the spec does not cover.
	Unmatched []spec.Unmatched `json:"unmatched,omitempty"`
	// Excluded are subcommands deliberately left out.
	Excluded []spec.Excluded `json:"excluded,omitempty"`
	// Mismatches are test cases whose last run disagreed with the author's
	// expectation — the spec did not converge.
	Mismatches []mismatchEntry `json:"mismatches,omitempty"`
}

type allowEntry struct {
	// Subcommand is the permitted path, space-joined.
	Subcommand string `json:"subcommand"`
	// Description is the prose shown for it, from the spec or a fallback.
	Description string `json:"description"`
}

type constraintEntry struct {
	Message string `json:"message"`
}

type ruleEntry struct {
	// Path locates the rule inside the spec, e.g. "deny.effects.destructive".
	Path string `json:"path"`
	// Description is the plain-language rendering of what the rule does.
	Description string `json:"description"`
}

type mismatchEntry struct {
	// Intent is the test case's human sentence.
	Intent string `json:"intent"`
	// ExpectAllow is what the author intended.
	ExpectAllow bool `json:"expectAllow"`
	// Actual is what the validator decided on the last run.
	Actual bool `json:"actual"`
	// Reason is the validator's short explanation of the disagreement.
	Reason string `json:"reason,omitempty"`
}

func buildReport(sp *spec.Spec, tk *toolkit.Toolkit) *report {
	r := &report{Spec: sp.Name, Intent: sp.Intent, Source: "user"}
	if sp.Generation != nil {
		r.Source = sp.Generation.Source
		r.GeneratedAt = sp.Generation.GeneratedAt
		r.Warnings = append(r.Warnings, sp.Generation.Warnings...)
		r.Unmatched = append(r.Unmatched, sp.Generation.Unmatched...)
		r.Excluded = append(r.Excluded, sp.Generation.Excluded...)
		for _, tc := range sp.Generation.TestCases {
			if tc.LastRunActual != nil && *tc.LastRunActual != tc.ExpectAllow {
				r.Mismatches = append(r.Mismatches, mismatchEntry{
					Intent:      tc.Intent,
					ExpectAllow: tc.ExpectAllow,
					Actual:      *tc.LastRunActual,
					Reason:      tc.LastRunReason,
				})
			}
		}
	}

	// allowSubcommands
	for i, sub := range sp.AllowSubcommands {
		desc := descOrFallback(sp, spec.PathKey("allowSubcommands", i), subcommandDescription(tk, sub))
		r.Allows = append(r.Allows, allowEntry{Subcommand: fmt.Sprintf("%s %s", tk.Target.Binary, sub), Description: desc})
	}

	// constraints
	for i, c := range sp.Constraints {
		msg := descOrFallback(sp, spec.PathKey("constraints", i), c.Message)
		r.Constraints = append(r.Constraints, constraintEntry{Message: msg})
	}

	// deny.effects
	if sp.Deny.Effects.Destructive {
		r.HardStops = append(r.HardStops, ruleEntry{
			Path:        "deny.effects.destructive",
			Description: descOrFallback(sp, "deny.effects.destructive", tmplDenyDestructive),
		})
	}
	if len(sp.Deny.Effects.Reads) > 0 {
		r.HardStops = append(r.HardStops, ruleEntry{
			Path:        "deny.effects.reads",
			Description: descOrFallback(sp, "deny.effects.reads", tmplDenyReads),
		})
	}
	if len(sp.Deny.Effects.Writes) > 0 {
		r.HardStops = append(r.HardStops, ruleEntry{
			Path:        "deny.effects.writes",
			Description: descOrFallback(sp, "deny.effects.writes", tmplDenyWrites),
		})
	}
	if sp.Deny.Effects.Creds.Writes {
		r.HardStops = append(r.HardStops, ruleEntry{
			Path:        "deny.effects.creds.writes",
			Description: descOrFallback(sp, "deny.effects.creds.writes", tmplDenyCredsWrites),
		})
	}

	// allow.*
	if sp.Allow.Network.Set {
		r.Bounds = append(r.Bounds, ruleEntry{
			Path:        "allow.network.destinations",
			Description: descOrFallback(sp, "allow.network.destinations", tmplAllowNetwork),
		})
	}
	if sp.Allow.Filesystem.Set {
		r.Bounds = append(r.Bounds, ruleEntry{
			Path:        "allow.filesystem.pathsUnder",
			Description: descOrFallback(sp, "allow.filesystem.pathsUnder", tmplAllowFilesystem),
		})
	}
	if sp.Allow.Creds.Set {
		r.Bounds = append(r.Bounds, ruleEntry{
			Path:        "allow.creds.required",
			Description: descOrFallback(sp, "allow.creds.required", tmplAllowCredsRequired),
		})
	}

	// Network rollup
	if sp.Allow.Network.Set && len(sp.Allow.Network.Destinations) > 0 {
		r.Network = "only " + strings.Join(sp.Allow.Network.Destinations, ", ")
	} else {
		r.Network = "(no bound declared)"
	}

	// Creds rollup
	r.Creds = credsSummary(tk, sp)

	return r
}

func descOrFallback(sp *spec.Spec, path, fallback string) string {
	if sp.Generation != nil {
		if d, ok := sp.Generation.Descriptions[path]; ok && d != "" {
			return d
		}
	}
	return fallback
}

// subcommandDescription finds the matching toolkit subcommand and returns
// its description, falling back to the joined path string if no match.
func subcommandDescription(tk *toolkit.Toolkit, joined string) string {
	for _, sc := range tk.Subcommands {
		if strings.Join(sc.Path, " ") == joined {
			if sc.Description != "" {
				return sc.Description
			}
			return joined
		}
	}
	return joined
}

func credsSummary(tk *toolkit.Toolkit, sp *spec.Spec) string {
	// Gather env var names that are "sensitive" across the toolkit.
	var sensitive []string
	for _, e := range tk.Env.Allowed {
		if e.Sensitive {
			sensitive = append(sensitive, e.Name)
		}
	}
	if sp.Deny.Effects.Creds.Writes && len(sensitive) > 0 {
		return "uses " + strings.Join(sensitive, ", ") + "; never writes/changes it"
	}
	if len(sensitive) > 0 {
		return "may read " + strings.Join(sensitive, ", ")
	}
	return "no required creds declared"
}
