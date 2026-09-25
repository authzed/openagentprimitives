package artifactrender_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/artifactrender"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	oapfmt "github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/test/oaptest"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/html"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelassets/oap"
)

func TestReconcile_ReadyOnHappyPath(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ar-test-1",
			Namespace:  "default",
			Finalizers: []string{spiceboxv1alpha1.FinalizerArtifactRender},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{
			Kind:           "html",
			Payload:        []byte("<h1>hi</h1>"),
			TimeoutSeconds: 30,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &artifactrender.Reconciler{Client: c, Store: blobstore.NewMem(), MaxOutputAbsolute: 10 << 20}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ar-test-1", Namespace: "default"}})
	require.NoError(t, err, "Reconcile")

	got := &spiceboxv1alpha1.ArtifactRender{}
	require.NoError(t,
		c.Get(context.Background(), types.NamespacedName{Name: "ar-test-1", Namespace: "default"}, got),
		"Get ArtifactRender")
	assert.Equal(t, spiceboxv1alpha1.ArtifactRenderPhaseReady, got.Status.Phase,
		"phase; status=%+v", got.Status)
	assert.Equal(t, "text/html", got.Status.OutputMIME, "OutputMIME")
	assert.NotEmpty(t, got.Status.OutputRef, "OutputRef should be set after render")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ArtifactRenderConditionReady)
	require.NotNil(t, cond, "Ready condition should be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Ready status")
}

// The operator renders the workshop draft's kind like any other: a valid
// bundle reaches Ready carrying the bundle MIME and the forced extension, so
// respond_to_user's attach and the download route have a Ready render to
// serve.
func TestReconcile_OapKind_ReadyWithBundleMIME(t *testing.T) {
	b, err := oapfmt.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	raw, err := oapfmt.Pack(b)
	require.NoError(t, err)

	scheme := testfixtures.NewScheme(t)
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: "ar-draft-1", Namespace: "default", Finalizers: []string{spiceboxv1alpha1.FinalizerArtifactRender}},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: "oap", Filename: "demo-agent", Payload: raw, TimeoutSeconds: 30},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &artifactrender.Reconciler{Client: c, Store: blobstore.NewMem(), MaxOutputAbsolute: 10 << 20}
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ar-draft-1", Namespace: "default"}})
	require.NoError(t, err)

	got := &spiceboxv1alpha1.ArtifactRender{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "ar-draft-1", Namespace: "default"}, got))
	assert.Equal(t, spiceboxv1alpha1.ArtifactRenderPhaseReady, got.Status.Phase, "status=%+v", got.Status)
	assert.Equal(t, oapfmt.ArtifactType, got.Status.OutputMIME)
	assert.Equal(t, "demo-agent"+oapfmt.DefaultExtension, got.Status.OutputFilename)
	assert.Equal(t, int64(len(raw)), got.Status.OutputSize)
}

// TestReconcile_FailureReasons covers the failure paths where Reconcile
// stamps a terminal Failed phase with a specific FailureReason. Each case
// builds a fake client with one ArtifactRender, reconciles, and asserts
// the phase + FailureReason.
func TestReconcile_FailureReasons(t *testing.T) {
	cases := []struct {
		name            string
		kind            string
		payload         []byte
		wantPhase       spiceboxv1alpha1.ArtifactRenderPhase
		wantReason      string
		wantMsgContains string
	}{
		{
			name:       "unregistered renderer kind → Failed/RendererUnknown",
			kind:       "no-such-renderer",
			payload:    []byte("anything"),
			wantPhase:  spiceboxv1alpha1.ArtifactRenderPhaseFailed,
			wantReason: spiceboxv1alpha1.ReasonArtifactRenderRendererUnknown,
		},
		{
			name:       "payload larger than MaxOutputAbsolute → Failed/PayloadTooLarge",
			kind:       "html",
			payload:    bigBytes(2 << 20),
			wantReason: spiceboxv1alpha1.ReasonArtifactRenderPayloadTooLarge,
		},
		{
			name:            "truncated mid-tag html payload → Failed/MalformedInput",
			kind:            "html",
			payload:         []byte(`<!DOCTYPE html><html><body><img src="data:image/png;base64,iVBORw0K`),
			wantPhase:       spiceboxv1alpha1.ArtifactRenderPhaseFailed,
			wantReason:      spiceboxv1alpha1.ReasonArtifactRenderMalformedInput,
			wantMsgContains: "cut off",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testfixtures.NewScheme(t)
			cr := &spiceboxv1alpha1.ArtifactRender{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "ar-fail",
					Namespace:  "default",
					Finalizers: []string{spiceboxv1alpha1.FinalizerArtifactRender},
				},
				Spec: spiceboxv1alpha1.ArtifactRenderSpec{
					Kind:           tc.kind,
					Payload:        tc.payload,
					TimeoutSeconds: 30,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
			r := &artifactrender.Reconciler{Client: c, Store: blobstore.NewMem(), MaxOutputAbsolute: 10 << 20}
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ar-fail", Namespace: "default"}})

			got := &spiceboxv1alpha1.ArtifactRender{}
			require.NoError(t,
				c.Get(context.Background(), types.NamespacedName{Name: "ar-fail", Namespace: "default"}, got),
				"Get ArtifactRender")
			if tc.wantPhase != "" {
				assert.Equal(t, tc.wantPhase, got.Status.Phase, "phase")
			}
			assert.Equal(t, tc.wantReason, got.Status.FailureReason, "FailureReason")
			if tc.wantMsgContains != "" {
				assert.Contains(t, got.Status.FailureMessage, tc.wantMsgContains, "FailureMessage")
			}
		})
	}
}

func TestReconcile_FinalizerDropsArtifactstoreBytes(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	store := blobstore.NewMem()
	now := metav1.Now()
	seededRef := seedStore(store, "rendered bytes")
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ar-test-4",
			Namespace:         "default",
			Finalizers:        []string{spiceboxv1alpha1.FinalizerArtifactRender},
			DeletionTimestamp: &now,
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{
			Kind:    "html",
			Payload: []byte("<h1>hi</h1>"),
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:     spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef: string(seededRef),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).WithStatusSubresource(cr).Build()
	r := &artifactrender.Reconciler{Client: c, Store: store, MaxOutputAbsolute: 10 << 20}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ar-test-4", Namespace: "default"}})
	require.NoError(t, err, "Reconcile finalize")

	exists, _ := store.Exists(context.Background(), seededRef)
	assert.False(t, exists, "artifactstore bytes should be deleted by finalizer")
	_ = artifactstore.Ref("") // keep import
}

func bigBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

func seedStore(s *blobstore.Store, content string) artifactstore.Ref {
	ref, _ := s.Put(context.Background(), "test", strings.NewReader(content))
	return ref
}
