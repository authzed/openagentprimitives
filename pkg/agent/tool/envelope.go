package tool

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// EnvelopeProblemKind distinguishes the classes of defect, so a caller can
// apply its own policy to one class without matching on Detail's prose.
type EnvelopeProblemKind string

const (
	// ProblemDuplicateName: two tools share an LLM-visible name. Always fatal.
	ProblemDuplicateName EnvelopeProblemKind = "duplicate_name"

	// ProblemUnhandleable: the tool cannot mint a permsurface handle, so it is
	// ABSENT FROM THE SURFACE YET STILL CALLABLE.
	//
	// Not fatal on its own, because a cluster whose plan gate is off is
	// unharmed by it and hard-failing would break one over a name that works.
	// It becomes fatal the moment anything ENFORCES against the surface — see
	// PlanGateEnforcingMakesUnhandleableFatal.
	ProblemUnhandleable EnvelopeProblemKind = "unhandleable"
)

// EnvelopeProblem is one defect found in an assembled tool envelope.
type EnvelopeProblem struct {
	// Tool is the LLM-visible tool name the problem concerns.
	Tool string
	// Kind classifies the defect. Callers branch on this rather than on
	// Detail, which is prose and free to be reworded.
	Kind EnvelopeProblemKind
	// Detail is an operator-facing explanation.
	Detail string
	// Fatal means the runner must refuse to start, for this problem ALONE.
	// A caller enforcing against the permission surface must additionally
	// treat ProblemUnhandleable as fatal; see FatalFor.
	Fatal bool
}

// FatalFor reports whether this envelope must refuse to start, given whether
// anything is ENFORCING against the permission surface.
//
// The escalation this implements was promised in three separate places — this
// file, permsurface.Enumerate's doc, and the runner's own comment — each saying
// an unhandleable tool "becomes fatal when the plan gate lands". The plan gate
// landed and denies; the escalation had not happened, so a tool that cannot
// mint a handle was absent from the surface, ungoverned by the gate (the gate's
// no-handle branch records OutcomeAllow), and dispatchable anyway.
//
// That is the one failure mode the surface must never allow: enforcing against
// a surface known to be incomplete, where the incompleteness is exactly what
// escapes enforcement.
//
// Conditioned on enforcement rather than made unconditional because the
// original reasoning still holds for everyone else: a cluster whose gate is off
// or logging is not harmed by such a tool, and refusing to start over a name
// that works would be a regression for them with nothing gained.
func FatalFor(problems []EnvelopeProblem, surfaceEnforced bool) bool {
	for _, p := range problems {
		if p.Fatal {
			return true
		}
		if surfaceEnforced && p.Kind == ProblemUnhandleable {
			return true
		}
	}
	return false
}

func (p EnvelopeProblem) String() string {
	sev := "warning"
	if p.Fatal {
		sev = "fatal"
	}
	return fmt.Sprintf("%s: tool %q: %s", sev, p.Tool, p.Detail)
}

// ValidateEnvelope reports defects in an assembled tool envelope.
//
// Two classes, deliberately different severities:
//
//   - A DUPLICATE tool name is fatal. The dispatch map is built as
//     byName[t.Name()] = t, so a duplicate silently shadows — the LLM calls one
//     tool and a different one runs. That is broken today, independent of any
//     permission feature.
//
//   - A tool that cannot mint a permsurface handle is a warning. It is harmless
//     until plan-gating exists, and hard-failing would break running clusters
//     over a name that currently works. It becomes fatal when the plan gate
//     lands, because from then on such a tool would be absent from the surface
//     yet still callable — the one failure mode the surface must never allow.
func ValidateEnvelope(tools []Tool) []EnvelopeProblem {
	var problems []EnvelopeProblem

	seen := map[string]bool{}
	for _, t := range tools {
		name := t.Name()
		if seen[name] {
			problems = append(problems, EnvelopeProblem{
				Tool:   name,
				Kind:   ProblemDuplicateName,
				Detail: "duplicate tool name in the envelope; the later tool would silently shadow the earlier one at dispatch",
				Fatal:  true,
			})
			continue
		}
		seen[name] = true

		for _, p := range candidatePermissions(t) {
			if err := handleable(name, p); err != nil {
				problems = append(problems, EnvelopeProblem{
					Tool:   name,
					Kind:   ProblemUnhandleable,
					Detail: fmt.Sprintf("cannot mint a permission handle: %v", err),
				})
				break // one report per tool is enough for an operator
			}
		}
	}
	return problems
}

