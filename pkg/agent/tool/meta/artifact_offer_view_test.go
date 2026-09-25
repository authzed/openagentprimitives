package meta_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/preview/markup"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/css"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/svg"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func newOfferViewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s), "corev1.AddToScheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spiceboxv1alpha1.AddToScheme")
	return s
}

// readyOnCreateUnique mirrors readyOnCreate but assigns a fresh UID per Create
// so multiple preview CRs in one test get distinct revision IDs (revisionIDFor
// hashes the CR UID). A real cluster mints unique UIDs; the fake client does not.
func readyOnCreateUnique() interceptor.Funcs {
	var n atomic.Int64
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if ar, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				ar.UID = types.UID(fmt.Sprintf("uid-preview-%d", n.Add(1)))
				ar.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				ar.Status.OutputRef = "mem://o"
				ar.Status.OutputMIME = "text/html"
				ar.Status.OutputSize = 10
				ar.Status.OutputFilename = "out.html"
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// finalizeSourceCR records a Ready source revision for kind on head and returns
// the source revision ID so a test can assert the preview keys off it.
func finalizeSourceCR(t *testing.T, svc *artifacts.Service, scope memory.Scope, head, kind, name string, uid types.UID) string {
	t.Helper()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid, Labels: map[string]string{artifacts.LabelArtifactID: head}},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: kind},
		Status:     spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputRef: "mem://" + name, OutputMIME: "text/css"},
	}
	rev, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err, "finalize source revision")
	return rev.RevisionID
}

// eventuallyPreviewChild polls GetPreviewChild until ok=true or the deadline,
// returning the render name. Used to observe the async generation goroutine.
func eventuallyPreviewChild(t *testing.T, svc *artifacts.Service, scope memory.Scope, sourceRevID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		name, ok, err := svc.GetPreviewChild(memory.WithSystemApproval(context.Background(), "test"), scope, sourceRevID)
		require.NoError(t, err, "GetPreviewChild poll")
		if ok {
			return name
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("preview child for %q never appeared", sourceRevID)
	return ""
}

func TestArtifactOfferView_PublishesOffer(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: "ar-1", Namespace: "default", UID: types.UID("u1"), Labels: map[string]string{artifacts.LabelArtifactID: head}},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status:     spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html"},
	}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err)

	var published [][]byte
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		NATSSubjectPrefix: "ap.session.default.sess1",
	})
	args, _ := json.Marshal(map[string]any{"artifact_id": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)
	require.Len(t, published, 1)
	assert.Contains(t, string(published[0]), "live_view_offer")
	assert.Contains(t, string(published[0]), head)
	assert.True(t, res.Trusted, "artifact_offer_view is a framework meta tool and must opt out of content-guard inspection")
}

func TestArtifactOfferView_RejectsUnknownArtifact(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		NATSPublish:       func(ctx context.Context, subject string, payload []byte) error { return nil },
		NATSSubjectPrefix: "ap.session.default.sess1",
	})
	args, _ := json.Marshal(map[string]any{"artifact_id": "artifact-nope"})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	assert.True(t, res.IsError)
}

func TestArtifactOfferView_BundledOnly_GeneratesPreviewChild(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	sourceRevID := finalizeSourceCR(t, svc, scope, head, "css", "ar-src", types.UID("u-src"))

	c := fake.NewClientBuilder().WithScheme(newOfferViewScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-preview")).Build()

	var published [][]byte
	var fetched int
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"css"},
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error {
			published = append(published, payload)
			return nil
		},
		Client:       c,
		PollInterval: time.Millisecond,
		RenderFetch: func(ctx context.Context, ns, sess, render string) ([]byte, string, error) {
			fetched++
			return []byte(".panel{color:red}"), "text/css", nil
		},
		MarkupGen: markup.Fake{}.Generate,
	})

	args, _ := json.Marshal(map[string]any{"artifact_id": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)
	require.Len(t, published, 1, "the live-view offer must be published synchronously")

	// Generation is async: poll until the preview child appears.
	name := eventuallyPreviewChild(t, svc, scope, sourceRevID)
	assert.NotEmpty(t, name, "preview child render name")
	assert.Positive(t, fetched, "RenderFetch must have been called to read the source bytes")
}

