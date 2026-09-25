package validator

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/validator/core"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// Invocation is the concrete thing being validated.
type Invocation struct {
	ToolName string
	Args     map[string]any
}

// Check runs the validator pipeline and returns a Decision.
//
// Phases (each helper lives in phases.go):
//
//	tool → allowedFields → deny.effects → deny.trust → constraints → allow.
//
// Each phase short-circuits the pipeline by populating Decision.FailedOn;
// finalize scrubs values registered up-front out of every user-visible string.
//
// The error return is reserved for internal failures (currently:
// malformed CEL syntax inside the spec itself — i.e. authoring bugs,
// not policy denies).
func Check(sp *mcpspec.Spec, inv Invocation) (*Decision, error) {
	d := &Decision{
		Redactions: map[string]redact.Descriptor{},
		Parsed: &ParsedArgs{
			ToolName: inv.ToolName,
			// Deep copy so the Decision's view is fully independent of
			// inv.Args: sensitive-arg token replacement and the parsed-args
			// scrub both mutate nested structures in place, and inv.Args
			// must stay unredacted for the CEL constraint phase.
			Args: redact.DeepCopyJSONMap(core.CopyAnyMap(inv.Args)),
		},
	}
	r := redact.New()

	tool, ok := findTool(sp, inv.ToolName)
	if !ok {
		msg := fmt.Sprintf("tool %q is not in the spec's allowlist", inv.ToolName)
		addTrace(d, "tool", StatusFail, msg, "")
		failedOn(d, "tool", msg)
		return finalize(d, r), nil
	}
	// Register + token-replace sensitive values on the Parsed copy, not
	// the raw Invocation.Args — CEL constraints below still evaluate
	// against the unredacted inv.Args, while everything the Decision
	// emits (Parsed.Args, Reason, Trace) is scrubbed.
	registerSensitiveArgs(d, r, tool, d.Parsed.Args)
	addTrace(d, "tool", StatusPass, "", "")

	if ok := checkAllowedFields(d, tool, inv.Args); !ok {
		return finalize(d, r), nil
	}

	if ok := checkDenyEffects(d, tool); !ok {
		return finalize(d, r), nil
	}

	if ok := checkDenyTrust(d, tool); !ok {
		return finalize(d, r), nil
	}

	if ok, err := checkConstraints(d, tool, inv.Args); err != nil {
		return nil, err
	} else if !ok {
		return finalize(d, r), nil
	}

	d.Allow = true
	d.Reason = "allowed"
	return finalize(d, r), nil
}
