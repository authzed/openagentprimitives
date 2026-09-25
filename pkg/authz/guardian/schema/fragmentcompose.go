package schema

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing/fstest"

	spicedbschema "github.com/authzed/spicedb/pkg/schema"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/generator"
	"github.com/authzed/spicedb/pkg/schemadsl/input"

	// compiler.Compile logs a trace line per definition through zerolog's
	// process-global logger; the same blank import validate_fragment.go carries,
	// for the same reason — anything linking this package stays silent.
	_ "github.com/authzed/openagentprimitives/pkg/platform/deplogs"
)

// NamedFragment is one contributor's schema text plus the name it is known by.
//
// Name becomes the fragment's FILENAME inside the synthetic filesystem the
// compiler reads, which is the whole reason this type exists rather than a
// bare []string. A collision between two fragments comes back as a parse error
// naming the losing file, so the caller can condition the right CR instead of
// reporting that "the schema" failed. Callers therefore owe Name something an
// operator can act on — an MCPServer's namespace/name, a channel kind's key —
// never an index.
//
// Name must be a single path segment: it is joined with ".zed" and imported by
// a relative path, and schemadsl's importer refuses anything that
// escapes its own directory.
type NamedFragment struct {
	Name string
	ZED  string
}

// ComposeFragments assembles frags into one schema and VALIDATES the result
// against spicedb's own type system before returning it.
//
// The two halves matter separately. Assembly is a compile over a synthetic
// filesystem: each fragment is a file, a generated root imports them in sorted
// order, and schemadsl merges them. Validation is (most of) the
// check SpiceDB itself runs at WriteSchema — without it, composition is only
// a PARSE, and a permission naming a relation or subject type nothing
// declares reaches the server and is refused there, once, cluster-wide, after
// every fragment has already merged.
//
// "Most of": spicedb's type system resolves an arrow's LEFT half (the
// tupleset relation) but never its RIGHT half — `permission view =
// rel->no_such_permission` type-checks clean here and simply resolves to
// nothing at Check time (see TestValidateComposedSchema_AcceptsAnUnresolved
// ArrowRightHalf). A cross-fragment dangling arrow is exactly the shape this
// asymmetry can hide, which is why callers that need that class caught —
// internal/cmd/operator's compile-time conformance test — run
// UnresolvedReferences too, rather than treating this validation as the only
// net.
//
// Output is a pure function of the input SET: fragments are sorted by name, so
// the caller's slice order — which comes from registries and map ranges that
// promise nothing — cannot move the bytes. That is what keeps the write-if-
// changed comparison in RunAll from rewriting the schema every reconcile.
func ComposeFragments(frags []NamedFragment) (string, error) {
	if len(frags) == 0 {
		return "", fmt.Errorf("compose fragments: no fragments supplied")
	}

	fsys := fstest.MapFS{}
	names := make([]string, 0, len(frags))
	for _, f := range frags {
		if f.Name == "" {
			return "", fmt.Errorf("compose fragments: fragment with empty name")
		}
		if strings.Contains(f.Name, "/") || !fs.ValidPath(f.Name) {
			return "", fmt.Errorf("compose fragments: fragment name %q must be a single path segment", f.Name)
		}
		file := f.Name + ".zed"
		if _, dup := fsys[file]; dup {
			return "", fmt.Errorf("compose fragments: duplicate fragment name %q", f.Name)
		}
		fsys[file] = &fstest.MapFile{Data: []byte(f.ZED)}
		names = append(names, f.Name)
	}
	sort.Strings(names)

	var root strings.Builder
	root.WriteString("use import\n\n")
	for _, n := range names {
		fmt.Fprintf(&root, "import \"./%s.zed\"\n", n)
	}

	compiled, err := compiler.Compile(
		compiler.InputSchema{
			Source:       input.Source("composed-fragments"),
			SchemaString: root.String(),
		},
		compiler.AllowUnprefixedObjectType(),
		compiler.SourceFS(fsys),
	)
	if err != nil {
		return "", fmt.Errorf("compose fragments: %w", err)
	}

	if err := validateCompiled(compiled); err != nil {
		return "", err
	}

	out, _, err := generator.GenerateSchema(context.Background(), compiled.OrderedDefinitions)
	if err != nil {
		return "", fmt.Errorf("compose fragments: render composed schema: %w", err)
	}
	return out, nil
}

// ValidateComposedSchema runs the type-system check SpiceDB runs at WriteSchema
// over already-composed schema TEXT.
//
// It exists for the stages ComposeFragments cannot cover: grant and slot
// composition rewrite the composed text afterwards, and a rewrite can produce
// something that no longer validates. Those stages have no contributor to
// isolate, so the only honest response is to refuse the write.
func ValidateComposedSchema(src string) error {
	compiled, err := compiler.Compile(
		compiler.InputSchema{
			Source:       input.Source("composed-schema"),
			SchemaString: src,
		},
		compiler.AllowUnprefixedObjectType(),
	)
	if err != nil {
		return fmt.Errorf("validate composed schema: parse: %w", err)
	}
	return validateCompiled(compiled)
}

// validateCompiled is the shared half: build the type system over exactly the
// definitions and caveats just compiled — no datastore, no cluster — and ask it
// to validate each definition, which is what rejects a duplicate
// relation/permission name and an unresolvable reference alike.
func validateCompiled(compiled *compiler.CompiledSchema) error {
	ts := spicedbschema.NewTypeSystem(
		spicedbschema.ResolverForPredefinedDefinitions(spicedbschema.PredefinedElements{
			Definitions: compiled.ObjectDefinitions,
			Caveats:     compiled.CaveatDefinitions,
		}))
	for _, def := range compiled.ObjectDefinitions {
		if _, err := ts.GetValidatedDefinition(context.Background(), def.GetName()); err != nil {
			return fmt.Errorf("validate composed schema: %w", err)
		}
	}
	return nil
}