func TestArtifactOfferView_BundledOnly_GeneratesPreviewForEveryRevision(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	// Two revisions on the same head: rev1 then rev2 (both bundled-only css).
	rev1 := finalizeSourceCR(t, svc, scope, head, "css", "ar-src-r1", types.UID("u-r1"))
	rev2 := finalizeSourceCR(t, svc, scope, head, "css", "ar-src-r2", types.UID("u-r2"))
	require.NotEqual(t, rev1, rev2, "the two source revisions must be distinct")

	// Each created preview CR must get a DISTINCT UID — the revision ID is
	// derived from the CR UID, so a fixed UID would alias both previews onto a
	// single revision (a fake-client artifact; a real cluster mints unique UIDs).
	c := fake.NewClientBuilder().WithScheme(newOfferViewScheme(t)).
		WithInterceptorFuncs(readyOnCreateUnique()).Build()

	var fetched int
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"css"},
		NATSPublish:  func(ctx context.Context, subject string, payload []byte) error { return nil },
		Client:       c,
		PollInterval: time.Millisecond,
		RenderFetch: func(ctx context.Context, ns, sess, render string) ([]byte, string, error) {
			fetched++
			return []byte(".panel{color:red}"), "text/css", nil
		},
		MarkupGen: markup.Fake{}.Generate,
	})

	args, _ := json.Marshal(map[string]any{"artifact_id": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)

	// Each revision must get its OWN preview child (keyed by that revision id).
	n1 := eventuallyPreviewChild(t, svc, scope, rev1)
	n2 := eventuallyPreviewChild(t, svc, scope, rev2)
	assert.NotEmpty(t, n1, "preview child for rev1")
	assert.NotEmpty(t, n2, "preview child for rev2")
	assert.NotEqual(t, n1, n2, "each revision gets a distinct preview render")
	assert.GreaterOrEqual(t, fetched, 2, "source bytes fetched once per revision")

	// Idempotency: a second offer must not double-generate for either revision.
	child1 := svc.PreviewChildID(rev1)
	child2 := svc.PreviewChildID(rev2)
	res, _ = tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "second offer: %s", res.Content)
	time.Sleep(50 * time.Millisecond)
	h1, _, err := svc.GetHead(memory.WithSystemApproval(context.Background(), "test"), scope, child1)
	require.NoError(t, err)
	assert.Equal(t, 1, h1.RevisionCount, "rev1 preview must stay at one revision (idempotent)")
	h2, _, err := svc.GetHead(memory.WithSystemApproval(context.Background(), "test"), scope, child2)
	require.NoError(t, err)
	assert.Equal(t, 1, h2.RevisionCount, "rev2 preview must stay at one revision (idempotent)")
}

func TestArtifactOfferView_BundledOnly_Idempotent(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	sourceRevID := finalizeSourceCR(t, svc, scope, head, "css", "ar-src2", types.UID("u-src2"))

	c := fake.NewClientBuilder().WithScheme(newOfferViewScheme(t)).
		WithInterceptorFuncs(readyOnCreate("uid-preview2")).Build()

	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"css"},
		NATSPublish:  func(ctx context.Context, subject string, payload []byte) error { return nil },
		Client:       c,
		PollInterval: time.Millisecond,
		RenderFetch: func(ctx context.Context, ns, sess, render string) ([]byte, string, error) {
			return []byte(".x{}"), "text/css", nil
		},
		MarkupGen: markup.Fake{}.Generate,
	})

	sctx := &tool.SessionContext{Namespace: "default", Name: "sess1"}
	args, _ := json.Marshal(map[string]any{"artifact_id": head})

	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sctx)
	require.False(t, res.IsError, "first call: %s", res.Content)
	eventuallyPreviewChild(t, svc, scope, sourceRevID)

	childHead := svc.PreviewChildID(sourceRevID)
	h1, _, err := svc.GetHead(memory.WithSystemApproval(context.Background(), "test"), scope, childHead)
	require.NoError(t, err)
	require.Equal(t, 1, h1.RevisionCount, "one preview revision after first offer")

	// Second offer for the same source revision must NOT create another preview.
	res, _ = tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, sctx)
	require.False(t, res.IsError, "second call: %s", res.Content)
	// Give any (incorrect) second goroutine a chance to run before asserting.
	time.Sleep(50 * time.Millisecond)
	h2, _, err := svc.GetHead(memory.WithSystemApproval(context.Background(), "test"), scope, childHead)
	require.NoError(t, err)
	assert.Equal(t, 1, h2.RevisionCount, "idempotent: RevisionCount must stay at 1 on re-offer")
}

func TestArtifactOfferView_StandaloneKind_NoPreviewGenerated(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	// html is Standalone: serve its own bytes, no preview child.
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: "ar-html", Namespace: "default", UID: types.UID("u-html"), Labels: map[string]string{artifacts.LabelArtifactID: head}},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status:     spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html"},
	}
	srcRev, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithScheme(newOfferViewScheme(t)).Build()
	var fetched int
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		NATSPublish:  func(ctx context.Context, subject string, payload []byte) error { return nil },
		Client:       c,
		PollInterval: time.Millisecond,
		RenderFetch: func(ctx context.Context, ns, sess, render string) ([]byte, string, error) {
			fetched++
			return nil, "", nil
		},
		MarkupGen: markup.Fake{}.Generate,
	})
	args, _ := json.Marshal(map[string]any{"artifact_id": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)

	time.Sleep(50 * time.Millisecond)
	_, ok, err := svc.GetPreviewChild(memory.WithSystemApproval(context.Background(), "test"), scope, srcRev.RevisionID)
	require.NoError(t, err)
	assert.False(t, ok, "standalone html must not generate a preview child")
	assert.Zero(t, fetched, "standalone path must not fetch source render bytes")
}

func TestArtifactOfferView_NilClient_NoGeneration(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	sourceRevID := finalizeSourceCR(t, svc, scope, head, "css", "ar-src3", types.UID("u-src3"))

	// Client/RenderFetch unset: generation is gated off even for a BundledOnly kind.
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"css"},
		NATSPublish: func(ctx context.Context, subject string, payload []byte) error { return nil },
	})
	args, _ := json.Marshal(map[string]any{"artifact_id": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)

	time.Sleep(50 * time.Millisecond)
	_, ok, err := svc.GetPreviewChild(memory.WithSystemApproval(context.Background(), "test"), scope, sourceRevID)
	require.NoError(t, err)
	assert.False(t, ok, "no Client/RenderFetch must mean no preview generation")
}
