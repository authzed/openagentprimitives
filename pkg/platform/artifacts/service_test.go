package artifacts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func newSvc() (*artifacts.Service, memory.Scope) {
	mem := memory.NewLocal(inmem.NewBackend())
	return artifacts.NewService(mem, nil), memory.Scope{Kind: "session", ID: "default/sess1"}
}

// newSvcWithMem is like newSvc but also returns the underlying memory so tests
// can query raw entries (e.g. to inspect link annotations on revisions).
func newSvcWithMem() (*artifacts.Service, memory.Memory, memory.Scope) {
	mem := memory.NewLocal(inmem.NewBackend())
	return artifacts.NewService(mem, nil), mem, memory.Scope{Kind: "session", ID: "default/sess1"}
}

func renderedCR(name, headID, parent, change, tags string, uid types.UID) *spiceboxv1alpha1.ArtifactRender {
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: uid,
			Labels:      map[string]string{artifacts.LabelArtifactID: headID},
			Annotations: map[string]string{},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:     spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef: "mem://" + name, OutputMIME: "text/html", OutputSize: 10, OutputFilename: name + ".html",
		},
	}
	if parent != "" {
		cr.Annotations[artifacts.AnnoParentRevision] = parent
	}
	cr.Annotations[artifacts.AnnoChangeDescription] = change
	if tags != "" {
		cr.Annotations[artifacts.AnnoAppliedTags] = tags
	}
	return cr
}

func TestFinalizeRevision_NewArtifactThenRevise(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	cr1 := renderedCR("ar-1", headID, "", "initial draft", "", types.UID("uid-1"))
	cr1.Annotations[artifacts.AnnoArtifactName] = "report"
	rev1, err := svc.FinalizeRevision(ctx, scope, cr1)
	require.NoError(t, err, "finalize rev1")
	assert.Equal(t, 1, rev1.Seq)
	assert.NotEmpty(t, rev1.RevisionID)

	head, ok, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	require.True(t, ok, "head must exist after first finalize")
	assert.Equal(t, "report", head.Name)
	assert.Equal(t, 1, head.RevisionCount)
	assert.Equal(t, rev1.RevisionID, head.Tags[artifacts.TagLatest], "latest must point at rev1")

	cr2 := renderedCR("ar-2", headID, rev1.RevisionID, "added chart", "draft", types.UID("uid-2"))
	rev2, err := svc.FinalizeRevision(ctx, scope, cr2)
	require.NoError(t, err, "finalize rev2")
	assert.Equal(t, 2, rev2.Seq)

	head, _, err = svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	assert.Equal(t, 2, head.RevisionCount)
	assert.Equal(t, rev2.RevisionID, head.Tags[artifacts.TagLatest], "latest moves to rev2")
	assert.Equal(t, rev2.RevisionID, head.Tags["draft"], "draft tag points at rev2")
}

func TestPreviewChildID_DeterministicAndPrefixed(t *testing.T) {
	svc, _ := newSvc()
	revID := "artrev-abc123"

	id1 := svc.PreviewChildID(revID)
	id2 := svc.PreviewChildID(revID)
	assert.Equal(t, id1, id2, "PreviewChildID must be deterministic for the same input")

	other := svc.PreviewChildID("artrev-different")
	assert.NotEqual(t, id1, other, "PreviewChildID must differ for different source revision IDs")

	assert.True(t, len(id1) > len("artifact-"), "result must have content beyond the prefix")
	assert.Equal(t, "artifact-", id1[:len("artifact-")], "result must carry the artifact- prefix")
}

func internalPreviewCR(name, headID, previewOf, previewRevOf string, uid types.UID) *spiceboxv1alpha1.ArtifactRender {
	cr := renderedCR(name, headID, "", "preview", "", uid)
	cr.Annotations[artifacts.AnnoInternal] = "true"
	if previewOf != "" {
		cr.Annotations[artifacts.AnnoPreviewOf] = previewOf
	}
	if previewRevOf != "" {
		cr.Annotations[artifacts.AnnoPreviewRevisionOf] = previewRevOf
	}
	return cr
}