// Candidates projects an assembled envelope into the permsurface input.
//
// permsurface deliberately does not import this package, so the projection
// lives here. Tools whose permission is outside the surface (stateless,
// passthrough) still appear as candidates; Enumerate drops them, so the filter
// stays in one place rather than being duplicated at every call site.
func Candidates(tools []Tool) []permsurface.Candidate {
	out := make([]permsurface.Candidate, 0, len(tools))
	for _, t := range tools {
		out = append(out, permsurface.Candidate{
			ToolName:   t.Name(),
			Permission: t.Permission(),
			Variants:   t.PermissionVariants(),
		})
	}
	return out
}

// BaseHandle returns the handle a tool's BASE permission mints, and ok=false
// when it has none (a meta or passthrough tool, which the plan gate does not
// govern).
//
// This resolves per TOOL, not per CALL. A tool with permission variants is
// checked against whichever variant's CEL condition matches its arguments, so a
// fully correct per-call resolution is argument-aware. That distinction does not
// bite under the synthesized session ceiling, which contains the entire surface:
// every variant of every tool is inside it, so the base handle cannot disagree
// with the dispatcher about membership. It DOES bite once phases carry narrower
// ceilings, and arg-aware resolution lands with them.
func BaseHandle(t Tool) (permsurface.Handle, bool) {
	return HandleForPermission(t.Permission(), t.Name())
}

// HandleForPermission mints the handle a Permission is checked against.
//
// Split out of BaseHandle so per-CALL resolution can reuse it: a sandbox tool
// resolves a different Permission per subcommand, and the plan gate needs the
// handle for the permission that call actually landed on. Two derivations of
// "which handle is this" would be free to disagree about what a call means,
// which is the one thing the gate and the dispatcher must not do.
func HandleForPermission(p authz.Permission, toolName string) (permsurface.Handle, bool) {
	switch p.StateImpact {
	case authz.Readonly, authz.Readwrite, authz.External:
	default:
		return permsurface.Handle{}, false
	}
	var (
		h   permsurface.Handle
		err error
	)
	if p.Check != nil && p.Check.Permission != "" && p.Check.ResourceType != "" {
		h, err = permsurface.NewPermHandle(p.Check.Permission, p.Check.ResourceType)
	} else {
		h, err = permsurface.NewToolHandle(toolName)
	}
	if err != nil {
		return permsurface.Handle{}, false
	}
	return h, true
}

// candidatePermissions returns every Permission a tool can dispatch under:
// each variant's Check plus the base Permission.
func candidatePermissions(t Tool) []authz.Permission {
	vs := t.PermissionVariants()
	out := make([]authz.Permission, 0, len(vs)+1)
	for _, v := range vs {
		out = append(out, v.Check)
	}
	return append(out, t.Permission())
}

// handleable reports whether p on toolName can mint a handle. Permissions
// outside the surface (stateless, passthrough) are trivially fine — they are
// never declared and never gated.
func handleable(toolName string, p authz.Permission) error {
	switch p.StateImpact {
	case authz.Readonly, authz.Readwrite, authz.External:
	default:
		return nil
	}
	var err error
	if p.Check != nil && p.Check.Permission != "" && p.Check.ResourceType != "" {
		_, err = permsurface.NewPermHandle(p.Check.Permission, p.Check.ResourceType)
	} else {
		_, err = permsurface.NewToolHandle(toolName)
	}
	return err
}

// CanBeGated reports whether the plan gate can ever gate this tool.
//
// "Ever" because resolution is per CALL: a sandbox tool is one tool for a whole
// CLI, and its BASE permission is the toolkit default — passthrough for gh —
// while its subcommands carry real checks. Counting base handles alone reported
// a gh tool as ungateable and produced the contradictory startup line
// `surfaceHandles=2 gatedTools=0`: handles found, none usable. A number that
// disagrees with the gate's actual behaviour is worse than no number, because
// it reads as a failure.
func CanBeGated(t Tool) bool {
	for _, p := range candidatePermissions(t) {
		if _, ok := HandleForPermission(p, t.Name()); ok {
			return true
		}
	}
	return false
}
