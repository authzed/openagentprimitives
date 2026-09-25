package validator

// Per-phase rule evaluation called by Check. Each helper returns either:
//   • (true, nil)   — phase passed; Check continues to the next phase
//   • (false, nil)  — phase failed and recorded the failure on the Decision;
//                     Check should return finalize(d, r)
//   • (false, err)  — internal error (e.g. malformed CEL); Check returns it

import (
	"encoding/json"
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	toolscel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
)

// checkAllowedFields enforces tool.Args.AllowedFields, fail-closed.
//
// An argument key not present in AllowedFields is rejected. Crucially, an
// EMPTY/unset AllowedFields is NOT "allow everything" — it denies any argument
// the caller passes (an arg-less call has nothing to reject and passes), so a
// forgotten allowlist cannot silently let the agent pass any field. The only
// way to permit arbitrary keys is the explicit tool.Args.UnconstrainedArgs
// opt-out.
func checkAllowedFields(d *Decision, tool *mcpspec.Tool, args map[string]any) bool {
	if tool.Args.UnconstrainedArgs {
		addTrace(d, "allowedFields", StatusSkip, "tool opted out of arg constraints via unconstrainedArgs", "")
		return true
	}
	allowed := map[string]bool{}
	for _, f := range tool.Args.AllowedFields {
		allowed[f] = true
	}
	var extras []string
	for k := range args {
		if !allowed[k] {
			extras = append(extras, k)
		}
	}
	if len(extras) == 0 {
		addTrace(d, "allowedFields", StatusPass, "", "")
		return true
	}
	sort.Strings(extras)
	var msg string
	if mcpspec.RefusesEveryArgument(tool.Args.AllowedFields, tool.Args.UnconstrainedArgs) {
		msg = fmt.Sprintf("argument(s) %v denied: tool has no allowedFields configured (set unconstrainedArgs to accept free-form args)", extras)
	} else {
		msg = fmt.Sprintf("field(s) %v not in allowedFields %v", extras, tool.Args.AllowedFields)
	}
	addTrace(d, "allowedFields", StatusFail, msg, "")
	failedOn(d, "allowedFields", msg)
	return false
}

// checkDenyEffects enforces tool.Deny.Effects against the probed Effects
// snapshot. Only Destructive is enforceable: MCP tool annotations have no
// analog for Reads / Writes / Creds.Writes, so those fields, when set, are
// surfaced as warnings at spec compile time (pkg/tools/mcp/spec/compile.go).
func checkDenyEffects(d *Decision, tool *mcpspec.Tool) bool {
	if tool.Deny.Effects.Destructive && tool.Effects.Destructive {
		msg := "tool is marked destructive (probed annotation) and spec denies destructive effects"
		addTrace(d, "deny.effects.destructive", StatusFail, msg, "")
		failedOn(d, "deny.effects.destructive", msg)
		return false
	}
	addTrace(d, "deny.effects", StatusPass, "", "")
	return true
}

// checkDenyTrust enforces opt-in tool.Deny.Trust rules against the
// probed SEP-1913 Trust snapshot. Each axis denies only when BOTH
// the static Trust declaration matches AND the spec author authorized
// denial via DenyTrust. Mirrors checkDenyEffects.
//
// The helpers (ContainsEnumFieldChecked, ContainsEnum) hide the SEP's
// possibles-array semantics: a tools/list field may be a string or
// a JSON array of strings.
//
// Fail-closed contract: when a deny axis is active but the metadata
// blob it inspects does not parse as a JSON object (truncated, bare
// string/array, attacker-corrupt), we cannot conclude "no match", so
// we DENY rather than fall through to allow — and record a Warning so
// the corruption is visible in the trace, not silent.
func checkDenyTrust(d *Decision, tool *mcpspec.Tool) bool {
	if tool.Deny.Trust.OutcomesIrreversible &&
		denyTrustAxis(d, "deny.trust.outcomesIrreversible",
			tool.Trust.InputMetadata, "inputMetadata", "outcomes", "irreversible",
			"tool may produce irreversible outcomes (SEP-1913 inputMetadata.outcomes) and spec denies") {
		return false
	}
	if tool.Deny.Trust.DestinationPublic &&
		denyTrustAxis(d, "deny.trust.destinationPublic",
			tool.Trust.InputMetadata, "inputMetadata", "destination", "public",
			"tool may transmit args to public destinations (SEP-1913 inputMetadata.destination) and spec denies") {
		return false
	}
	if tool.Deny.Trust.SourceUntrustedPublic &&
		denyTrustAxis(d, "deny.trust.sourceUntrustedPublic",
			tool.Trust.ReturnMetadata, "returnMetadata", "source", "untrustedPublic",
			"tool may return untrusted public data (SEP-1913 returnMetadata.source) and spec denies") {
		return false
	}
	addTrace(d, "deny.trust", StatusPass, "", "")
	return true
}

