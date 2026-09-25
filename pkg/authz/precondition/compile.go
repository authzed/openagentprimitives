// Package precondition compiles a slot precondition — a CEL predicate over the
// facts recorded about one candidate resource instance — and extracts, up
// front, the complete set of facts that predicate reads.
//
// The reference set is the load-bearing half. A precondition evaluates
// TRI-STATE: satisfied, refused, or undetermined, and undetermined is decided
// by asking whether every referenced fact has been recorded yet. A reference
// missed here is a fact nobody checks for presence, which silently turns
// undetermined into satisfied and opens the gate. The walk is therefore written
// to REFUSE every use of `facts` it does not recognize rather than to skip it:
// an unrecognized shape becomes a compile error an author sees at admission,
// never an unchecked read at dispatch. See factReferences.
//
// The set is extracted statically rather than inferred from evaluation errors.
// The tempting alternative — build an activation holding only the facts already
// recorded and read cel-go's "no such key" as undetermined — makes EVERY
// evaluation error read as undetermined, so a genuine expression bug (a type
// mismatch, a bad operator) silently becomes "deny, not yet known": the slot
// stays shut for a reason nobody can see and the author never learns the
// predicate is broken. Extracting up front keeps "this fact is not here yet"
// distinct from "this expression is wrong".
package precondition

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
)

// The two fact provenances (§1 of the design). Provenance is part of a fact's
// address because it is a trust grade: a fact the platform derived from a
// signed envelope is a stronger claim than one derived from a response to a
// call the agent shaped, and an author who gated on the former must not
// silently receive the latter.
const (
	ProvenanceEnvelope = "envelope"
	ProvenanceObserved = "observed"
)

// The only two bindings a precondition sees. Every other identifier is an
// "undeclared reference" compile error, which is what makes `facts` the single
// door fact data can enter through — and therefore what makes the reference
// walk's enumeration of `facts` identifiers exhaustive.
const (
	factsVar = "facts"
	slotVar  = "slot"
)

// FactRef names one fact a predicate reads: `facts.<Provenance>.<Name>`.
type FactRef struct {
	Provenance string
	Name       string
}

// Compiled is a precondition ready to evaluate, together with the facts it
// reads. The two travel as one value because evaluating the program without
// first checking the references for presence is precisely the bypass this
// package exists to prevent.
type Compiled struct {
	expr string
	prg  cel.Program
	refs []FactRef
}

// Expression returns the predicate's source text, for messages that must quote
// the rule back to an author or an approver.
func (c *Compiled) Expression() string { return c.expr }

// Program returns the compiled program. Callers evaluate it only after every
// reference reported by References is known to be recorded.
func (c *Compiled) Program() cel.Program { return c.prg }

// References returns every fact the predicate reads, deduplicated and sorted by
// provenance then name. The slice is a copy: this set decides undetermined, and
// a caller that could shrink it in place could bind a slot no observation has
// cleared.
func (c *Compiled) References() []FactRef { return slices.Clone(c.refs) }

// celEnv builds the precondition environment.
//
// This is a deliberately SEPARATE environment from pkg/authz's celEnv, not a
// fork of it: the bindings are disjoint (facts/slot here, args/result/item
// there) and nothing a precondition decides touches a tool call's arguments.
// The separation is spelled out because forking that env and then diverging is
// exactly how pkg/authz/relwrites silently lost spicedb_user_id — the
// production failure pkg/authz.SpiceDBUserIDFunction's doc comment records. So
// this env starts from nothing and adds only what a fact predicate needs, and a
// change to the pkg/authz env is deliberately not inherited here.
//
// Nothing extra is registered, on purpose. No custom functions, and no
// ext.Strings(): the standard library already carries the comparisons, `in`,
// size() and the startsWith/contains/matches tests a fact predicate needs.
// Extension surface is easy to add later and effectively impossible to remove
// once specs are authored against it.
//
// EnableMacroCallTracking is not decoration: it is what populates
// SourceInfo().MacroCalls(), which rejectFactPresenceProbe reads to find has()
// over a fact and which walkRoots walks as a second source of references.
func celEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(factsVar, cel.DynType),
		cel.Variable(slotVar, cel.DynType),
		cel.EnableMacroCallTracking(),
	)
}

