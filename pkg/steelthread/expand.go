package steelthread

import (
	"context"
	"fmt"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// CaptureSource identifies a capture's SpiceDB reads — NewSpiceDBExpander
// below is the one call it makes. It never writes; the RelWriter it is
// handed is used only for this read (ExpandPermissionTree, which relsource
// never gates — see writer.go). Claims empty: a reader has nothing to own.
// Qualified (not plain Source) because this package owns more than one
// concept.
//
// Declared exactly once here; every wiring site (`oap session capture`, the
// e2e round-trip test) references this var rather than retyping the name,
// so a claim can never drift from what this reader actually presents.
var CaptureSource = relsource.Source{Name: "sessioncapture"}

func init() {
	relsource.Register(CaptureSource)
}

// ellipsisRelation is how SpiceDB spells "the subject itself, no relation" when
// it reports a relation explicitly. A subject carrying it is a DIRECT subject
// and must render with no "#relation" suffix — rendering "user:alice#..."
// would make it unequal to the "user:alice" relwrites_audit records, and
// DeriveSeed's subtraction is an exact triple match.
const ellipsisRelation = "..."

// PermissionExpander is the one SpiceDB call NewSpiceDBExpander makes.
//
// An interface rather than *authzed.Client so the flattening — and above all
// the SUBJECT RENDERING, which has to agree byte for byte with
// relwritesaudit.Tuple.Subject — is testable against a canned tree with no
// container. *authzed.Client satisfies it as generated.
type PermissionExpander interface {
	ExpandPermissionTree(
		ctx context.Context,
		in *v1.ExpandPermissionTreeRequest,
		opts ...grpc.CallOption,
	) (*v1.ExpandPermissionTreeResponse, error)
}

// NewSpiceDBExpander returns the Expander DeriveSeed needs, backed by SpiceDB's
// ExpandPermissionTree.
//
// FullyConsistent, deliberately. A capture reads state the session itself just
// wrote; anything weaker can miss a tuple that exists, and a tuple missing from
// the seed is a replay that denies where the real run allowed — reported at
// replay time, pointing at the wrong layer.
//
// # The rendering contract
//
// Every Tuple this produces is compared for EXACT equality against
// relwritesaudit.Tuple in DeriveSeed's subtraction, so the two renderings are
// one contract, not two conventions that happen to agree today:
//
//   - Resource is "<type>:<id>", the same shape relwritesaudit.Tuple.Resource
//     documents.
//   - Subject is "<type>:<id>", plus "#<relation>" when — and only when — the
//     subject is a subject SET. SpiceDB reports a direct subject either with an
//     empty OptionalRelation or with the ellipsis; both render suffix-free.
//
// Diverge on the suffix and the subtraction silently misses: a tuple the
// SESSION wrote gets seeded into the fixture, the replayed run finds the grant
// already present, and the bundle passes with the relationship-writing code
// broken. Nothing downstream can notice, which is why
// TestSpiceDBExpander_RendersSubjectsExactlyAsRelwritesAuditRecordsThem pins
// the two representations against each other rather than against a literal.
func NewSpiceDBExpander(api PermissionExpander) Expander {
	return func(ctx context.Context, resourceType, resourceID, permission string) ([]Tuple, error) {
		resp, err := api.ExpandPermissionTree(ctx, &v1.ExpandPermissionTreeRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
			Resource:    &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Permission:  permission,
		})
		if err != nil {
			return nil, fmt.Errorf("steelthread: expand %s:%s#%s: %w", resourceType, resourceID, permission, err)
		}
		var out []Tuple
		if err := flattenExpansion(resp.GetTreeRoot(), &out); err != nil {
			return nil, fmt.Errorf("steelthread: expand %s:%s#%s: %w", resourceType, resourceID, permission, err)
		}
		return out, nil
	}
}

// flattenExpansion walks the permission tree and appends one Tuple per leaf
// subject.
//
// A malformed node is an ERROR, never a skipped row. A dropped leaf is a tuple
// missing from the seed, and a seed missing a tuple produces a replay that
// denies where the run allowed — a divergence reported minutes later, against
// the replay rather than against the capture that caused it. A nil node is not
// malformed: SpiceDB returns no tree at all for a permission that expands to
// nothing, which is a legitimate (and separately reported) answer.
func flattenExpansion(node *v1.PermissionRelationshipTree, out *[]Tuple) error {
	if node == nil {
		return nil
	}
	switch t := node.GetTreeType().(type) {
	case *v1.PermissionRelationshipTree_Intermediate:
		for _, child := range t.Intermediate.GetChildren() {
			if err := flattenExpansion(child, out); err != nil {
				return err
			}
		}
		return nil

	case *v1.PermissionRelationshipTree_Leaf:
		obj := node.GetExpandedObject()
		if obj.GetObjectType() == "" || obj.GetObjectId() == "" {
			return fmt.Errorf("expansion leaf has no expanded object (type=%q id=%q)",
				obj.GetObjectType(), obj.GetObjectId())
		}
		relation := node.GetExpandedRelation()
		if relation == "" {
			return fmt.Errorf("expansion leaf on %s:%s has no expanded relation",
				obj.GetObjectType(), obj.GetObjectId())
		}
		resource := obj.GetObjectType() + ":" + obj.GetObjectId()
		for _, s := range t.Leaf.GetSubjects() {
			subject, err := renderSubject(s)
			if err != nil {
				return fmt.Errorf("expansion leaf %s#%s: %w", resource, relation, err)
			}
			*out = append(*out, Tuple{Resource: resource, Relation: relation, Subject: subject})
		}
		return nil

	default:
		// A tree node that is neither, which the proto's oneof says cannot
		// happen — but an unrecognized node holding subjects would drop them
		// silently, and this package's whole job is to not do that.
		return fmt.Errorf("expansion node on %s is neither a leaf nor an intermediate",
			node.GetExpandedObject().GetObjectType())
	}
}

// renderSubject formats one SpiceDB subject the way relwritesaudit.Tuple
// records it. See NewSpiceDBExpander's "rendering contract" for why the
// suffix rule is load-bearing.
func renderSubject(s *v1.SubjectReference) (string, error) {
	obj := s.GetObject()
	if obj.GetObjectType() == "" || obj.GetObjectId() == "" {
		return "", fmt.Errorf("subject has no object (type=%q id=%q)", obj.GetObjectType(), obj.GetObjectId())
	}
	out := obj.GetObjectType() + ":" + obj.GetObjectId()
	if rel := s.GetOptionalRelation(); rel != "" && rel != ellipsisRelation {
		out += "#" + rel
	}
	return out, nil
}
