package schema

import (
	"fmt"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
)

// UnresolvedReference is one name a definition asks for that the definition it
// would resolve against does not declare — either a name in a permission
// expression, or an allowed subject type on a direct relation.
type UnresolvedReference struct {
	// Definition is the object definition holding the reference.
	Definition string
	// Permission is the member carrying the reference: a permission, for a
	// finding out of an expression, or a direct RELATION, for a finding out of
	// its allowed subject types. The field keeps its name because callers log
	// it under that key; read it as "the member of Definition at fault".
	Permission string
	// Name is what resolves to nothing: a relation-or-permission name, a
	// subject type (`foo`), or a subject-set type (`foo#bar`).
	Name string
	// SubjectType marks a finding raised against a direct relation's allowed
	// subject types rather than against a permission expression.
	//
	// It exists because the two kinds answer different questions and the
	// callers need different answers. An expression finding is SELF-CONTAINED:
	// `permission read = owner` on a definition declaring no `owner` is wrong
	// no matter what else is merged in, so a caller holding a partial view can
	// refuse on it. A subject-type finding says only "not declared in THIS
	// text" — which is the whole point when the text has to stand alone (the
	// bare scaffold), and a false alarm when the caller deliberately holds one
	// fragment and the scaffold and the type comes from a sibling fragment.
	// ValidateFragment is that caller; see its own handling.
	SubjectType bool
}

// String renders a finding for logs and test failure messages.
func (r UnresolvedReference) String() string {
	return fmt.Sprintf("%s#%s references %q, which is not declared", r.Definition, r.Permission, r.Name)
}

// UnresolvedReferences reports every name src asks for that resolves to
// nothing — in a permission expression, or in a direct relation's allowed
// subject types.
//
// compiler.Compile is a PARSE. It builds the expression tree and stops; it
// never asks whether `owner` in `permission read = creator + owner` is a name
// the definition actually declares. composeAllParseOnly still bottoms out in
// exactly that parse — it is what ValidateFragment's own single-fragment
// compose and PartitionCompatibleFragments' trial composes run on a PARTIAL
// view of the eventual fragment set, deliberately (see composeAllParseOnly's
// doc), which is why both then call this function themselves rather than
// trusting the compile alone to have caught anything. ComposeBase no longer
// bottoms out in a bare parse: it composes through composeFragmentSet, which
// validates against spicedb's own type system, so a dangling reference in a
// compile-time fragment is now refused there directly. Where nothing
// upstream validates, a dangling reference would otherwise be refused only
// by SpiceDB at WriteSchema — once, cluster-wide, after every fragment has
// already been merged. This walks the parsed form and names those references
// before the write, for the callers that still need it.
//
// Resolution rules, which are the whole substance of the function:
//
//   - A permission may name a RELATION or another PERMISSION on its own
//     definition, so the declared set is the union of both.
//   - An arrow (`rel->perm`, and the functioned `rel.any(perm)` /
//     `rel.all(perm)`) has two halves that resolve in different places. The
//     LEFT half is a name on the local definition. The RIGHT half resolves on
//     whatever types the left half points at.
//   - An arrow's right half is reported only when it resolves on NONE of the
//     left relation's declared subject types, and only when at least one of
//     those types is a definition src declares. A relation may accept several
//     types and the arrow is satisfied by any of them; a type src does not
//     declare has no known member set to check against. Both cases skip.
//   - A DIRECT relation's allowed subject types are checked outright: a type
//     src does not declare is reported, and so is a subject-set type
//     (`foo#bar`) whose target definition declares no `bar`. This half is NOT
//     biased against false positives the way the arrow's right half is, and
//     the asymmetry is deliberate — an arrow may legitimately reach a type a
//     sibling fragment contributes, but a subject type is resolved by
//     SpiceDB's WriteSchema for EVERY schema it is given, including the base
//     scaffold written bare by the install bootstrap and by
//     test/testspicedb.WriteSchemaText. A schema that only resolves once some
//     other text is merged into it is exactly the bug this half catches:
//     `group#member` named `onepassword_group`, nothing declared it, and every
//     schema write in the repo failed at the server. A caller that KNOWS it
//     holds only part of the eventual schema drops these by their SubjectType
//     marker rather than being served a weaker answer here.
//
// The bias throughout is against false positives. A finding here is meant to
// be a live bug, and the caller is expected to log it rather than refuse the
// write — a resolver that gates on a name it merely failed to understand would
// freeze schema writes for the whole cluster.
//
// Findings are returned in definition order, then permission order, then the
// order the names appear in the expression, so repeated runs over the same
// source produce the same slice.
func UnresolvedReferences(src string) ([]UnresolvedReference, error) {
	namespaces, err := compiledNamespaces(src, "unresolved-references")
	if err != nil {
		return nil, err
	}
	// definition name → every name it declares, relations and permissions
	// alike. A permission expression may legitimately name either.
	declared := namespaceDeclaredNames(namespaces)

	var found []UnresolvedReference
	for _, ns := range namespaces {
		local := declared[ns.GetName()]
		// A relation's subject types are needed to resolve the right half of
		// any arrow over it, so index them once per definition.
		subjectTypes := make(map[string][]string, len(ns.GetRelation()))
		for _, rel := range ns.GetRelation() {
			subjectTypes[rel.GetName()] = allowedNamespaces(rel)
		}
		for _, rel := range ns.GetRelation() {
			w := &refWalker{
				definition:   ns.GetName(),
				permission:   rel.GetName(),
				local:        local,
				declared:     declared,
				subjectTypes: subjectTypes,
			}
			// A Relation entry with a rewrite is a permission; without one it
			// is a direct relation and has no expression to walk. Same
			// discriminator ComposeWithSkipped uses. The two are exclusive: a
			// permission carries no type information, a direct relation no
			// expression.
			if rewrite := rel.GetUsersetRewrite(); rewrite != nil {
				w.walkRewrite(rewrite)
			} else {
				w.checkSubjectTypes(rel)
			}
			found = append(found, w.found...)
		}
	}
	return found, nil
}

