// Package fromcrd converts v1alpha1.ObservesBlock into observe.Block, the
// single conversion site for both CRD carriers of ObservesBlock
// (MCPServerTool and SpiceboxToolspecSpec) -- two hand-maintained copies of
// this converter is the same shape as the silent-drop bug this DSL has
// already produced (see pkg/tools/mcp/spec.ObservesSpec, whose json tags
// must match the CRD type's exactly or the round trip drops the value).
//
// This is a SEPARATE package from pkg/authz/observe, not a function or
// method there, because observe.Block is deliberately CRD-decoupled:
// pkg/authz/observe compiles and evaluates blocks and must not import
// pkg/apis/v1alpha1 to do it. Package fromcrd imports both observe and
// v1alpha1 and is imported by neither, so the CRD dependency stops at the
// controllers, which sit above both in the dependency graph and are the only
// callers that hold a CRD object to convert.
package fromcrd

import (
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/observe"
)

// FromCRD converts the CRD's ObservesBlock into observe.Block, the
// CRD-decoupled shape observe.ValidateBlock (and, later, evaluation) works
// against.
func FromCRD(b v1alpha1.ObservesBlock) observe.Block {
	subjects := make([]observe.SubjectExpr, len(b.Subjects))
	for i, s := range b.Subjects {
		subjects[i] = observe.SubjectExpr{ResourceType: s.ResourceType, ResourceID: s.ResourceID}
	}
	return observe.Block{
		When:     b.When,
		ForEach:  b.ForEach,
		Subjects: subjects,
		Facts:    b.Facts,
	}
}