// Compile type-checks a precondition expression and extracts the facts it
// reads. Every way the expression could read a fact the caller cannot name is
// an error, not a silently unchecked read.
func Compile(expr string) (*Compiled, error) {
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("precondition: CEL env: %w", err)
	}
	checked, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("precondition: compile %q: %w", expr, iss.Err())
	}

	// Unlike pkg/authz.CompileBool, dyn is NOT accepted. That helper tolerates
	// it because a resourceIDExpr pulling a field out of a nested map checks as
	// dyn and is coerced at eval time. A precondition has no such latitude: it
	// decides whether a slot binds, and one that might not be a bool is one that
	// might not decide.
	if out := checked.OutputType(); !out.IsExactType(cel.BoolType) {
		// The message names the fix as well as the reason: bare `facts.observed.ok`
		// checks as dyn because every fact is dyn, and it is the mistake an author
		// makes first. Someone waiting on this error needs to know what to write.
		return nil, fmt.Errorf(
			"precondition: expression %q must return bool, got %s: a precondition decides whether a slot binds, so it may not evaluate to a value that is not a decision — compare the fact explicitly, as in `facts.observed.ok == true` rather than `facts.observed.ok`",
			expr, out)
	}

	native := checked.NativeRep()
	if err := rejectShadowedBindings(expr, native); err != nil {
		return nil, err
	}
	if err := rejectFactPresenceProbe(expr, native); err != nil {
		return nil, err
	}
	refs, err := factReferences(expr, native)
	if err != nil {
		return nil, err
	}

	// Bounded like every other CEL program in the tree. This site was missed by
	// the sweep that added the budget -- it constructs from `checked` rather than
	// `ast`, so a grep for the common spelling walked past it -- and it is the
	// one compiling TENANT-AUTHORED preconditions, which is the input the budget
	// exists for.
	prg, err := env.Program(checked, celbudget.ProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("precondition: program %q: %w", expr, err)
	}
	return &Compiled{expr: expr, prg: prg, refs: refs}, nil
}

// walkRoots returns every expression tree that must be inspected: the checked
// (macro-EXPANDED) AST, plus every original macro call the parser recorded.
//
// The expanded tree is what will actually be evaluated, so it is the authority,
// and against cel-go v0.26.0 the standard macros all preserve their operands
// into it — exists/all/filter/map keep the iteration range and the predicate
// body, has() keeps its target. The recorded macro calls are walked anyway
// because a macro expansion is free to drop a sub-expression the author wrote,
// and the cost of that happening is a fact read nobody checks for presence.
// Walking the author's own form too makes that unrepresentable, and the extra
// references it could only ADD are conservative: one more reference can hold a
// slot undetermined, but it can never bind one.
//
// NO TEST PINS THE SECOND ROOT SET, and that is not an oversight to fix by
// deleting it. Every expression authorable against celEnv today reaches the same
// reference set from the expanded tree alone, so removing the loop below leaves
// the suite green — pinning it honestly would take a macro that drops a
// sub-expression from its expansion, and cel-go v0.26.0 ships none. It earns its
// place the day one appears: a new cel-go release, or a macro registered on this
// env (ext.Bindings, ext.TwoVarComprehensions, a custom one). Whoever registers
// such a macro should add the row that makes this load-bearing; until then it is
// insurance against a change nobody here can write a test for.
func walkRoots(native *celast.AST) []celast.NavigableExpr {
	roots := []celast.NavigableExpr{celast.NavigateAST(native)}
	for _, macro := range native.SourceInfo().MacroCalls() {
		if macro == nil {
			continue
		}
		roots = append(roots, celast.NavigateExpr(native, macro))
	}
	return roots
}

