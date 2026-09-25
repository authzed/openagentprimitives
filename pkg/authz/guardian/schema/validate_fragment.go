package schema

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"

	// compiler.Compile logs a trace line per definition through zerolog's
	// process-global logger. Blank-imported (rather than left to each main) so
	// that anything linking this package is silent: binaries, `go test`
	// binaries, and the in-process e2e harness alike. Deferring compilation to
	// first use (see ReservedDefinitionNames below) keeps a main() call early
	// enough for binaries; this import is what covers everything without one.
	_ "github.com/authzed/openagentprimitives/pkg/platform/deplogs"
)

var reservedNames = sync.OnceValue(mustReservedDefinitionNames)

// ReservedDefinitionNames returns the set of definition names owned by the
// code-owned base scaffold (pkg/authz/spicedb/schema/schema.zed) — user, group,
// agentsession, memory_entry, artifact, infoleakage_grant, platform as
// of this writing. It is derived by parsing authzschema.Schema on first
// call (rather than at package init) so it can never drift out of sync
// with the scaffold: a future definition added to schema.zed is automatically
// reserved without a matching edit here. pkg/authz/spicedb/schema/schema_test.go pins
// the expected name set against this derivation so an accidental scaffold
// rename is caught by a normal test failure rather than silently changing
// what fragments are allowed to declare.
//
// Computed on first call rather than at package init: an eager initializer
// compiled the scaffold before main() could configure logging, which leaked
// SpiceDB's zerolog trace output into every oap command, and made every
// binary linking this package pay schema compilation at startup whether or
// not it validates a fragment. Compilation is now deferred until first use.
func ReservedDefinitionNames() map[string]struct{} {
	return reservedNames()
}

func mustReservedDefinitionNames() map[string]struct{} {
	parsed, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("base-scaffold"),
		SchemaString: authzschema.Schema,
	}, compiler.AllowUnprefixedObjectType())
	if err != nil {
		// The base scaffold is code-owned and covered by its own tests;
		// a compile failure here means the embed itself is broken, which
		// would already break every other guardian schema operation. Fail
		// loudly on first use rather than silently returning an empty
		// reserved set (which would let fragments redeclare `agentsession`
		// undetected).
		panic(fmt.Sprintf("guardian/schema: base scaffold failed to compile: %v", err))
	}
	out := make(map[string]struct{}, len(parsed.ObjectDefinitions))
	for _, def := range parsed.ObjectDefinitions {
		out[def.GetName()] = struct{}{}
	}
	return out
}

// definitionNameRe matches a `definition <name>` declaration inside raw
// SpiceDB schema DSL text. It is a lightweight lexical scan (not a full
// parse) sufficient to catch a RawZed fragment attempting to redeclare a
// reserved scaffold definition before it ever reaches EmitSpicedbSchema
// or the compiler.
var definitionNameRe = regexp.MustCompile(`(?m)^\s*definition\s+([A-Za-z_][A-Za-z0-9_]*)\b`)