// denyTrustAxis evaluates a single active deny.trust axis against a
// metadata blob, returning true when the axis should DENY (so the
// caller short-circuits). It denies on a positive enum match AND on an
// unparseable blob — the latter fails CLOSED with a Warning so a
// corrupt or attacker-controlled trust annotation cannot slip an opt-in
// deny axis by being unreadable. Only invoked when the deny flag is
// already known active.
func denyTrustAxis(d *Decision, path string, raw json.RawMessage, blob, field, target, denyMsg string) bool {
	match, parseOK := ContainsEnumFieldChecked(raw, field, target)
	if !parseOK {
		msg := fmt.Sprintf("SEP-1913 %s could not be parsed as a JSON object; failing closed on %s", blob, path)
		d.Warnings = append(d.Warnings, Warning{Kind: "TrustMetadataUnparseable", Message: msg})
		addTrace(d, path, StatusFail, msg, "")
		failedOn(d, path, msg)
		return true
	}
	if match {
		addTrace(d, path, StatusFail, denyMsg, "")
		failedOn(d, path, denyMsg)
		return true
	}
	return false
}

// checkConstraints evaluates each tool.Args.Constraints[i].CEL expression
// against the args struct. The first false (or any internal error) ends
// the pipeline.
func checkConstraints(d *Decision, tool *mcpspec.Tool, args map[string]any) (bool, error) {
	if len(tool.Args.Constraints) == 0 {
		return true, nil
	}
	env, err := toolscel.MCPEnv()
	if err != nil {
		return false, fmt.Errorf("cel env: %w", err)
	}
	st, err := structpb.NewStruct(args)
	if err != nil {
		return false, fmt.Errorf("args to struct: %w", err)
	}
	for i, c := range tool.Args.Constraints {
		path := fmt.Sprintf("constraints[%d]", i)
		// Each of the four ways evaluation can break returns the SAME typed
		// error, differing only in Stage. They are one fact to everyone
		// downstream — this rule cannot be consulted, so the tool is
		// unusable until someone edits the spec — and splitting them into
		// four error shapes would make every consumer re-join them.
		ast, iss := env.Compile(c.CEL)
		if iss != nil && iss.Err() != nil {
			return false, &ConstraintError{Path: path, Stage: "compile", CEL: c.CEL, Err: iss.Err()}
		}
		prog, perr := env.Program(ast, celbudget.ProgramOptions()...)
		if perr != nil {
			return false, &ConstraintError{Path: path, Stage: "program", CEL: c.CEL, Err: perr}
		}
		out, _, eerr := prog.Eval(map[string]any{"args": st})
		if eerr != nil {
			return false, &ConstraintError{Path: path, Stage: "eval", CEL: c.CEL, Err: eerr}
		}
		v, ok := out.Value().(bool)
		if !ok {
			return false, &ConstraintError{
				Path: path, Stage: "result", CEL: c.CEL,
				Err: fmt.Errorf("did not evaluate to bool (got %T)", out.Value()),
			}
		}
		if !v {
			msg := c.Message
			if msg == "" {
				msg = "constraint failed"
			}
			addTrace(d, path, StatusFail, msg, c.CEL)
			failedOn(d, path, msg)
			return false, nil
		}
		addTrace(d, path, StatusPass, "", c.CEL)
	}
	return true, nil
}
