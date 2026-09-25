package validator

// Per-phase rule evaluation called by Check. Each helper returns either:
//   • (true, nil)   — phase passed; Check continues to the next phase
//   • (false, nil)  — phase failed and recorded the failure on the Decision;
//                     Check should return finalize(d, r)
//   • (false, err)  — internal error (e.g. malformed CEL); Check returns it

import (
	"fmt"

	semver "github.com/Masterminds/semver/v3"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/effect"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// checkRevision verifies the spec was authored against the toolkit's
// current revision.
func checkRevision(d *Decision, tk *toolkit.Toolkit, sp *spec.Spec) bool {
	if sp.Toolkit.Revision != tk.ToolkitRevision {
		msg := fmt.Sprintf("spec authored for revision %q, toolkit is %q", sp.Toolkit.Revision, tk.ToolkitRevision)
		addTrace(d, "revision", StatusFail, msg, "")
		failedOn(d, "revision", msg)
		return false
	}
	addTrace(d, "revision", StatusPass, "", "")
	return true
}

// checkBinaryVersion verifies the supplied invocation BinaryVersion (when
// present) is in toolkit.Target.VersionRange. Returns an internal error
// only if the toolkit's version range is itself malformed.
func checkBinaryVersion(d *Decision, tk *toolkit.Toolkit, sp *spec.Spec, inv Invocation) (bool, error) {
	if inv.BinaryVersion == "" {
		if sp.Require.VerifiedBinaryVersion {
			msg := "require.verifiedBinaryVersion=true but no BinaryVersion supplied"
			addTrace(d, "binaryVersion", StatusFail, msg, "")
			failedOn(d, "binaryVersion", msg)
			return false, nil
		}
		addTrace(d, "binaryVersion", StatusSkip, "BinaryVersion not supplied", "")
		d.Warnings = append(d.Warnings, Warning{Kind: "VersionUnverified", Message: "BinaryVersion was not supplied by the caller"})
		return true, nil
	}
	if tk.Target.VersionRange == "" {
		addTrace(d, "binaryVersion", StatusSkip, "no versionRange declared", "")
		return true, nil
	}
	v, err := semver.NewVersion(inv.BinaryVersion)
	if err != nil {
		msg := fmt.Sprintf("BinaryVersion %q is not valid semver", inv.BinaryVersion)
		addTrace(d, "binaryVersion", StatusFail, msg, "")
		failedOn(d, "binaryVersion", msg)
		return false, nil
	}
	c, err := semver.NewConstraint(tk.Target.VersionRange)
	if err != nil {
		return false, fmt.Errorf("toolkit target.versionRange invalid: %w", err)
	}
	if !c.Check(v) {
		msg := fmt.Sprintf("BinaryVersion %s not in %s", inv.BinaryVersion, tk.Target.VersionRange)
		addTrace(d, "binaryVersion", StatusFail, msg, "")
		failedOn(d, "binaryVersion", msg)
		return false, nil
	}
	addTrace(d, "binaryVersion", StatusPass, fmt.Sprintf("%s ∈ %s", inv.BinaryVersion, tk.Target.VersionRange), "")
	return true, nil
}

// checkAllowSubcommands enforces the spec's allow-list of subcommands.
func checkAllowSubcommands(d *Decision, sp *spec.Spec, call *parser.Call) bool {
	if !containsString(sp.AllowSubcommands, call.Subcommand) {
		msg := fmt.Sprintf("%q is not in allowSubcommands (%v)", call.Subcommand, sp.AllowSubcommands)
		addTrace(d, "allowSubcommands", StatusFail, msg, "")
		failedOn(d, "allowSubcommands", msg)
		return false
	}
	addTrace(d, "allowSubcommands", StatusPass, "", "")
	return true
}

// checkStructured runs the structured rules, applies any matching
// exceptions, and reports the first unlifted deny.
func checkStructured(d *Decision, sp *spec.Spec, resolved effect.Resolved, callMap, config map[string]any) (bool, error) {
	denies := evaluateStructuredRules(sp, resolved)

	unlifted, err := applyExceptionsWithMap(sp, denies, callMap, config, d)
	if err != nil {
		return false, err
	}
	if len(unlifted) > 0 {
		first := unlifted[0]
		addTrace(d, first.Path, StatusFail, first.Message, "")
		failedOn(d, first.Path, first.Message)
		return false, nil
	}
	addTrace(d, "structured", StatusPass, "", "")
	return true, nil
}

// checkConstraints evaluates each spec.Constraints[i] CEL expression
// against the call map. The first false (or any internal error) ends
// the pipeline.
func checkConstraints(d *Decision, sp *spec.Spec, callMap, config map[string]any) (bool, error) {
	for i, c := range sp.Constraints {
		path := fmt.Sprintf("constraints[%d]", i)
		prog, err := compileExpression(c.CEL)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		ok, err := evalBool(prog, callMap, config)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		if !ok {
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

// applyExceptionsWithMap evaluates spec.Exceptions[*] against any structured
// denies, lifting matches; the residual denies (if any) cause a Decision
// deny in checkStructured.
func applyExceptionsWithMap(sp *spec.Spec, denies []ruleDeny, callMap, config map[string]any, d *Decision) ([]ruleDeny, error) {
	if len(denies) == 0 || len(sp.Exceptions) == 0 {
		return denies, nil
	}
	var unlifted []ruleDeny
	for _, deny := range denies {
		lifted := false
		for idx, ex := range sp.Exceptions {
			if !containsString(ex.Overrides, deny.Path) {
				continue
			}
			prog, err := compileExpression(ex.When)
			if err != nil {
				return nil, fmt.Errorf("exceptions[%d]: %w", idx, err)
			}
			ok, err := evalBool(prog, callMap, config)
			if err != nil {
				return nil, fmt.Errorf("exceptions[%d]: %w", idx, err)
			}
			if ok {
				addTrace(d, fmt.Sprintf("exceptions[%d]", idx), StatusOverrideApplied,
					fmt.Sprintf("lifted %s: %s", deny.Path, ex.Message), ex.When)
				lifted = true
				break
			}
		}
		if !lifted {
			unlifted = append(unlifted, deny)
		}
	}
	return unlifted, nil
}

// callAsMap projects the parsed call + invocation + resolved effects into the
// shape the CEL engine binds (the `call` root activation). resourceID is the
// canonical resource-instance id this call names (or "" when it names no
// argument-derived external instance), exposed as call.resourceId so a
// constraint can gate on WHICH resource a call reaches — see resolveResourceID.
func callAsMap(call *parser.Call, inv Invocation, resolved effect.Resolved, resourceID string) map[string]any {
	flags := map[string]any{}
	for k, v := range call.Flags {
		flags[k] = v
	}
	positional := map[string]any{}
	for k, v := range call.Positional {
		positional[k] = v
	}
	env := map[string]string{}
	if inv.Env != nil {
		for k, v := range inv.Env {
			env[k] = v
		}
	}
	return map[string]any{
		"subcommand":     call.Subcommand,
		"subcommandPath": call.SubcommandPath,
		"argv":           call.Argv,
		"tail":           call.Tail,
		"flags":          flags,
		"positional":     positional,
		"env":            env,
		"cwd":            inv.Cwd,
		"binaryVersion":  inv.BinaryVersion,
		"resourceId":     resourceID,
		"effects": map[string]any{
			"destructive": resolved.Destructive,
			"reads":       resolved.Reads,
			"writes":      resolved.Writes,
			"network":     map[string]any{"destinations": resolved.Network.Destinations},
			"filesystem":  map[string]any{"paths": resolved.Filesystem.Paths},
			"creds": map[string]any{
				"required": resolved.Creds.Required,
				"writes":   resolved.Creds.Writes,
			},
		},
	}
}
