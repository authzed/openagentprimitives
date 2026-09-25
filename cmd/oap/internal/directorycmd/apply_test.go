package directorycmd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
)

// getRelationshipSource fetches the applied RelationshipSource by name, so a
// test can inspect exactly what Apply wrote.
func getRelationshipSource(t *testing.T, dyn dynamic.Interface, ns, name string) *unstructured.Unstructured {
	t.Helper()
	got, err := dyn.Resource(relationshipSourceGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// nestedString reads a nested string field, failing the test if the read
// itself errors (a type mismatch, not merely "absent").
func nestedString(t *testing.T, obj *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	v, _, err := unstructured.NestedString(obj.Object, fields...)
	require.NoError(t, err)
	return v
}

// nestedJSON reads a nested field and re-marshals it to JSON text, so a
// caller can assert.JSONEq against it regardless of how the dynamic client
// happened to order the decoded map's keys.
func nestedJSON(t *testing.T, obj *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "expected %v to be set", fields)
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// applyAsReplace works around a limitation of the dynamic fake client: its
// built-in apply-patch reactor merges via strategicpatch.StrategicMergePatch,
// which looks up patch directives from Go struct tags — and finds none on
// *unstructured.Unstructured ("unable to find api field in struct
// Unstructured for the json field ..."), so a second Apply against an
// object that already exists fails in the fake even though it works fine
// against a real apiserver. This test registers a reactor that instead
// replaces the stored object outright with the applied one, which is a
// faithful stand-in here: every apply in this package uses one field
// manager (fieldManager) with Force=true, so there is never another
// manager's fields for a real merge to preserve. A missing object still
// surfaces NotFound from the Update call below, which is exactly what
// drives kube.Apply's own create-fallback — so a first apply against an
// empty client still exercises the Create path unchanged.
func applyAsReplace(t *testing.T, dyn *dynfake.FakeDynamicClient, gvr schema.GroupVersionResource) {
	t.Helper()
	dyn.PrependReactor("patch", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchAction)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), &obj.Object); err != nil {
			return true, nil, err
		}
		obj.SetName(pa.GetName())
		obj.SetNamespace(pa.GetNamespace())
		if err := dyn.Tracker().Update(gvr, obj, pa.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
}

// The applied object must carry exactly what was chosen, and spec.config
// must survive byte-for-byte — the kind parses it, not this command.
func TestApply_WritesTheChosenSpec(t *testing.T) {
	dyn := fakeDyn(t)
	require.NoError(t, directorycmd.Apply(ctx, dyn, "default", directorycmd.Selections{
		Kind: "github", Identity: "ghid", Credential: "pat", Name: "gh",
		Config: json.RawMessage(`{"orgs":["acme"]}`),
	}))

	got := getRelationshipSource(t, dyn, "default", "gh")
	assert.Equal(t, "github", nestedString(t, got, "spec", "kind"))
	assert.Equal(t, "ghid", nestedString(t, got, "spec", "auth", "agentIdentity"))
	assert.JSONEq(t, `{"orgs":["acme"]}`, nestedJSON(t, got, "spec", "config"))
}

// A byte-identical re-apply must be a genuine no-op: SSA field ownership
// churning on every run re-reconciles every watcher of the object.
func TestApply_IsIdempotent(t *testing.T) {
	dyn := fakeDyn(t)
	applyAsReplace(t, dyn, relationshipSourceGVR)
	sel := directorycmd.Selections{Kind: "github", Identity: "ghid", Credential: "pat", Name: "gh"}
	require.NoError(t, directorycmd.Apply(ctx, dyn, "default", sel))
	first := getRelationshipSource(t, dyn, "default", "gh")
	require.NoError(t, directorycmd.Apply(ctx, dyn, "default", sel))
	second := getRelationshipSource(t, dyn, "default", "gh")

	assert.Equal(t, first.Object["spec"], second.Object["spec"],
		"the applied spec is a pure function of its inputs")
}

// The command applies spec and metadata; status belongs to the controller.
func TestApply_NeverWritesStatus(t *testing.T) {
	dyn := fakeDyn(t)
	require.NoError(t, directorycmd.Apply(ctx, dyn, "default", directorycmd.Selections{
		Kind: "slack", Identity: "slid", Credential: "bot", Name: "sl",
	}))
	got := getRelationshipSource(t, dyn, "default", "sl")
	_, found, err := unstructured.NestedMap(got.Object, "status")
	require.NoError(t, err)
	assert.False(t, found, "a client must not apply a status stanza")
}

// A kind that needs no endpoint must not get an empty one: an empty string
// is a value, and writing it claims the field under this field manager.
func TestApply_OmitsAnEmptyEndpoint(t *testing.T) {
	dyn := fakeDyn(t)
	require.NoError(t, directorycmd.Apply(ctx, dyn, "default", directorycmd.Selections{
		Kind: "slack", Identity: "slid", Credential: "bot", Name: "sl",
	}))
	got := getRelationshipSource(t, dyn, "default", "sl")
	_, found, err := unstructured.NestedString(got.Object, "spec", "baseURL")
	require.NoError(t, err)
	assert.False(t, found)
}
