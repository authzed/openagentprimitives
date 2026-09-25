package render

import (
	"fmt"
	"strings"

	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// report is the structured intermediate shape shared by all output formats
// (text, markdown, JSON). buildReport produces it from a (spec, toolName)
// pair; the per-format renderers in format_*.go take it as their only input.
type report struct {
	// Tool is the upstream tool's name.
	Tool string `json:"tool"`
	// Intent is the spec's stated reason for permitting the tool.
	Intent string `json:"intent,omitempty"`
	// Description is the prose the model sees for the tool.
	Description string `json:"description,omitempty"`
	// AllowedFields is the permitted argument surface. Empty means every
	// argument is denied unless UnconstrainedArgs is set.
	AllowedFields []string `json:"allowedFields,omitempty"`
	// UnconstrainedArgs reports that the tool opted out of arg allowlisting.
	UnconstrainedArgs bool `json:"unconstrainedArgs,omitempty"`
	// SensitiveFields are the argument paths whose values get redacted.
	SensitiveFields []string `json:"sensitiveFields,omitempty"`
	// Constraints is the prose justification of each CEL gate, never the CEL.
	Constraints []constraintEntry `json:"constraints,omitempty"`
	// HardStops are the deny rules that refuse a call outright.
	HardStops []string `json:"hardStops,omitempty"`
	// WritesRels describes the SpiceDB relationships a successful call writes.
	WritesRels []string `json:"writesRelationships,omitempty"`
}

// constraintEntry carries only the human-readable justification — never
// the raw CEL.
type constraintEntry struct {
	Message string `json:"message"`
}

func buildReport(sp *mcpspec.Spec, toolName string) (*report, error) {
	var t *mcpspec.Tool
	for i := range sp.Tools {
		if sp.Tools[i].Name == toolName {
			t = &sp.Tools[i]
			break
		}
	}
	if t == nil {
		return nil, fmt.Errorf("MCP spec %q has no tool named %q", sp.Name, toolName)
	}

	r := &report{
		Tool:              t.Name,
		Intent:            t.Intent,
		Description:       t.DescriptionOverride,
		AllowedFields:     t.Args.AllowedFields,
		UnconstrainedArgs: t.Args.UnconstrainedArgs,
		SensitiveFields:   t.Args.SensitiveFields,
	}

	for _, c := range t.Args.Constraints {
		msg := strings.TrimSpace(c.Message)
		if msg == "" {
			// CEL with no Message: signal the gate exists without
			// leaking the expression. The toolspec/MCP lint nudges
			// authors to fill Message so this stays rare.
			msg = "an additional constraint applies (no description provided)"
		}
		r.Constraints = append(r.Constraints, constraintEntry{Message: msg})
	}

	if t.Deny.Effects.Destructive {
		r.HardStops = append(r.HardStops, "destructive operations blocked")
	}
	for _, w := range t.Deny.Effects.Reads {
		r.HardStops = append(r.HardStops, "reads of "+w+" blocked")
	}
	for _, w := range t.Deny.Effects.Writes {
		r.HardStops = append(r.HardStops, "writes to "+w+" blocked")
	}
	if t.Deny.Effects.Creds.Writes {
		r.HardStops = append(r.HardStops, "credential writes blocked")
	}

	for _, w := range t.WritesRelationships {
		r.WritesRels = append(r.WritesRels,
			fmt.Sprintf("%s#%s@%s", w.Tuple.Resource, w.Tuple.Relation, w.Tuple.Subject))
	}
	return r, nil
}
