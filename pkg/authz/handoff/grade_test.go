package handoff_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/handoff"
)

var childRef = authz.SessionRef{Namespace: "ns", Name: "child"}

// depsFor builds a grader over one tag's facts.
func depsFor(childAudience, tagReaders []string, untrusted bool) handoff.Deps {
	return handoff.Deps{
		ChildAudience: func(context.Context, authz.SessionRef) ([]string, error) {
			return childAudience, nil
		},
		TagReaders: func(context.Context, string) ([]string, error) {
			return tagReaders, nil
		},
		TagCarriesUntrusted: func(context.Context, string) (bool, error) {
			return untrusted, nil
		},
	}
}

// TestGradeIsTwoIndependentAxes walks the whole table.
//
// Confidentiality asks `A(child) ⊆ R(T)`: is everyone who could see the
// child's context already entitled to see T? Integrity asks whether T carries
// untrusted content. They are INDEPENDENT, and three of the four cells are
// obvious.
//
// The fourth is the point of the whole design. A public web page is readable
// by everyone, so the confidentiality leg is spotlessly clean — and it is
// precisely the injection vector. A gate that asks "is this sensitive?"
// auto-grants it. Both axes must pass.
func TestGradeIsTwoIndependentAxes(t *testing.T) {
	everyone := []string{"user:tim", "user:fred", "user:sam", "user:sarah"}
	childReaders := []string{"user:tim", "user:fred"}

	cases := []struct {
		name      string
		audience  []string
		readers   []string
		untrusted bool
		want      handoff.Grade
	}{
		{
			// Text a user typed into the channel: everyone who can see the
			// child could already see it, and it came from a trusted source.
			name:     "covered and trusted: auto-grant",
			audience: childReaders, readers: everyone, untrusted: false,
			want: handoff.GradeAutoGrant,
		},
		{
			// A narrow-permission resource. Someone who can read the child's
			// transcript is not entitled to the tag, so granting it discloses.
			name:     "not covered, trusted: route to owners",
			audience: everyone, readers: childReaders, untrusted: false,
			want: handoff.GradeRoute,
		},
		{
			name:     "not covered and untrusted: route (either axis alone routes it)",
			audience: everyone, readers: childReaders, untrusted: true,
			want: handoff.GradeRoute,
		},
		{
			// THE case. Readable by everyone, so `A ⊆ R` holds trivially — and
			// it is a public web page, the canonical injection vector. A
			// sensitivity-only gate waves this straight through.
			name:     "covered but UNTRUSTED: must route, never auto-grant",
			audience: childReaders, readers: everyone, untrusted: true,
			want: handoff.GradeRoute,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := handoff.GradeRequest(context.Background(),
				depsFor(tc.audience, tc.readers, tc.untrusted), childRef, "ptt-1")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTheReasonNamesWhichAxisFailed keeps a routed request explicable.
//
// An approver asked to decide on a grant needs to know whether it is being
// asked because someone would see data they should not, or because the data
// itself may be hostile. Those call for different judgments, and "routed" alone
// supports neither.
func TestTheReasonNamesWhichAxisFailed(t *testing.T) {
	_, reason, err := handoff.GradeRequest(context.Background(),
		depsFor([]string{"user:sarah"}, []string{"user:tim"}, false), childRef, "ptt-1")
	require.NoError(t, err)
	assert.Contains(t, reason, "user:sarah", "a disclosure must name who would newly see the data")

	_, reason, err = handoff.GradeRequest(context.Background(),
		depsFor([]string{"user:tim"}, []string{"user:tim"}, true), childRef, "ptt-1")
	require.NoError(t, err)
	assert.Contains(t, reason, "untrusted",
		"an integrity routing must say so: the audience is fine and that is not why it routed")
}

// TestTheZeroGradeIsRoute pins the fail-safe direction of the type itself.
//
// A Grade returned alongside an error, or left unset by a future branch, must
// mean ROUTE. If the zero value were AutoGrant, every path that forgot to set
// it would silently disclose.
func TestTheZeroGradeIsRoute(t *testing.T) {
	var g handoff.Grade
	assert.Equal(t, handoff.GradeRoute, g,
		"the zero value must be the conservative one; an unset Grade must never mean auto-grant")
}

// TestAFailedLookupNeverAutoGrants covers each axis going unanswerable.
//
// A fact we could not establish is not a fact in the request's favour. All
// three lookups must fail toward routing — which asks a human — rather than
// toward granting, which asks nobody.
func TestAFailedLookupNeverAutoGrants(t *testing.T) {
	base := func() handoff.Deps { return depsFor([]string{"user:tim"}, []string{"user:tim"}, false) }

	cases := []struct {
		name string
		mut  func(*handoff.Deps)
	}{
		{name: "child audience unresolvable", mut: func(d *handoff.Deps) {
			d.ChildAudience = func(context.Context, authz.SessionRef) ([]string, error) {
				return nil, errors.New("boom")
			}
		}},
		{name: "tag readers unresolvable", mut: func(d *handoff.Deps) {
			d.TagReaders = func(context.Context, string) ([]string, error) { return nil, errors.New("boom") }
		}},
		{name: "integrity unresolvable", mut: func(d *handoff.Deps) {
			d.TagCarriesUntrusted = func(context.Context, string) (bool, error) { return false, errors.New("boom") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base()
			tc.mut(&d)
			got, _, err := handoff.GradeRequest(context.Background(), d, childRef, "ptt-1")
			require.Error(t, err)
			assert.Equal(t, handoff.GradeRoute, got,
				"an unanswerable question must not resolve as auto-grant even alongside an error")
		})
	}
}

// TestAnEmptyChildAudienceStillGradesOnIntegrity is the trap the confidentiality
// leg alone would miss.
//
// A child nobody can read has an empty audience, which is vacuously covered:
// there is nobody to disclose to. That is a correct answer on the
// confidentiality axis and says nothing at all about whether the data is
// hostile — so an untrusted tag must still route.
func TestAnEmptyChildAudienceStillGradesOnIntegrity(t *testing.T) {
	got, _, err := handoff.GradeRequest(context.Background(),
		depsFor(nil, []string{"user:tim"}, true), childRef, "ptt-1")
	require.NoError(t, err)
	assert.Equal(t, handoff.GradeRoute, got,
		"nobody to leak to is not the same as safe to feed in")
}

// TestGradeUsesTheSharedPredicate pins that the confidentiality leg is
// provenance.Unauthorized rather than a fourth subset check.
//
// Three call sites already share it — channel egress, tool-call egress, slot
// grading. A local reimplementation would be a fourth, each looking correct
// alone and free to disagree under a later edit.
func TestGradeUsesTheSharedPredicate(t *testing.T) {
	// A duplicate in the audience must not change the answer, which a
	// hand-rolled loop counting matches would get wrong.
	got, _, err := handoff.GradeRequest(context.Background(),
		depsFor([]string{"user:tim", "user:tim"}, []string{"user:tim"}, false), childRef, "ptt-1")
	require.NoError(t, err)
	assert.Equal(t, handoff.GradeAutoGrant, got)
}