func TestFinalizeRevision_InternalFlag_AndPreviewLinks(t *testing.T) {
	svc, mem, scope := newSvcWithMem()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// Source artifact
	srcHeadID := svc.NewArtifactID()
	srcRev, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-src", srcHeadID, "", "source", "", types.UID("uid-src")))
	require.NoError(t, err, "finalize source revision")

	// Preview child
	childHeadID := svc.PreviewChildID(srcRev.RevisionID)
	cr := internalPreviewCR("ar-preview", childHeadID, srcHeadID, srcRev.RevisionID, types.UID("uid-preview"))
	result, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize preview child")

	// Head must be Internal=true
	head, ok, err := svc.GetHead(ctx, scope, childHeadID)
	require.NoError(t, err)
	require.True(t, ok, "preview child head must exist after finalize")
	assert.True(t, head.Internal, "head.Internal must be true for an internal preview artifact")

	// Verify links on the revision entry via direct memory query
	qres, err := mem.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{"artifact_revision"},
		IDs:   []string{result.RevisionID},
	})
	require.NoError(t, err)
	require.Len(t, qres.Entries, 1)

	var foundPreviewOf, foundPreviewRevOf bool
	for _, l := range qres.Entries[0].Links {
		if l.Relation == "preview_of" && l.ID == srcHeadID {
			foundPreviewOf = true
		}
		if l.Relation == "preview_revision_of" && l.ID == srcRev.RevisionID {
			foundPreviewRevOf = true
		}
	}
	assert.True(t, foundPreviewOf, "revision must carry preview_of link to source head")
	assert.True(t, foundPreviewRevOf, "revision must carry preview_revision_of link to source revision")
}

func TestGetPreviewChild_BeforeAndAfterFinalize(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")

	srcHeadID := svc.NewArtifactID()
	srcRev, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-src2", srcHeadID, "", "source", "", types.UID("uid-src2")))
	require.NoError(t, err, "finalize source revision")

	// Before any preview is finalized: ok must be false
	renderName, ok, err := svc.GetPreviewChild(ctx, scope, srcRev.RevisionID)
	require.NoError(t, err, "GetPreviewChild before finalize must not error")
	assert.False(t, ok, "ok must be false before preview is finalized")
	assert.Empty(t, renderName)

	// Finalize the preview child
	childHeadID := svc.PreviewChildID(srcRev.RevisionID)
	cr := internalPreviewCR("ar-preview2", childHeadID, srcHeadID, srcRev.RevisionID, types.UID("uid-preview2"))
	_, err = svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize preview child")

	// After finalize: ok must be true and RenderName must match
	renderName, ok, err = svc.GetPreviewChild(ctx, scope, srcRev.RevisionID)
	require.NoError(t, err, "GetPreviewChild after finalize must not error")
	assert.True(t, ok, "ok must be true after preview is finalized")
	assert.Equal(t, "ar-preview2", renderName)
}

func TestGetRevision_LoadsFinalizedRevision(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	rev, err := svc.FinalizeRevision(ctx, scope, renderedCR("ar-get", headID, "", "draft", "", types.UID("uid-get")))
	require.NoError(t, err, "finalize revision")

	got, ok, err := svc.GetRevision(ctx, scope, rev.RevisionID)
	require.NoError(t, err, "GetRevision must not error")
	require.True(t, ok, "ok must be true for an existing revision")
	assert.Equal(t, "ar-get", got.RenderName, "RenderName must match the finalized CR name")
	assert.Equal(t, 1, got.Seq, "Seq must match the finalized revision")

	_, ok, err = svc.GetRevision(ctx, scope, "artrev-missing")
	require.NoError(t, err, "GetRevision for an absent ID must not error")
	assert.False(t, ok, "ok must be false for an absent revision")
}

// tornHeadMemory wraps a memory.Memory and fails the first Put of an artifact
// HEAD entry, letting the revision Put through. That is exactly what a runner
// pod killed (SIGTERM / OOM / eviction) between FinalizeRevision's two Puts
// leaves behind: a durable revision and a head that never learned about it.
type tornHeadMemory struct {
	memory.Memory
	failHeadPuts int
}

func (m *tornHeadMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if e.Kind == "artifact" && m.failHeadPuts > 0 {
		m.failHeadPuts--
		return memory.Entry{}, errors.New("simulated crash before the head write landed")
	}
	return m.Memory.Put(ctx, e)
}