// matchAll walks every root with cel-go's own traversal and returns the nodes
// of the given kind.
//
// The traversal is deliberately the library's (ast.MatchDescendants over
// ast.visit) rather than a switch written here. That visit covers every
// expression kind — call target and arguments, comprehension range/init/
// condition/step/result, list elements, map keys and values, struct fields,
// select operands — and a kind added by a future cel-go is covered by it on the
// day we upgrade. A hand-rolled walk would instead skip the new kind in
// silence, and a skipped node is a fact reference nobody records.
func matchAll(native *celast.AST, kind celast.ExprKind) []celast.NavigableExpr {
	var out []celast.NavigableExpr
	for _, root := range walkRoots(native) {
		out = append(out, celast.MatchDescendants(root, celast.KindMatcher(kind))...)
	}
	return out
}

// factChain reports whether e is a chain of selects rooted at the `facts`
// binding, returning the field names from the root outward: `facts.observed.x`
// yields ["observed", "x"].
func factChain(e celast.Expr) ([]string, bool) {
	var fields []string
	for {
		switch e.Kind() {
		case celast.SelectKind:
			sel := e.AsSelect()
			fields = append(fields, sel.FieldName())
			e = sel.Operand()
		case celast.IdentKind:
			if e.AsIdent() != factsVar {
				return nil, false
			}
			slices.Reverse(fields)
			return fields, true
		default:
			return nil, false
		}
	}
}

// rejectShadowedBindings refuses a comprehension that binds an iteration
// variable named `facts` or `slot`.
//
// cel-go allows the shadowing, and it would put this walk and the running
// expression into disagreement about what `facts.observed.x` denotes: the walk
// would record a fact reference for an expression that never touches the facts
// map. That direction is fail-closed — an extra reference only holds the slot
// undetermined — but a rule nobody can read is worse than one that is refused,
// and an author who shadows a binding is confused rather than clever.
//
// It also keeps the reference walk's identifier matching sound. With shadowing
// refused and no let-binding extension registered (celEnv adds none), a `facts`
// identifier can only be the declared variable.
func rejectShadowedBindings(expr string, native *celast.AST) error {
	for _, node := range matchAll(native, celast.ComprehensionKind) {
		comp := node.AsComprehension()
		// Every name a comprehension binds, not just the iteration variables. The
		// accumulator is unauthorable today — the standard macros name it @result
		// and ext.Bindings, which would let an author choose it, is not registered
		// — so this arm cannot fire. It is here because the comment above leans on
		// "no let-binding extension registered", and a rule that holds only while
		// that stays true should check the case rather than assume it.
		bound := []string{comp.IterVar(), comp.AccuVar()}
		if comp.HasIterVar2() {
			bound = append(bound, comp.IterVar2())
		}
		for _, name := range bound {
			if name == factsVar || name == slotVar {
				return fmt.Errorf(
					"precondition: expression %q: %q may not be a comprehension variable because it shadows the %s binding, which would make the extracted fact references and the running expression disagree about what %s.x reads",
					expr, name, name, name)
			}
		}
	}
	return nil
}

// rejectFactPresenceProbe refuses has() over a fact.
//
// An unwritten fact is ABSENT, not null, and referencing one yields
// undetermined rather than false. has() would hand the author a decidable
// boolean for exactly that condition, collapsing the tri-state the gate depends
// on and reintroducing the order-dependence monotonicity exists to remove: an
// expression could bind a slot precisely because no observation had cleared it
// yet.
//
// Probing INSIDE a fact's value is a different thing and stays allowed —
// has(facts.observed.pr.head) still requires the fact `pr` to be recorded, so
// an absent fact is undetermined no matter what the expression does with it.
// Only a probe of the fact's own name (or of a whole provenance namespace)
// collapses the tri-state, which is why the refusal is keyed on the length of
// the select chain.
func rejectFactPresenceProbe(expr string, native *celast.AST) error {
	// The recorded macro call is the author's original `has(...)` form. Reading
	// it beats matching the expression text, which would trip over a string
	// literal containing "has(" and would miss nothing else.
	for _, macro := range native.SourceInfo().MacroCalls() {
		if macro == nil || macro.Kind() != celast.CallKind {
			continue
		}
		call := macro.AsCall()
		if call.FunctionName() != operators.Has || len(call.Args()) != 1 {
			continue
		}
		if fields, ok := factChain(call.Args()[0]); ok && len(fields) <= 2 {
			return factPresenceProbeErr(expr)
		}
	}

	// Belt to that brace: a test-only select is producible only by has(), and it
	// survives into the expanded AST whether or not macro-call tracking recorded
	// the original form. celEnv turns that tracking on, so this second pass is
	// redundant today — and it is what keeps the refusal from turning into a
	// silent bypass if that option is ever lost in an edit or an upgrade.
	for _, node := range matchAll(native, celast.SelectKind) {
		if !node.AsSelect().IsTestOnly() {
			continue
		}
		if fields, ok := factChain(node); ok && len(fields) <= 2 {
			return factPresenceProbeErr(expr)
		}
	}
	return nil
}

