package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func TestResolve_FormsAndTree(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	rev1, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-1", headID, "", "v1", "", types.UID("u1")))
	require.NoError(t, err)
	rev2, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-2", headID, rev1.RevisionID, "v2", "published", types.UID("u2")))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, handle, wantRender string
	}{
		{"specific CR name", "ar-1", "ar-1"},
		{"revision id", rev1.RevisionID, "ar-1"},
		{"artifact newest", headID, "ar-2"},
		{"artifact by tag", headID + "#published", "ar-2"},
		// A #tag on an already revision-specific handle is redundant and must
		// be stripped (not passed into an id/name lookup that would then fail).
		// This is what the artifact-ref sanitizer advertises for all forms.
		{"render name with redundant #tag stripped", "ar-1#published", "ar-1"},
		{"revision id with redundant #tag stripped", rev1.RevisionID + "#published", "ar-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ResolveToRender(ctx, scope, tc.handle)
			require.NoError(t, err, "resolve %q", tc.handle)
			assert.Equal(t, tc.wantRender, got)
		})
	}

	_, unknownTagErr := svc.ResolveToRender(ctx, scope, headID+"#nope")
	assert.Error(t, unknownTagErr, "unknown tag must error")
	assert.True(t, errors.Is(unknownTagErr, artifacts.ErrNotFound), "unknown tag must wrap ErrNotFound")

	gotHead, parent, kind, err := svc.ResolveRevisionTarget(ctx, scope, rev1.RevisionID)
	require.NoError(t, err)
	assert.Equal(t, headID, gotHead)
	assert.Equal(t, rev1.RevisionID, parent)
	assert.Equal(t, "html", kind)

	tree, err := svc.RevisionTree(ctx, scope, headID)
	require.NoError(t, err)
	assert.Len(t, tree, 2)
	assert.Equal(t, rev2.RevisionID, tree[1].RevisionID)
	assert.Equal(t, rev1.RevisionID, tree[1].ParentID)

	arts, err := svc.ListArtifacts(ctx, scope)
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Equal(t, headID, arts[0].ArtifactID)
	assert.Equal(t, 2, arts[0].RevisionCount)
}

// TestFinalizeRevision_ResolveWidgetCSP_RoundTrips proves the producer→
// consumer contract: a widget revision finalized with Spec.CSP set (as
// pkg/agent/runner/loop.go's persistWidget does after parsing `_meta.ui.csp`)
// must round-trip through ResolveWidgetCSP for every handle form
// ResolveToRender accepts, and a revision with no declared CSP must resolve
// to nil (the safe restrictive-default fallback), never an error.
func TestFinalizeRevision_ResolveWidgetCSP_RoundTrips(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	cr := renderedCR("ar-widget-csp", headID, "", "widget", "", types.UID("uid-widget-csp"))
	cr.Spec.CSP = &spiceboxv1alpha1.WidgetCSP{
		ConnectDomains: []string{"https://api.example.test"},
	}
	rev, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize revision with a declared CSP")

	for _, tc := range []struct{ name, handle string }{
		{"specific CR name", "ar-widget-csp"},
		{"revision id", rev.RevisionID},
		{"artifact newest", headID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ResolveWidgetCSP(ctx, scope, tc.handle)
			require.NoError(t, err, "resolve %q", tc.handle)
			require.NotNil(t, got, "declared CSP must round-trip, not be dropped")
			assert.Equal(t, []string{"https://api.example.test"}, got.ConnectDomains)
		})
	}

	// A revision finalized with no CSP declared resolves to nil, not an error.
	noCSPHeadID := svc.NewArtifactID()
	_, err = svc.FinalizeRevision(ctx, scope, renderedCR("ar-no-csp", noCSPHeadID, "", "no csp", "", types.UID("uid-no-csp")))
	require.NoError(t, err)
	got, err := svc.ResolveWidgetCSP(ctx, scope, noCSPHeadID)
	require.NoError(t, err, "absent CSP must not be an error")
	assert.Nil(t, got, "absent CSP must resolve to nil (the safe restrictive-default fallback)")
}

