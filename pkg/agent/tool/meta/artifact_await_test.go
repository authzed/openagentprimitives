package meta_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func newAwaitSvc() *artifacts.Service {
	return artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
}

func TestArtifactAwait_RejectsUnknownHandle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	tl := meta.NewArtifactAwait(meta.ArtifactAwaitConfig{Client: c, Artifacts: newAwaitSvc(), PollInterval: 5 * time.Millisecond})
	args, err := json.Marshal(map[string]any{"handle": "ar-unknown"})
	require.NoError(t, err, "marshal args")
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError, "unknown handle must produce IsError")
	assert.Contains(t, res.Content, "ar-unknown", "error must name the bad handle")
}

func TestArtifactAwait_ReadyCR_RecordsRevision(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	svc := newAwaitSvc()
	headID := svc.NewArtifactID()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-1", Namespace: "default", UID: types.UID("cr-uid"),
			Labels: map[string]string{artifacts.LabelArtifactID: headID},
			// Real renders (artifact_prepare) carry an owner ref to the session;
			// artifact_await now requires it, so the legitimate own-render path
			// must model it.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: "sess1", UID: types.UID("sess-uid-1"),
			}},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:          spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputMIME:     "text/html",
			OutputSize:     1234,
			OutputFilename: "report.html",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	tl := meta.NewArtifactAwait(meta.ArtifactAwaitConfig{Client: c, Artifacts: svc, PollInterval: 5 * time.Millisecond})
	args, err := json.Marshal(map[string]any{"handle": "ar-1"})
	require.NoError(t, err, "marshal args")
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1", AgentSessionUID: types.UID("sess-uid-1")})
	require.False(t, res.IsError, "ready CR must not produce IsError; got: %s", res.Content)
	assert.Contains(t, res.Content, `"status":"ready"`, "response must report status=ready")
	assert.True(t, res.Trusted, "artifact_await is a framework meta tool and must opt out of content-guard inspection")

	tree, err := svc.RevisionTree(memory.WithSystemApproval(context.Background(), "test"), memory.Scope{Kind: "session", ID: "default/sess1"}, headID)
	require.NoError(t, err)
	require.Len(t, tree, 1, "await must record one revision")
	assert.Equal(t, "ar-1", tree[0].RenderName)
}

// A render CR belonging to ANOTHER session, discovered by name (the runner Role
// grants unpinned `list` on artifactrenders and the name embeds the session, so
// a prompt-injected agent lists the namespace and reads a sibling's render
// name), must be REFUSED — not finalized into the caller's own artifact chain.
//
// artifact_await fetched by name in sess.Namespace and called FinalizeRevision
// with NO ownership check, while its sibling respond_to_user does check
// tool.OwnedBySession. So session B could pull session A's Ready render into B's
// scope, learn A's artifact metadata, and light up an artifact_offer_view of A's
// content on B's channel — a cross-session confidentiality breach.
func TestArtifactAwait_RefusesAnotherSessionsRender(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	svc := newAwaitSvc()
	headID := svc.NewArtifactID()

	// A Ready render owned by session A, in the shared namespace.
	victim := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-sessA-abc123", Namespace: "default", UID: types.UID("cr-uid-A"),
			Labels: map[string]string{artifacts.LabelArtifactID: headID},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: "sessA", UID: types.UID("sess-uid-A"),
			}},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
			OutputSize: 1234, OutputFilename: "secret.html",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(victim).WithStatusSubresource(victim).Build()
	tl := meta.NewArtifactAwait(meta.ArtifactAwaitConfig{Client: c, Artifacts: svc, PollInterval: 5 * time.Millisecond})

	args, err := json.Marshal(map[string]any{"handle": "ar-sessA-abc123"})
	require.NoError(t, err)

	// Session B, a different session in the same namespace, awaits A's render.
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args,
		&tool.SessionContext{Namespace: "default", Name: "sessB", AgentSessionUID: types.UID("sess-uid-B")})

	require.True(t, res.IsError, "awaiting another session's render must be refused: %s", res.Content)
	assert.NotContains(t, res.Content, "secret.html", "and none of the victim's metadata may leak into the result")

	// And nothing may have been finalized into B's scope: the artifact head was
	// never created there, so the lookup finds nothing (empty tree or not-found).
	tree, terr := svc.RevisionTree(memory.WithSystemApproval(context.Background(), "test"),
		memory.Scope{Kind: "session", ID: "default/sessB"}, headID)
	assert.True(t, terr != nil || len(tree) == 0,
		"a refused await must not write a revision into the caller's scope (tree=%v err=%v)", tree, terr)
}
