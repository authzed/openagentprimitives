package artifactview

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewriteArtifactRefs_ResolvedHandle_RewritesImgAndLink verifies that an
// `artifact:HANDLE` value on img[src] and link[href] — the only two contexts
// the html sanitizer lets it survive on — is replaced with the token-gated
// asset URL when the resolver finds the handle within (ns, sess), and that no
// `artifact:` scheme survives in the output.
func TestRewriteArtifactRefs_ResolvedHandle_RewritesImgAndLink(t *testing.T) {
	av := &fakeDeps{
		resolveAsset: func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
			require.Equal(t, "default", ns)
			require.Equal(t, "s1", sess)
			require.Equal(t, "ar-x", handle)
			return "/artifacts/a/?ct=TOK123", true, nil
		},
	}
	in := `<html><head><link rel="stylesheet" href="artifact:ar-x"></head><body><img src="artifact:ar-x"></body></html>`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err)

	body := string(out)
	assert.NotContains(t, body, "artifact:", "no artifact: scheme value should survive the rewrite")
	assert.Contains(t, body, `src="/artifacts/a/?ct=TOK123"`, "img[src] must be rewritten to the minted asset URL")
	assert.Contains(t, body, `href="/artifacts/a/?ct=TOK123"`, "link[href] must be rewritten to the minted asset URL")
}

// TestRewriteArtifactRefs_UnresolvedHandle_DropsAttrKeepsPrimary is the
// same-session security boundary test: a handle that does not resolve within
// (ns, sess) — unknown, or belonging to a different session — must have its
// attribute dropped, not fail the whole document. The rest of the primary
// (including the element itself) is still served.
func TestRewriteArtifactRefs_UnresolvedHandle_DropsAttrKeepsPrimary(t *testing.T) {
	av := &fakeDeps{
		resolveAsset: func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
			return "", false, nil // outside (ns,sess) or unknown — unresolvable
		},
	}
	in := `<html><head></head><body><p>keep me</p><img id="pic" src="artifact:ar-other-session"></body></html>`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err)

	body := string(out)
	assert.NotContains(t, body, "artifact:", "the unresolved artifact: value must not survive")
	assert.NotContains(t, body, `src=`, "the src attribute itself must be dropped, not just its value")
	assert.Contains(t, body, `id="pic"`, "the element must survive — only the one attribute is dropped")
	assert.Contains(t, body, "keep me", "the rest of the primary document must still be served")
}

// TestRewriteArtifactRefs_ResolveError_DropsAttrDoesNotFail verifies that a
// genuine backend error from ResolveAssetURL (not just "not found") degrades
// the same way as an unresolved handle — drop and continue — rather than
// failing the whole primary.
func TestRewriteArtifactRefs_ResolveError_DropsAttrDoesNotFail(t *testing.T) {
	av := &fakeDeps{
		resolveAsset: func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
			return "", false, assert.AnError
		},
	}
	in := `<img src="artifact:ar-x">`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err, "a resolver error must not fail the whole rewrite")
	assert.NotContains(t, string(out), "artifact:")
}

// TestRewriteArtifactRefs_NonArtifactSrc_Untouched verifies ordinary
// (non-artifact:) src/href values — http(s), data: — pass through unchanged;
// the rewrite only ever touches `artifact:` values.
func TestRewriteArtifactRefs_NonArtifactSrc_Untouched(t *testing.T) {
	av := &fakeDeps{}
	in := `<img src="data:image/png;base64,AAAA">`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err)
	assert.Contains(t, string(out), `src="data:image/png;base64,AAAA"`)
}

// TestRewriteArtifactRefs_KindWithoutRefRewriter_ContentUnchanged is the
// Task 7 genericity proof: this function never branches on a kind name — it
// dispatches through the channelassets registry and a RefRewriter
// type-assert (see rewrite.go). The "image" kind (blank-imported in
// helpers_test.go) is a REAL registered renderer with no reference concept
// at all, so an `artifact:` value in image-kind content must pass through
// completely untouched, and ResolveAssetURL must never even be called.
func TestRewriteArtifactRefs_KindWithoutRefRewriter_ContentUnchanged(t *testing.T) {
	resolveCalled := false
	av := &fakeDeps{
		renderKind: func(ctx context.Context, ns, sess, renderName string) (string, error) {
			return "image", nil
		},
		resolveAsset: func(ctx context.Context, ns, sess, handle string) (string, bool, error) {
			resolveCalled = true
			return "/artifacts/a/?ct=TOK123", true, nil
		},
	}
	in := `<img src="artifact:ar-x">`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err)
	assert.Equal(t, in, string(out), "content must pass through byte-for-byte unchanged")
	assert.False(t, resolveCalled, "resolve must never be invoked for a kind with no RefRewriter")
}

// TestRewriteArtifactRefs_UnregisteredKind_ContentUnchanged is the same
// no-op proof for the OTHER dispatch miss: a kind name the registry has
// never heard of at all (not merely one lacking RefRewriter).
func TestRewriteArtifactRefs_UnregisteredKind_ContentUnchanged(t *testing.T) {
	av := &fakeDeps{
		renderKind: func(ctx context.Context, ns, sess, renderName string) (string, error) {
			return "no-such-kind", nil
		},
	}
	in := `<img src="artifact:ar-x">`
	out, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(in))
	require.NoError(t, err)
	assert.Equal(t, in, string(out))
}

// TestRewriteArtifactRefs_RenderKindLookupFails_ReturnsError proves a
// render-kind resolution failure is surfaced as an error (the caller,
// contentHandler, logs it and falls back to serving content untransformed)
// rather than silently swallowed.
func TestRewriteArtifactRefs_RenderKindLookupFails_ReturnsError(t *testing.T) {
	av := &fakeDeps{
		renderKind: func(ctx context.Context, ns, sess, renderName string) (string, error) {
			return "", assert.AnError
		},
	}
	_, err := rewriteArtifactRefs(context.Background(), av, "default", "s1", "render-1", []byte(`<img src="artifact:ar-x">`))
	require.Error(t, err)
}
