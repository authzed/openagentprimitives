// Package schema's base.go composes the COMPILE-TIME half of the schema: the
// scaffold, plus fragments that ship inside the binary rather than arriving
// from a cluster.
//
// Two different things are called "toolkits" and they land on opposite sides of
// this line. The embedded built-ins (toolkits/*.yaml, pulled in by the //go:embed
// in toolkits/toolkits.go) belong HERE. SpiceboxToolkit CRs are installed, so
// they are tenant input and go through ValidateFragment and the partition with
// MCPServer, SidecarToolbox and SpiceDBBootstrap. The controller appends both
// sets a few lines apart, which is what makes them easy to conflate.
//
// The compile-time set is deliberately NOT partitioned. Partitioning exists to
// isolate a bad contributor to its own CR's status, and these have no CR: the
// only honest response to one that does not parse is a loud error, which is why
// a conformance test in internal/cmd/operator (the binary whose compile-time
// fragment set actually matters — it is what blank-imports every channel kind
// and links the embedded toolkits) asserts the real set composes.
package schema

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ComposeBase returns the scaffold with every compile-time fragment merged in.
//
// Each fragment is run through ValidateFragment first, so redeclaring a
// scaffold definition is refused here exactly as it is for a runtime fragment.
// The error names the offending fragment's definitions, because with no CR to
// carry a condition the error text is the only diagnostic there is.
//
// ValidateFragment parses each fragment ALONE against the scaffold, so two
// compile-time fragments that each pass individually can still collide once
// merged — EmitSpicedbSchema's conflict detection covers only structured
// Resources[], not RawZed text, so two RawZed blocks that both declare the
// same definition with different bodies would otherwise be concatenated
// without complaint. The merge is therefore assembled and validated here too,
// through the SAME composeFragmentSet the production compose path
// (composeAllWithSkipped, via ComposeAll/RunAll) uses for its own
// scaffold+fragments assembly — one function, so the compile-time and
// runtime paths cannot diverge on what "composes" means.
func ComposeBase(compileTime []*spiceboxv1alpha1.SpiceDBSchemaFragment) (string, error) {
	for _, frag := range compileTime {
		if frag == nil {
			continue
		}
		if err := ValidateFragment(frag); err != nil {
			return "", fmt.Errorf("compile-time fragment is invalid (this is a build bug, not a cluster problem): %w", err)
		}
	}
	frags := make([]IdentifiedFragment, 0, len(compileTime))
	for _, frag := range compileTime {
		frags = append(frags, IdentifiedFragment{Fragment: frag})
	}
	merged, err := composeFragmentSet(frags)
	if err != nil {
		return "", fmt.Errorf("compile-time fragments do not compose into a valid schema (this is a build bug, not a cluster problem): %w", err)
	}
	return merged, nil
}