// TestFinalizeRevision_TornHeadWrite_RepairedOnRefinalize pins the convergence
// contract: after a finalize that wrote the revision but not the head, the
// designed retry (artifact_prepare times out -> the agent calls artifact_await,
// which re-finalizes the SAME CR) must repair the head rather than report
// success over a half-finalized artifact.
func TestFinalizeRevision_TornHeadWrite_RepairedOnRefinalize(t *testing.T) {
	torn := &tornHeadMemory{Memory: memory.NewLocal(inmem.NewBackend()), failHeadPuts: 1}
	svc := artifacts.NewService(torn, nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	cr := renderedCR("ar-torn", headID, "", "initial draft", "draft", types.UID("uid-torn"))
	cr.Annotations[artifacts.AnnoArtifactName] = "report"

	_, err := svc.FinalizeRevision(ctx, scope, cr)
	require.Error(t, err, "the torn head Put must surface as an error, not be swallowed")

	// The revision is already durable and already listed by RevisionTree, which
	// reads the revision_of link rather than the head.
	tree, err := svc.RevisionTree(ctx, scope, headID)
	if err == nil {
		assert.Len(t, tree, 1, "the torn revision is durable and observable")
	}

	// The retry the tools actually perform: same CR, same UID, same revision ID.
	res, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "re-finalize after a torn head write must succeed")

	head, ok, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err, "GetHead after repair")
	require.True(t, ok, "re-finalize must create the head the torn write never wrote")
	assert.Equal(t, 1, head.RevisionCount, "RevisionCount must count the durable revision")
	assert.Equal(t, res.RevisionID, head.Tags[artifacts.TagLatest], "latest must point at the durable revision")
	assert.Equal(t, res.RevisionID, head.Tags["draft"], "the CR's applied tags must not be silently lost")
	assert.Contains(t, res.Tags, artifacts.TagLatest, "the returned result must report the tags now pointing at the revision")

	// And the handle resolves, which is the user-visible symptom of the bug.
	name, err := svc.ResolveToRender(ctx, scope, headID)
	require.NoError(t, err, "artifact-…#latest must resolve after the repair")
	assert.Equal(t, "ar-torn", name)
}

// TestFinalizeRevision_TornHeadWrite_StaleRevisionDoesNotStealLatest guards the
// repair's monotonicity: repairing an older torn revision AFTER a newer one has
// been finalized must not move `latest` backwards.
func TestFinalizeRevision_TornHeadWrite_StaleRevisionDoesNotStealLatest(t *testing.T) {
	torn := &tornHeadMemory{Memory: memory.NewLocal(inmem.NewBackend()), failHeadPuts: 1}
	svc := artifacts.NewService(torn, nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	// rev1 tears: its revision lands, its head write does not.
	cr1 := renderedCR("ar-old", headID, "", "first", "", types.UID("uid-old"))
	_, err := svc.FinalizeRevision(ctx, scope, cr1)
	require.Error(t, err, "first finalize must tear")

	// rev2 finalizes cleanly and takes latest.
	cr2 := renderedCR("ar-new", headID, "", "second", "", types.UID("uid-new"))
	rev2, err := svc.FinalizeRevision(ctx, scope, cr2)
	require.NoError(t, err, "second finalize must succeed")

	// Now the late repair of rev1 arrives.
	rev1, err := svc.FinalizeRevision(ctx, scope, cr1)
	require.NoError(t, err, "late repair of the torn revision must succeed")

	head, ok, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	require.True(t, ok, "head must exist")
	assert.Equal(t, rev2.RevisionID, head.Tags[artifacts.TagLatest],
		"a late repair of an older revision must not move latest backwards")
	assert.NotEqual(t, rev1.RevisionID, head.Tags[artifacts.TagLatest])
}

func TestFinalizeRevision_Idempotent(t *testing.T) {
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()
	cr := renderedCR("ar-1", headID, "", "draft", "", types.UID("uid-1"))

	r1, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err)
	r2, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err)
	assert.Equal(t, r1.RevisionID, r2.RevisionID, "same CR must finalize to the same revision")

	head, _, err := svc.GetHead(ctx, scope, headID)
	require.NoError(t, err)
	assert.Equal(t, 1, head.RevisionCount, "re-finalize must not double-bump RevisionCount")
}
