package manifests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectStorageClass_PVC(t *testing.T) {
	in, err := Postgres()
	require.NoError(t, err)
	// Postgres()[0] is the PVC.
	out, err := InjectStorageClass(in[0], "ap-stateful-hyperdisk")
	require.NoError(t, err)
	docs, err := Split(out)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	got, found, err := unstructuredString(docs[0].Object, "spec", "storageClassName")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "ap-stateful-hyperdisk", got)
}

func TestInjectStorageClass_StatefulSet(t *testing.T) {
	in, err := Neo4j()
	require.NoError(t, err)
	// Neo4j()[1] is the StatefulSet.
	out, err := InjectStorageClass(in[1], "ap-stateful-hyperdisk")
	require.NoError(t, err)
	docs, err := Split(out)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	tmpls, found, err := unstructuredSlice(docs[0].Object, "spec", "volumeClaimTemplates")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, tmpls, 1)
	tmpl := tmpls[0].(map[string]any)
	got, found, err := unstructuredString(tmpl, "spec", "storageClassName")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "ap-stateful-hyperdisk", got)
}

func TestInjectStorageClass_EmptyClass_NoChange(t *testing.T) {
	in, err := Postgres()
	require.NoError(t, err)
	out, err := InjectStorageClass(in[0], "")
	require.NoError(t, err)
	assert.Equal(t, in[0], out, "empty class returns input unchanged")
}

func TestInjectStorageClass_NonStatefulDoc_NoChange(t *testing.T) {
	in, err := Postgres()
	require.NoError(t, err)
	// Postgres()[1] is the Service — not a PVC/STS, must pass through untouched.
	out, err := InjectStorageClass(in[1], "ap-stateful-hyperdisk")
	require.NoError(t, err)
	assert.Equal(t, in[1], out)
}

// test-only thin wrappers around unstructured helpers (avoid importing the
// unstructured package into the test).
func unstructuredString(obj map[string]any, fields ...string) (string, bool, error) {
	return nestedString(obj, fields...)
}
func unstructuredSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	return nestedSlice(obj, fields...)
}