// compiledNamespaces compiles src and returns its object-type namespaces —
// the same parse UnresolvedReferences runs, factored out so a caller that
// only needs "what does this text declare" (definitionDeclaresName) does not
// duplicate the compile-and-filter step. sourceName is compiler.Compile's
// diagnostic label; callers pass one that identifies which caller is asking,
// since src itself may be a whole schema or a single definition's block.
func compiledNamespaces(src, sourceName string) ([]*core.NamespaceDefinition, error) {
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source(sourceName),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	var namespaces []*core.NamespaceDefinition
	for _, def := range compiled.OrderedDefinitions {
		// OrderedDefinitions interleaves caveats with namespaces; caveats
		// declare no relations and are never an arrow target.
		if ns, ok := def.(*core.NamespaceDefinition); ok {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces, nil
}

// namespaceDeclaredNames maps each namespace to every name it declares,
// relations and permissions alike — a permission compiles to a Relation
// carrying a UsersetRewrite, so a bare name reference could mean either.
func namespaceDeclaredNames(namespaces []*core.NamespaceDefinition) map[string]map[string]struct{} {
	declared := make(map[string]map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		names := make(map[string]struct{}, len(ns.GetRelation()))
		for _, rel := range ns.GetRelation() {
			names[rel.GetName()] = struct{}{}
		}
		declared[ns.GetName()] = names
	}
	return declared
}

// definitionDeclaresName reports whether src declares `name` (a relation or a
// permission) under the definition named `definition`. It is the same
// declared-name resolution UnresolvedReferences walks against, asked of one
// name instead of walked for danglers.
//
// ComposeSlots calls this with src set to just the target definition's own
// block — exactly the text definitionBlockBounds slices out — before deciding
// whether slotPermissionExpr's `owner` leg has anything to resolve against. A
// lone block compiles fine even when it references an external subject type
// nothing in the block declares (`relation viewer: user` with no `user`
// definition present): compiler.Compile is a parse, not a type-check, and only
// the block's OWN declared names are asked for here — see UnresolvedReferences'
// doc for why ITS resolution needs the whole schema and this one does not.
func definitionDeclaresName(src, definition, name string) (bool, error) {
	namespaces, err := compiledNamespaces(src, "definition-declared-names")
	if err != nil {
		return false, err
	}
	declared := namespaceDeclaredNames(namespaces)
	names, ok := declared[definition]
	if !ok {
		return false, nil
	}
	_, has := names[name]
	return has, nil
}

// refWalker carries the per-permission resolution context down the expression
// tree so the recursive walk stays a plain traversal.
type refWalker struct {
	definition string
	permission string
	// local is the set of names the definition under walk declares.
	local map[string]struct{}
	// declared is every definition's declared set, for resolving arrow targets.
	declared map[string]map[string]struct{}
	// subjectTypes maps a local relation to the object types it accepts.
	subjectTypes map[string][]string

	found []UnresolvedReference
}

func (w *refWalker) report(name string) {
	w.found = append(w.found, UnresolvedReference{
		Definition: w.definition,
		Permission: w.permission,
		Name:       name,
	})
}

// reportSubjectType records a finding raised against a direct relation's
// allowed subject types. See UnresolvedReference.SubjectType for why the two
// are told apart.
func (w *refWalker) reportSubjectType(name string) {
	w.found = append(w.found, UnresolvedReference{
		Definition:  w.definition,
		Permission:  w.permission,
		Name:        name,
		SubjectType: true,
	})
}

// checkLocal reports name unless the definition under walk declares it.
func (w *refWalker) checkLocal(name string) {
	if name == "" {
		return
	}
	if _, ok := w.local[name]; !ok {
		w.report(name)
	}
}

// checkSubjectTypes resolves every allowed subject type on a direct relation.
//
// `relation member: user | onepassword_group#member` asks for two things the
// text has to make good on: a definition named onepassword_group, and a
// `member` on it. SpiceDB's WriteSchema resolves both and rejects the whole
// schema when either is missing; compiler.Compile resolves neither, which is
// why a scaffold naming an undeclared type sat green in every in-process test
// and failed at the first live write.
//
// A wildcard (`user:*`) names its object type and no relation, so only the
// type is checked. The subject relation `...` is SpiceDB's marker for "the
// object itself, not a subject set" and names nothing to resolve.
func (w *refWalker) checkSubjectTypes(rel *core.Relation) {
	ti := rel.GetTypeInformation()
	if ti == nil {
		return
	}
	for _, allowed := range ti.GetAllowedDirectRelations() {
		ns := allowed.GetNamespace()
		if ns == "" {
			continue
		}
		names, declared := w.declared[ns]
		if !declared {
			w.reportSubjectType(ns)
			continue
		}
		subjectRel := allowed.GetRelation()
		if subjectRel == "" || subjectRel == ellipsisRelation {
			continue
		}
		if _, ok := names[subjectRel]; !ok {
			w.reportSubjectType(ns + "#" + subjectRel)
		}
	}
}

// ellipsisRelation is SpiceDB's marker on an AllowedRelation meaning "the
// object itself" — what `relation member: user` compiles to, as opposed to the
// subject set `user#something`.
const ellipsisRelation = "..."

// checkArrow resolves an arrow. The tupleset half is a local name; the
// computed half resolves on the types that local relation accepts.
func (w *refWalker) checkArrow(tupleset, computed string) {
	w.checkLocal(tupleset)
	if computed == "" {
		return
	}
	// Every type the left relation accepts is a place the right half could
	// resolve. Report only when it resolves on none of the ones we can see.
	var sawKnownType bool
	for _, target := range w.subjectTypes[tupleset] {
		names, ok := w.declared[target]
		if !ok {
			// The target type is not declared in this source — a fragment may
			// contribute it elsewhere, or it may itself be a bug, but either
			// way its member set is unknown here.
			continue
		}
		sawKnownType = true
		if _, ok := names[computed]; ok {
			return
		}
	}
	if sawKnownType {
		w.report(computed)
	}
}

func (w *refWalker) walkRewrite(rewrite *core.UsersetRewrite) {
	if rewrite == nil {
		return
	}
	// Union, intersection and exclusion are three distinct oneof arms carrying
	// the same SetOperation shape. Walking only the union arm is the silent
	// failure this function exists to avoid, so take whichever is set.
	for _, op := range []*core.SetOperation{
		rewrite.GetUnion(),
		rewrite.GetIntersection(),
		rewrite.GetExclusion(),
	} {
		if op == nil {
			continue
		}
		for _, child := range op.GetChild() {
			w.walkChild(child)
		}
	}
}

func (w *refWalker) walkChild(child *core.SetOperation_Child) {
	if child == nil {
		return
	}
	switch {
	case child.GetComputedUserset() != nil:
		w.checkLocal(child.GetComputedUserset().GetRelation())
	case child.GetTupleToUserset() != nil:
		ttu := child.GetTupleToUserset()
		w.checkArrow(ttu.GetTupleset().GetRelation(), ttu.GetComputedUserset().GetRelation())
	case child.GetFunctionedTupleToUserset() != nil:
		// `rel.any(perm)` / `rel.all(perm)`. Same two halves as the plain
		// arrow, a different proto message; the scaffold uses `.all()`, so a
		// walker that skips this arm reports nothing for real input.
		fttu := child.GetFunctionedTupleToUserset()
		w.checkArrow(fttu.GetTupleset().GetRelation(), fttu.GetComputedUserset().GetRelation())
	case child.GetUsersetRewrite() != nil:
		// A parenthesized sub-expression.
		w.walkRewrite(child.GetUsersetRewrite())
	}
	// The remaining arms — _this, _nil and _self — name nothing to resolve.
}

// allowedNamespaces returns the object types a direct relation accepts. A
// permission (rewrite, no type information) accepts none, which is why an
// arrow over a permission reports no right-hand finding rather than a wrong
// one. Wildcards and subject relations both still name their object type.
func allowedNamespaces(rel *core.Relation) []string {
	ti := rel.GetTypeInformation()
	if ti == nil {
		return nil
	}
	allowed := ti.GetAllowedDirectRelations()
	out := make([]string, 0, len(allowed))
	for _, a := range allowed {
		if ns := a.GetNamespace(); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
