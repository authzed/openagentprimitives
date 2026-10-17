package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// browserScreenshotTestScheme builds a scheme carrying the ArtifactRender CRD
// type. package runner (this file) cannot reuse another file's internal-test
// scheme helper across _test.go files of the same package that declare their
// own (see apptoolsGrantTestScheme's doc comment) — each file in this
// package's test suite mints its own, so this one does too.
func browserScreenshotTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// fakeScreenshotPutter is a minimal screenshotArtifactPutter recorder: it
// never touches real storage, just remembers the keys/bytes it was asked to
// Put and hands back a fixed Ref.
type fakeScreenshotPutter struct {
	ref   artifactstore.Ref
	err   error
	calls []struct {
		key   string
		bytes []byte
	}
}

func (f *fakeScreenshotPutter) Put(_ context.Context, key string, r io.Reader) (artifactstore.Ref, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.calls = append(f.calls, struct {
		key   string
		bytes []byte
	}{key: key, bytes: b})
	if f.err != nil {
		return "", f.err
	}
	return f.ref, nil
}

func TestStoreScreenshotArtifact_InlineVsRef(t *testing.T) {
	scheme := browserScreenshotTestScheme(t)
	sess := channelevents.SessionRef{Namespace: "default", Name: "s1"}

	t.Run("PNG under the 256 KiB cap is stored inline", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		putter := &fakeScreenshotPutter{ref: "mem://should-not-be-used"}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			SessionUID:    types.UID("sess-uid-1"),
		}
		small := bytes.Repeat([]byte{0xAB}, 1024) // < 256 KiB

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.NoError(t, err)
		require.NotNil(t, ref)

		assert.NotEmpty(t, ref.RenderName)
		assert.NotEmpty(t, ref.ArtifactID)
		assert.Equal(t, "image/png", ref.MIME)
		assert.Equal(t, "page.png", ref.Filename)
		assert.Equal(t, "current page (credential fields redacted)", ref.AltText)

		var cr spiceboxv1alpha1.ArtifactRender
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sess.Namespace, Name: ref.RenderName}, &cr),
			"the created ArtifactRender must be readable back by the returned RenderName")
		assert.Equal(t, ref.RenderName, cr.Name)
		assert.Equal(t, "image", cr.Spec.Kind)
		assert.Equal(t, small, []byte(cr.Spec.Payload), "small PNG must be inline")
		assert.Empty(t, cr.Spec.PayloadRef, "small PNG must not set PayloadRef")
		assert.Equal(t, ref.ArtifactID, cr.Labels["artifact.agentprimitives.authzed.com/id"])
		assert.Empty(t, putter.calls, "artifactstore.Put must not be called for an inline-sized PNG")
	})

	t.Run("PNG over the 256 KiB cap is stored by ref", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		putter := &fakeScreenshotPutter{ref: "mem://default/s1/screenshot.png"}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			SessionUID:    types.UID("sess-uid-1"),
		}
		big := bytes.Repeat([]byte{0xCD}, 300*1024) // > 256 KiB

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, big)
		require.NoError(t, err)
		require.NotNil(t, ref)
		assert.Equal(t, "image/png", ref.MIME)

		var cr spiceboxv1alpha1.ArtifactRender
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: sess.Namespace, Name: ref.RenderName}, &cr))
		assert.Nil(t, cr.Spec.Payload, "big PNG must not be inlined")
		assert.Equal(t, "mem://default/s1/screenshot.png", cr.Spec.PayloadRef, "big PNG must be stored by ref")

		require.Len(t, putter.calls, 1, "artifactstore.Put must be called exactly once for an oversized PNG")
		assert.Equal(t, big, putter.calls[0].bytes, "the exact PNG bytes must reach the artifact store")
	})

	t.Run("artifact store Put failure is returned, not swallowed", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		putErr := errors.New("boom: store unavailable")
		putter := &fakeScreenshotPutter{err: putErr}
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: putter,
			SessionUID:    types.UID("sess-uid-1"),
		}
		big := bytes.Repeat([]byte{0xEF}, 300*1024)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, big)
		require.Error(t, err)
		assert.Nil(t, ref)
		assert.ErrorIs(t, err, putErr)
	})

	t.Run("CR Create failure is returned, not swallowed", func(t *testing.T) {
		createErr := errors.New("boom: create refused")
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return createErr
				},
			}).
			Build()
		deps := screenshotStoreDeps{
			Client:     c,
			SessionUID: types.UID("sess-uid-1"),
		}
		small := bytes.Repeat([]byte{0x01}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.Error(t, err)
		assert.Nil(t, ref)
	})

	t.Run("deterministic name/id seams are honored", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		deps := screenshotStoreDeps{
			Client:        c,
			ArtifactStore: &fakeScreenshotPutter{},
			SessionUID:    types.UID("sess-uid-1"),
			NewRenderName: func(session string) string { return "ar-" + session + "-fixed" },
			NewArtifactID: func() string { return "artifact-fixed" },
		}
		small := bytes.Repeat([]byte{0x02}, 16)

		ref, err := storeScreenshotArtifact(context.Background(), deps, sess, small)
		require.NoError(t, err)
		assert.Equal(t, "ar-s1-fixed", ref.RenderName)
		assert.Equal(t, "artifact-fixed", ref.ArtifactID)
	})
}
