package install

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func spiceboxClass(image string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetKind("SpiceboxClass")
	u.SetName("sre-sandbox")
	_ = unstructured.SetNestedField(u.Object, image, "spec", "image")
	return u
}

func sidecarToolbox(image string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetKind("SidecarToolbox")
	u.SetName("k8s-mcp")
	_ = unstructured.SetNestedField(u.Object, image, "spec", "source", "image")
	return u
}

func imageOf(t *testing.T, u *unstructured.Unstructured, path ...string) string {
	t.Helper()
	s, _, err := unstructured.NestedString(u.Object, path...)
	require.NoError(t, err)
	return s
}

func TestRewriteImageRefs(t *testing.T) {
	sc, st := spiceboxClass("sre-sandbox:dev"), sidecarToolbox("sre-dedicated-mcp:dev")
	crs := []*unstructured.Unstructured{sc, st}

	require.NoError(t, rewriteImageRefs(crs, map[string]string{
		"sre-sandbox:dev":       "reg.example.com/sre-sandbox@sha256:aaa",
		"sre-dedicated-mcp:dev": "reg.example.com/sre-dedicated-mcp@sha256:bbb",
	}))
	assert.Equal(t, "reg.example.com/sre-sandbox@sha256:aaa", imageOf(t, sc, "spec", "image"))
	assert.Equal(t, "reg.example.com/sre-dedicated-mcp@sha256:bbb", imageOf(t, st, "spec", "source", "image"))
}

func TestRewriteImageRefsNoopOnEmptyOrUnknown(t *testing.T) {
	sc := spiceboxClass("sre-sandbox:dev")
	require.NoError(t, rewriteImageRefs([]*unstructured.Unstructured{sc}, nil))
	assert.Equal(t, "sre-sandbox:dev", imageOf(t, sc, "spec", "image"), "empty map = untouched")

	require.NoError(t, rewriteImageRefs([]*unstructured.Unstructured{sc}, map[string]string{"other:dev": "x"}))
	assert.Equal(t, "sre-sandbox:dev", imageOf(t, sc, "spec", "image"), "unmatched ref = untouched")
}
