// Package handoff grades a request to hand a datum to a delegated child.
//
// The grading is MECHANICAL, and that is the design rather than an
// implementation convenience: the alternative is an LLM deciding its own data
// access, which is not a decision a model driven by the very content in
// question can be trusted to make.
package handoff

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

// Grade is what to do with a request to bind a tag into a child's data slot.
type Grade int

const (
	// GradeRoute sends the request to the resource owners for a decision.
	//
	// THE ZERO VALUE, deliberately. A Grade returned alongside an error, or
	// left unset by a future branch, means route — which asks a human. If
	// auto-grant were zero, every path that forgot to set it would silently
	// disclose, and nothing would look wrong.
	GradeRoute Grade = iota

	// GradeAutoGrant binds the tag without asking, because doing so discloses
	// nothing: everyone who could already see the child's context was entitled
	// to see the datum, and the datum is not untrusted.
	GradeAutoGrant
)

// Deps are the three facts a grade needs. Each is a lookup rather than a value
// so the caller decides consistency and caching.
type Deps struct {
	// ChildAudience returns A(child): the subjects who can read the child's
	// transcript, which is `agentsession#read_transcript`. It is the set that
	// would newly see the datum if it were bound.
	ChildAudience func(ctx context.Context, child authz.SessionRef) ([]string, error)

	// TagReaders returns R(T): the tag's fully-resolved audience, `pt_tag#reader`.
	// Resolved rather than direct — the intersection across the derivation tree
	// happens in SpiceDB.
	TagReaders func(ctx context.Context, tagID string) (readers []string, err error)

	// TagCarriesUntrusted answers the INTEGRITY axis, `pt_tag#carries_untrusted`.
	// A separate question about the same request, and the one a
	// sensitivity-only gate never asks.
	TagCarriesUntrusted func(ctx context.Context, tagID string) (bool, error)
}

// GradeRequest decides whether binding tagID into child's slot discloses
// anything, and returns a human-readable reason for the decision.
//
// Two INDEPENDENT axes, and both must pass to auto-grant:
//
//   - Confidentiality. If `A(child) ⊆ R(T)` the grant discloses nothing —
//     everyone who could see the child's context was already entitled to see
//     T. This is provenance.Unauthorized, shared with both egress checks
//     rather than reimplemented.
//   - Integrity. A tag carrying untrusted content is a candidate injection
//     vector regardless of who may read it.
//
// The fourth quadrant is why both are required, and it is the one an intuitive
// gate gets backwards. A PUBLIC WEB PAGE is readable by everyone, so the
// confidentiality leg is spotlessly clean — and it is precisely the injection
// vector. A gate that asks only "is this sensitive?" auto-grants it.
//
// Every unanswerable lookup routes. A fact we could not establish is not a
// fact in the request's favour, and routing asks a human where granting asks
// nobody.
func GradeRequest(ctx context.Context, d Deps, child authz.SessionRef, tagID string) (Grade, string, error) {
	if d.ChildAudience == nil || d.TagReaders == nil || d.TagCarriesUntrusted == nil {
		return GradeRoute, "", fmt.Errorf("handoff: grader is not fully wired")
	}

	// Integrity first: it is one boolean against two set lookups, and a tag
	// that routes on integrity routes whatever the audience turns out to be.
	untrusted, err := d.TagCarriesUntrusted(ctx, tagID)
	if err != nil {
		return GradeRoute, "", fmt.Errorf("handoff: integrity of tag %q: %w", tagID, err)
	}
	if untrusted {
		return GradeRoute, fmt.Sprintf(
			"tag %s carries untrusted content, so it routes for a decision even if its audience already covers this child — "+
				"a source readable by everyone is exactly the shape an injection arrives in", tagID), nil
	}

	audience, err := d.ChildAudience(ctx, child)
	if err != nil {
		return GradeRoute, "", fmt.Errorf("handoff: audience of child %s/%s: %w", child.Namespace, child.Name, err)
	}
	readers, err := d.TagReaders(ctx, tagID)
	if err != nil {
		return GradeRoute, "", fmt.Errorf("handoff: readers of tag %q: %w", tagID, err)
	}

	if newly := provenance.Unauthorized(audience, readers); len(newly) > 0 {
		return GradeRoute, fmt.Sprintf(
			"binding tag %s would disclose it to %s, who are not among its authorized readers",
			tagID, strings.Join(newly, ", ")), nil
	}
	return GradeAutoGrant, fmt.Sprintf(
		"everyone who can read this child could already read tag %s, and it carries nothing untrusted", tagID), nil
}
