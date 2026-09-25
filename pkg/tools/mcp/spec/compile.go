package spec

import (
	"fmt"

	"github.com/google/cel-go/cel"

	"github.com/authzed/openagentprimitives/pkg/authz"
	toolscel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// Result is what Compile returns. Warnings are non-fatal advisories
// (e.g. Deny.Effects fields the MCP validator cannot enforce against
// probed annotations).
type Result struct {
	Warnings []Warning
}

// Warning is one non-fatal compile-time note.
type Warning struct {
	Path    string
	Message string
}

// Compile type-checks every tools[].args.constraints[].cel and returns
// any non-fatal warnings about the spec's structure (e.g. Deny.Effects
// fields the MCP runtime cannot enforce). The returned error is reserved
// for fatal compile errors (a CEL expression that doesn't type-check).
func Compile(sp *Spec) (*Result, error) {
	res := &Result{}
	if sp == nil {
		return res, nil
	}
	for i, tool := range sp.Tools {
		for j, c := range tool.Args.Constraints {
			if err := toolscel.MCPCompileExpr(c.CEL); err != nil {
				return nil, fmt.Errorf("tools[%d].args.constraints[%d]: %v", i, j, err)
			}
		}
		// Non-fatal lint: introspect_tool surfaces Constraint.Message as
		// the agent-facing justification of a CEL gate, so a constraint
		// with CEL but no message degrades to a generic "an additional
		// constraint applies" line. Warn, don't fail.
		for _, lint := range toolspec.LintConstraintMessages(tool.Args.Constraints) {
			res.Warnings = append(res.Warnings, Warning{
				Path:    fmt.Sprintf("tools[%d].args.constraints", i),
				Message: lint,
			})
		}
		// Deny.Effects.Destructive is wired to the validator; the
		// other DenyEffects fields (Reads, Writes, Creds.Writes) have
		// no probed-annotation analog yet, so warn when they're set.
		de := tool.Deny.Effects
		if len(de.Reads) > 0 {
			res.Warnings = append(res.Warnings, Warning{
				Path:    fmt.Sprintf("tools[%d].deny.effects.reads", i),
				Message: "Reads is not enforceable for MCP tools (no probed-annotation analog); use a constraint instead",
			})
		}
		if len(de.Writes) > 0 {
			res.Warnings = append(res.Warnings, Warning{
				Path:    fmt.Sprintf("tools[%d].deny.effects.writes", i),
				Message: "Writes is not enforceable for MCP tools (no probed-annotation analog); use a constraint instead",
			})
		}
		if de.Creds.Writes {
			res.Warnings = append(res.Warnings, Warning{
				Path:    fmt.Sprintf("tools[%d].deny.effects.creds.writes", i),
				Message: "Creds.Writes is not enforceable for MCP tools; use a constraint instead",
			})
		}
		// Destructive without an Effects snapshot to test against is also worth surfacing.
		if de.Destructive && !tool.Effects.Destructive {
			res.Warnings = append(res.Warnings, Warning{
				Path:    fmt.Sprintf("tools[%d].deny.effects.destructive", i),
				Message: "deny.effects.destructive is set but the probed effects snapshot does not mark this tool destructive — the rule will be a no-op until the probe is re-run",
			})
		}
	}
	// Slice-4 additions: one-of for resourceIDTemplate/resourceIDExpr;
	// CEL compile checks for variant.When and resourceIDExpr.
	for i, t := range sp.Tools {
		if t.Permission != nil && t.Permission.Check != nil {
			if err := validateResourceIDForms(t.Permission.Check); err != nil {
				return nil, fmt.Errorf("tools[%d].permission.check: %w", i, err)
			}
		}
		for j, v := range t.PermissionVariants {
			if _, err := compileWhenCEL(v.When); err != nil {
				return nil, fmt.Errorf("tools[%d].permissionVariants[%d].when: %w", i, j, err)
			}
			if v.Check.Check != nil {
				if err := validateResourceIDForms(v.Check.Check); err != nil {
					return nil, fmt.Errorf("tools[%d].permissionVariants[%d].check: %w", i, j, err)
				}
				if v.Check.Check.ResourceIDExpr != "" {
					if _, err := compileExprCEL(v.Check.Check.ResourceIDExpr); err != nil {
						return nil, fmt.Errorf("tools[%d].permissionVariants[%d].check.resourceIDExpr: %w", i, j, err)
					}
				}
			}
		}
	}
	return res, nil
}

func validateResourceIDForms(c *authz.PermissionCheck) error {
	hasTpl := c.ResourceIDTemplate != ""
	hasExpr := c.ResourceIDExpr != ""
	if hasTpl == hasExpr {
		return fmt.Errorf("exactly one of resourceIDTemplate or resourceIDExpr must be set")
	}
	return nil
}

// compileWhenCEL type-checks a permissionVariants[].when expression
// using the shared authz CEL evaluator. The returned Program is
// discarded at spec-compile time — variants are re-compiled at runtime
// load — but compiling here surfaces type errors at admission.
func compileWhenCEL(expr string) (cel.Program, error) { return authz.CompileBool(expr) }

// compileExprCEL type-checks a permission.check.resourceIDExpr (or
// variant-level analog) expression. See compileWhenCEL for the runtime
// re-compile note.
func compileExprCEL(expr string) (cel.Program, error) { return authz.CompileString(expr) }