func factPresenceProbeErr(expr string) error {
	return fmt.Errorf(
		"precondition: expression %q: has() is not available over facts because a fact that has not been recorded yet is undetermined, not false: has() would turn that into a decidable boolean and let the predicate bind a slot precisely because no observation had cleared it",
		expr)
}

// factReferences extracts every fact the expression reads.
//
// It enumerates `facts` identifiers rather than select shapes, because the
// identifier is the only door fact data can enter through: celEnv declares two
// variables and every other name is a compile error, so a read that reaches the
// facts map is rooted at one of these nodes. Each one must then be the base of
// exactly `facts.<provenance>.<name>` — anything else is refused by
// referenceAt. That inversion is the security property of this package: the
// walk never skips a use of `facts` it cannot classify, so the set of accepted
// shapes is exactly the set of extracted references, and an unhandled shape
// fails loudly at admission instead of quietly binding a slot at dispatch.
func factReferences(expr string, native *celast.AST) ([]FactRef, error) {
	seen := make(map[FactRef]struct{})
	for _, node := range matchAll(native, celast.IdentKind) {
		if node.AsIdent() != factsVar {
			continue
		}
		ref, err := referenceAt(expr, node)
		if err != nil {
			return nil, err
		}
		seen[ref] = struct{}{}
	}
	refs := make([]FactRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	// Sorted so status.factSources and any recorded hold reason are stable
	// across reconciles rather than churning with Go's map order.
	slices.SortFunc(refs, func(a, b FactRef) int {
		return cmp.Or(cmp.Compare(a.Provenance, b.Provenance), cmp.Compare(a.Name, b.Name))
	})
	return refs, nil
}

// referenceAt turns one `facts` identifier into the fact it names, refusing
// every shape it cannot name. A deeper select (facts.observed.pr.head.sha)
// reads INTO a fact's value and still names the fact `pr` at its root, which is
// the thing whose presence decides undetermined.
func referenceAt(expr string, ident celast.NavigableExpr) (FactRef, error) {
	provNode, ok := ident.Parent()
	if !ok || provNode.Kind() != celast.SelectKind {
		return FactRef{}, malformedFactAddressErr(expr)
	}
	nameNode, ok := provNode.Parent()
	if !ok || nameNode.Kind() != celast.SelectKind {
		return FactRef{}, malformedFactAddressErr(expr)
	}
	provenance := provNode.AsSelect().FieldName()
	if provenance != ProvenanceEnvelope && provenance != ProvenanceObserved {
		// Not treated as a fact that simply never arrives: a typo'd namespace
		// would then hold the slot undetermined forever, and the author would be
		// looking for a missing observation instead of a missing letter.
		return FactRef{}, fmt.Errorf(
			"precondition: expression %q: unknown fact provenance %q: only %q and %q exist",
			expr, provenance, ProvenanceEnvelope, ProvenanceObserved)
	}
	return FactRef{Provenance: provenance, Name: nameNode.AsSelect().FieldName()}, nil
}

func malformedFactAddressErr(expr string) error {
	return fmt.Errorf(
		"precondition: expression %q: facts must be addressed as facts.<provenance>.<name>, with <provenance> one of %q or %q: index syntax (facts[\"observed\"][\"x\"]), a whole-namespace read (facts.observed) and passing facts to a function all read a fact this compiler cannot name, and a fact it cannot name is a fact it cannot check for presence",
		expr, ProvenanceEnvelope, ProvenanceObserved)
}
