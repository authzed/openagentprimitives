package relwrites

import (
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// BlocksFromSpec converts the toolspec library's resolved writesRelationships
// declarations (pkg/tools/toolspec/spec.RelationshipWriteSpec — what
// v1alpha1.SpiceboxToolspecSpec.ToSpec() produces, and what a SandboxTool's
// *spec.Spec carries) into the Blocks Run evaluates.
//
// This is the ONE field-for-field translation from that type to Block.
// Before it existed, pkg/agent/tool/sandbox.SandboxTool's
// evaluateWritesRelationships carried its own inline copy of this same
// four-plus-three-field struct-literal mapping. A field added to
// RelationshipWriteSpec and forgotten at a hand-copied call site compiles
// clean and defaults to the zero value there — for RequireSlotBound
// specifically, that zero value is "unmarked", which un-gates exactly what
// the field exists to protect, silently, with no error and nothing to name
// it. Sharing the mapping here turns a per-call-site hand copy into a single
// place to fix, and a single place a test can pin.
func BlocksFromSpec(writes []spec.RelationshipWriteSpec) []Block {
	blocks := make([]Block, 0, len(writes))
	for _, wr := range writes {
		blocks = append(blocks, Block{
			When:             wr.When,
			ForEach:          wr.ForEach,
			Exclusive:        wr.Exclusive,
			RequireSlotBound: wr.RequireSlotBound,
			Tuple: Tuple{
				Resource: wr.Tuple.Resource,
				Relation: wr.Tuple.Relation,
				Subject:  wr.Tuple.Subject,
			},
		})
	}
	return blocks
}