// TestListArtifacts_ExcludesInternalHeads verifies that Internal=true artifact
// heads are hidden from ListArtifacts while normal heads remain visible.
func TestListArtifacts_ExcludesInternalHeads(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Normal artifact
	normalHeadID := svc.NewArtifactID()
	cr := renderedCR("ar-normal", normalHeadID, "", "normal artifact", "", types.UID("uid-normal"))
	cr.Annotations[artifacts.AnnoArtifactName] = "visible"
	_, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize normal artifact")

	// Internal preview-child artifact
	srcRevID := "artrev-fakesrc"
	childHeadID := svc.PreviewChildID(srcRevID)
	internalCR := renderedCR("ar-internal", childHeadID, "", "preview", "", types.UID("uid-internal"))
	internalCR.Annotations[artifacts.AnnoInternal] = "true"
	_, err = svc.FinalizeRevision(ctx, scope, internalCR)
	require.NoError(t, err, "finalize internal artifact")

	arts, err := svc.ListArtifacts(ctx, scope)
	require.NoError(t, err)
	require.Len(t, arts, 1, "ListArtifacts must exclude Internal=true heads")
	assert.Equal(t, normalHeadID, arts[0].ArtifactID)
	assert.Equal(t, "visible", arts[0].Name)
}

// TestResolveToRender_NotFound_WrapsErrNotFound verifies that each not-found
// handle form returns an error wrapping ErrNotFound (not a generic error),
// so callers can map it to 404 rather than 500.
func TestResolveToRender_NotFound_WrapsErrNotFound(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	// Finalize one revision so we have a known revision ID to test the
	// artifact-prefix / tag-missing branch on an existing artifact.
	rev1, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-nf1", headID, "", "v1", "", types.UID("u-nf1")))
	require.NoError(t, err)

	cases := []struct {
		name   string
		handle string
	}{
		{
			// artifact-… prefix, artifact ID does not exist in this scope
			name:   "artifact-prefix: artifact not in scope → ErrNotFound",
			handle: svc.NewArtifactID(),
		},
		{
			// artifact-…#tag prefix, artifact exists but tag is absent
			name:   "artifact-prefix: tag absent on existing artifact → ErrNotFound",
			handle: headID + "#nope",
		},
		{
			// artrev-… prefix, revision ID does not exist
			name:   "artrev-prefix: revision not in scope → ErrNotFound",
			handle: rev1.RevisionID[:len(rev1.RevisionID)-4] + "dead", // mangled ID
		},
		{
			// default (ar-… CR name) branch: no revision linked to this render name
			name:   "cr-name: no revision linked to render name → ErrNotFound",
			handle: "ar-does-not-exist",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ResolveToRender(ctx, scope, tc.handle)
			require.Error(t, err, "missing handle must error")
			assert.True(t, errors.Is(err, artifacts.ErrNotFound),
				"error must wrap ErrNotFound so callers can return 404; got: %v", err)
		})
	}
}

// TestListArtifacts_TieOnCreatedAtIsBrokenByID pins the TOTAL order of
// ListArtifacts.
//
// CreatedAt is formatted to second granularity, so artifacts made in the same
// second carry equal keys, and sort.Slice is not stable: with only CreatedAt to
// compare, equal keys came back in an arbitrary order that varied between runs.
// The model reads this list, so two identical reads returning two different
// sequences is a production defect, not a test artifact — it was found by the
// meta-tool determinism guard firing on artifact_history under load.
//
// Asserting the ID order rather than merely "stable across calls" is what makes
// this fail on a non-total comparison: a stable sort would keep insertion order
// and pass while the defect remained for any other caller's input order.
func TestListArtifacts_TieOnCreatedAtIsBrokenByID(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Three artifacts finalized back to back, so they share a CreatedAt second.
	ids := make([]string, 0, 3)
	for i, nm := range []string{"gamma", "alpha", "beta"} {
		headID := svc.NewArtifactID()
		cr := renderedCR("ar-"+nm, headID, "", nm+" artifact", "",
			types.UID(fmt.Sprintf("uid-%s-%d", nm, i)))
		cr.Annotations[artifacts.AnnoArtifactName] = nm
		_, err := svc.FinalizeRevision(ctx, scope, cr)
		require.NoError(t, err, "finalize %s", nm)
		ids = append(ids, headID)
	}

	arts, err := svc.ListArtifacts(ctx, scope)
	require.NoError(t, err)
	require.Len(t, arts, 3)

	got := make([]string, 0, len(arts))
	for _, a := range arts {
		got = append(got, a.ArtifactID)
	}
	want := slices.Clone(got)
	slices.Sort(want)
	assert.Equal(t, want, got,
		"artifacts sharing a CreatedAt second must be ordered by ArtifactID; an "+
			"arbitrary order here hands the model different bytes for the same read")

	// And the same call twice must agree, which is the property the determinism
	// guard observed failing.
	again, err := svc.ListArtifacts(ctx, scope)
	require.NoError(t, err)
	regot := make([]string, 0, len(again))
	for _, a := range again {
		regot = append(regot, a.ArtifactID)
	}
	assert.Equal(t, got, regot, "two identical reads must return the same sequence")
	assert.Len(t, ids, 3)
}