func rawZedDefinitionNames(rawZed string) []string {
	if rawZed == "" {
		return nil
	}
	matches := definitionNameRe.FindAllStringSubmatch(rawZed, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// ValidateFragment checks a single MCPServer's SpiceDBSchemaFragment in
// isolation — independent of any other MCPServer's fragment — so the
// guardian controller can partition MCPServers into "contributes a good
// fragment" and "contributes a bad fragment" BEFORE composing the
// cluster-wide schema. A bad fragment must never reach RunAll: RunAll's
// compose→WriteSchema is all-or-nothing, so a single invalid fragment
// fed into it would fail the write for every AgentSessionGrants in the
// cluster (the cross-tenant DoS this function exists to prevent).
//
// A nil fragment is valid (nothing to check). Checks, in order:
//
//  1. Reserved-name collision: the fragment's Resources[].Name or any
//     `definition <name>` in its RawZed must not collide with a
//     reserved scaffold definition (ReservedDefinitionNames). This
//     includes the historical "cannot redeclare user" check as a
//     special case of the general reserved-name rule.
//  2. EmitSpicedbSchema on the fragment ALONE — catches internal
//     conflicts (a fragment declaring the same resource name twice with
//     different bodies) and any structural issue EmitSpicedbSchema
//     itself rejects.
//  3. composeAllParseOnly on the fragment ALONE over the base scaffold,
//     which parses the result with the same SpiceDB schema compiler
//     RunAll's own assembly ultimately bottoms out in — catching RawZed
//     syntax errors. It is deliberately the PARSE-ONLY compose, not
//     RunAll's validating one: this fragment is checked alone, so a
//     reference onto a type only a SIBLING fragment or the channel-kind
//     baseline declares would look dangling here and is not — see
//     composeAllParseOnly's doc. Step 4 below (UnresolvedReferences) is
//     what still catches a SELF-CONTAINED dangling reference despite that
//     leniency.
func ValidateFragment(frag *spiceboxv1alpha1.SpiceDBSchemaFragment) error {
	if frag == nil {
		return nil
	}

	reserved := ReservedDefinitionNames()
	for _, r := range frag.Resources {
		if _, ok := reserved[r.Name]; ok {
			return fmt.Errorf("fragment declares resource %q, which collides with a reserved scaffold definition", r.Name)
		}
	}
	for _, name := range rawZedDefinitionNames(frag.RawZed) {
		if _, ok := reserved[name]; ok {
			return fmt.Errorf("fragment's rawZed redefines %q, which collides with a reserved scaffold definition", name)
		}
	}

	if _, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag}); err != nil {
		return fmt.Errorf("emit fragment: %w", err)
	}

	composed, err := composeAllParseOnly([]IdentifiedFragment{{Fragment: frag}}, nil, nil)
	if err != nil {
		return fmt.Errorf("compose fragment over base scaffold: %w", err)
	}

	// A dangling permission reference — a name a permission expression asks for
	// that its own definition does not declare — is refused HERE, fail-closed,
	// rather than left for SpiceDB's WriteSchema to reject once, cluster-wide,
	// after every fragment has been merged (which freezes every
	// AgentSessionGrants). compiler.Compile above is only a PARSE; it never
	// resolves the references, which is why this is a separate step.
	//
	// Run against the fragment composed over the base scaffold, so a reference
	// into a scaffold type (agentsession#interact) resolves. The resolver is
	// deliberately biased against false positives: an arrow ALL of whose target
	// types are absent here (provided by another fragment or a channel kind) is
	// SKIPPED, not flagged — so this rejects a fragment's SELF-CONTAINED dangling
	// name and leaves a genuinely cross-fragment reference alone. The one
	// residual is an arrow over a relation accepting BOTH a present scaffold type
	// and an absent one, where the name resolves only on the absent side: that
	// would be reported. No production fragment has that shape (the built-in
	// toolkits are all `expr: owner`), so it is latent, not a live outage. A dangling reference
	// onto a BASELINE or already-accepted type is caught one layer out:
	// PartitionCompatibleFragments runs UnresolvedReferences on each trial
	// composed against the channel-kind baseline, where those types are
	// present. RunAll (composer.go)'s ValidateComposedSchema gate remains as a
	// backstop for the failure classes spicedb's type system DOES catch —
	// an undeclared relation/permission expression name, and a subject type
	// (or subject-set relation) absent from the composed set — refusing the
	// write in-process, before WriteSchema is ever called, rather than merely
	// logging the finding. It is not a backstop for an arrow's RIGHT half: the
	// type system never resolves a computed permission name against an
	// arrow's target type (see TestValidateComposedSchema_AcceptsAnUnresolved
	// ArrowRightHalf in fragmentcompose_test.go), so a cross-fragment dangling
	// arrow specifically is caught only by an UnresolvedReferences call
	// somewhere in the chain — here, or in PartitionCompatibleFragments.
	unresolved, err := UnresolvedReferences(composed)
	if err != nil {
		return fmt.Errorf("resolve fragment references: %w", err)
	}
	for _, u := range unresolved {
		// A SUBJECT-TYPE finding is dropped here for the same reason the arrow's
		// right half is skipped above, and it is the same partial view that
		// causes both: `composed` is THIS fragment over the scaffold, with no
		// channel-kind baseline and no sibling fragment. A tenant fragment
		// typing `relation member: slack_channel#member` names a definition
		// the slack channel kind declares (pkg/channels/channelkinds/slack/
		// schema.zed) — present at the real compose, absent here — so
		// refusing on it would reject a valid fragment and cost that CR its
		// availability, which is the false positive this whole resolver is
		// biased against.
		//
		// Nothing is lost by dropping it: an undeclared subject type that
		// resolves nowhere in the REAL schema is caught one layer out, by
		// PartitionCompatibleFragments over the channel-kind baseline
		// (partition.go), where those types are present, and reported again by
		// RunAll's backstop (composer.go). What must never be dropped is the
		// scaffold's own case, and the scaffold composes no fragments at all —
		// see UnresolvedReferences' own doc, and the guard in
		// pkg/channels/channelkinds/onepassword.
		if u.SubjectType {
			continue
		}
		return fmt.Errorf("fragment has a dangling permission reference: %s", u.String())
	}

	return nil
}
