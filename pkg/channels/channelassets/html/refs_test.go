package html

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewriteRefs_ResolvedHandle_RewritesImgAndLink verifies img[src] and
// link[href] — the only two contexts the sanitizer lets an artifact: value
// survive on — are rewritten to resolve's replacement, and the resolved
// handle is reported back.
func TestRewriteRefs_ResolvedHandle_RewritesImgAndLink(t *testing.T) {
	in := `<html><head><link rel="stylesheet" href="artifact:ar-x"></head><body><img src="artifact:ar-x"></body></html>`
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		require.Equal(t, "ar-x", handle)
		return "/assets/x.css", true
	})

	body := string(out)
	assert.NotContains(t, body, "artifact:", "no artifact: scheme value should survive the rewrite")
	assert.Contains(t, body, `href="/assets/x.css"`, "link[href] must be rewritten to the resolved replacement")
	assert.Contains(t, body, `src="/assets/x.css"`, "img[src] must be rewritten to the resolved replacement")
	assert.Equal(t, []string{"ar-x"}, resolved, "the single distinct handle must be reported once")
}

// TestRewriteRefs_UnresolvedHandle_DropsAttrKeepsPrimary proves keep=false
// drops the attribute (not the element, not the rest of the document) and
// reports no resolved handles.
func TestRewriteRefs_UnresolvedHandle_DropsAttrKeepsPrimary(t *testing.T) {
	in := `<html><head></head><body><p>keep me</p><img id="pic" src="artifact:ar-other-session"></body></html>`
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		return "", false
	})

	body := string(out)
	assert.NotContains(t, body, "artifact:", "the unresolved artifact: value must not survive")
	assert.NotContains(t, body, `src=`, "the src attribute itself must be dropped, not just its value")
	assert.Contains(t, body, `id="pic"`, "the element must survive — only the one attribute is dropped")
	assert.Contains(t, body, "keep me", "the rest of the document must still be served")
	assert.Empty(t, resolved, "nothing resolved")
}

// TestRewriteRefs_NonArtifactSrc_Untouched verifies ordinary (non-artifact:)
// src/href values — http(s), data: — pass through unchanged and resolve is
// never called for them.
func TestRewriteRefs_NonArtifactSrc_Untouched(t *testing.T) {
	in := `<img src="data:image/png;base64,AAAA">`
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		t.Fatalf("resolve must not be called for a non-artifact: value, got handle %q", handle)
		return "", false
	})
	assert.Contains(t, string(out), `src="data:image/png;base64,AAAA"`)
	assert.Empty(t, resolved)
}

// TestRewriteRefs_TagSuffix_PassedThroughVerbatim proves a "#tag" suffix on
// the handle is neither stripped nor otherwise interpreted by this kind —
// resolve receives it exactly as written. Tag handling (when needed) is
// entirely the caller's concern.
func TestRewriteRefs_TagSuffix_PassedThroughVerbatim(t *testing.T) {
	in := `<img src="artifact:artrev-abc123#draft">`
	var gotHandle string
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		gotHandle = handle
		return "/assets/draft.png", true
	})
	assert.Equal(t, "artrev-abc123#draft", gotHandle, "the #tag suffix must reach resolve unmodified")
	assert.Contains(t, string(out), `src="/assets/draft.png"`)
	assert.Equal(t, []string{"artrev-abc123#draft"}, resolved)
}

// TestRewriteRefs_DedupesRepeatedHandle proves resolve is called at most
// once per distinct handle even when the same handle appears on multiple
// elements — required for callers whose resolve has side effects (the
// bundler fetches + records secondary bytes on first resolution).
func TestRewriteRefs_DedupesRepeatedHandle(t *testing.T) {
	in := `<html><body><img src="artifact:ar-x"><img src="artifact:ar-x"></body></html>`
	calls := 0
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		calls++
		return "/assets/x.png", true
	})
	assert.Equal(t, 1, calls, "resolve must be called once per distinct handle, not once per reference")
	assert.Equal(t, []string{"ar-x"}, resolved, "one distinct handle, reported once")
	assert.Equal(t, 2, strings.Count(string(out), `src="/assets/x.png"`), "both img tags must be rewritten to the same replacement")
}

// TestRewriteRefs_MixedKeepAndDrop_ReportsOnlyKept proves the resolved list
// contains only handles that were actually kept, in first-seen order, even
// when other handles on the same document were dropped.
func TestRewriteRefs_MixedKeepAndDrop_ReportsOnlyKept(t *testing.T) {
	in := `<html><body><img src="artifact:ar-keep"><img src="artifact:ar-drop"></body></html>`
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		if handle == "ar-keep" {
			return "/assets/keep.png", true
		}
		return "", false
	})
	body := string(out)
	assert.Contains(t, body, `src="/assets/keep.png"`)
	assert.NotContains(t, body, "artifact:")
	assert.Equal(t, []string{"ar-keep"}, resolved)
}

// TestRewriteRefs_NoRefs_ReturnsNilResolved proves a document with no
// artifact: refs at all returns an unmodified-shape document and a nil
// resolved list (the caller's "nothing to bundle" signal).
func TestRewriteRefs_NoRefs_ReturnsNilResolved(t *testing.T) {
	in := `<html><body><p>no refs here</p></body></html>`
	out, resolved := New().RewriteRefs([]byte(in), func(handle string) (string, bool) {
		t.Fatalf("resolve must not be called, got handle %q", handle)
		return "", false
	})
	assert.Equal(t, in, string(out), "no-refs input must be returned byte-unchanged (no re-serialize)")
	assert.Nil(t, resolved)
}
