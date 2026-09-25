package schema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// EmitSpicedbSchema concatenates per-MCPServer schema fragments into one block
// of SpiceDB definitions. Identical resource declarations dedupe; mismatched
// declarations of the same resource name return an error. RawZed fields are
// appended verbatim after all structured Resources are emitted.
//
// It does NOT emit `definition user {}` — that lives in the canonical scaffold
// (pkg/authz/spicedb/schema), which the composer concatenates BEFORE this output, so
// emitting it here would double-declare user. MCPServer fragments still MUST
// NOT declare user (the guard below rejects them); the standalone-validation
// callers only inspect the returned error, not the text, so omitting user is
// safe for them.
func EmitSpicedbSchema(fragments []*spiceboxv1alpha1.SpiceDBSchemaFragment) (string, error) {
	structured, err := emitStructuredResources(fragments)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(structured)

	// Append all RawZed fragments in order.
	for _, f := range fragments {
		if f == nil || f.RawZed == "" {
			continue
		}
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(f.RawZed)
		if !strings.HasSuffix(f.RawZed, "\n") {
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String()) + "\n", nil
}

// emitStructuredResources is EmitSpicedbSchema's structured-only half: the
// merge/dedupe over every fragment's Resources[], rendered as SpiceDB
// definitions, with no RawZed appended.
//
// It exists as its own function so composeFragmentSet (composer.go) can run
// the SAME merge over the whole fragment set and emit its output as one
// named fragment, rather than per-fragment — see composeFragmentSet's doc
// comment for why per-fragment emission would turn today's silent dedupe of
// byte-identical structured resources into a hard duplicate-definition
// collision. EmitSpicedbSchema calls this too, so the merge logic exists in
// exactly one place.
func emitStructuredResources(fragments []*spiceboxv1alpha1.SpiceDBSchemaFragment) (string, error) {
	merged := map[string]spiceboxv1alpha1.SpiceDBResource{}
	for _, f := range fragments {
		if f == nil {
			continue
		}
		for _, r := range f.Resources {
			if r.Name == "user" {
				return "", fmt.Errorf("MCPServer cannot redeclare implicit resource %q", r.Name)
			}
			existing, present := merged[r.Name]
			if present && !reflect.DeepEqual(existing, r) {
				return "", fmt.Errorf("conflicting definitions for resource %q across schema fragments", r.Name)
			}
			merged[r.Name] = r
		}
	}

	var b strings.Builder

	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		r := merged[n]
		fmt.Fprintf(&b, "definition %s {\n", r.Name)
		for _, rel := range r.Relations {
			if rel.Wildcard {
				fmt.Fprintf(&b, "\trelation %s: %s:*\n", rel.Name, rel.SubjectType)
			} else {
				fmt.Fprintf(&b, "\trelation %s: %s\n", rel.Name, rel.SubjectType)
			}
		}
		for _, p := range r.Permissions {
			fmt.Fprintf(&b, "\tpermission %s = %s\n", p.Name, p.Expr)
		}
		b.WriteString("}\n\n")
	}

	return b.String(), nil
}
