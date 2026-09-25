package directorycmd_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

var ctx = context.Background()

// relationshipSourceGVR mirrors the one in existing.go — kept separate so
// the test fixtures do not reach into the package's unexported var.
var relationshipSourceGVR = schema.GroupVersionResource{
	Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "relationshipsources",
}

// fakeDyn builds a fake dynamic client seeded with objs, wired for
// RelationshipSource and AgentIdentity list operations — mirroring how
// cmd/oap/internal/settingscmd's tests build theirs (dynfake +
// unstructured.Unstructured fixtures). Shared across this package's test
// files; a new caller that lists a further GVR extends the map here rather
// than hand-rolling a second constructor.
func fakeDyn(t *testing.T, objs ...*unstructured.Unstructured) *dynfake.FakeDynamicClient {
	t.Helper()
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			relationshipSourceGVR: "RelationshipSourceList",
			agentIdentityGVR:      "AgentIdentityList",
		},
		runtimeObjs...)
}

// relationshipSource builds an unstructured RelationshipSource fixture named
// name, with spec.kind set to kind, per the CRD shape in
// pkg/apis/v1alpha1/relationshipsource_types.go. opts layer on the optional
// fields a given case needs.
func relationshipSource(name, kind string, opts ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	obj.SetKind("RelationshipSource")
	obj.SetName(name)
	obj.SetNamespace("default")
	_ = unstructured.SetNestedField(obj.Object, kind, "spec", "kind")
	for _, opt := range opts {
		opt(obj)
	}
	return obj
}

func withAuth(identity, credential string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, identity, "spec", "auth", "agentIdentity")
		_ = unstructured.SetNestedField(obj.Object, credential, "spec", "auth", "credential")
	}
}

func withBaseURL(url string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, url, "spec", "baseURL")
	}
}

// withConfig sets spec.config from a JSON literal, mirroring how the
// apiserver would have decoded a *apiextensionsv1.JSON field into the
// unstructured representation this command actually reads.
func withConfig(rawJSON string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		var v any
		if err := json.Unmarshal([]byte(rawJSON), &v); err != nil {
			panic(err) // fixture-only: a bad literal here is a test bug, not a runtime path
		}
		_ = unstructured.SetNestedField(obj.Object, v, "spec", "config")
	}
}

// Nothing configured is the ordinary first run, not an error.
func TestExistingFor_NoCRYieldsTheZeroValue(t *testing.T) {
	got, err := directorycmd.ExistingFor(ctx, fakeDyn(t), "default", "github")
	require.NoError(t, err)
	assert.Equal(t, relsync.ExistingConfig{}, got)
}

// A re-run must find its own CR and carry every field forward, including
// spec.config verbatim — the wizard does not parse it, the kind does.
func TestExistingFor_CarriesEveryFieldForward(t *testing.T) {
	dyn := fakeDyn(t, relationshipSource("gh", "github", withAuth("ghid", "pat"),
		withBaseURL("https://ghe.example.com/api/v3"), withConfig(`{"orgs":["acme"]}`)))

	got, err := directorycmd.ExistingFor(ctx, dyn, "default", "github")
	require.NoError(t, err)
	assert.Equal(t, "gh", got.Name)
	assert.Equal(t, "ghid", got.Identity)
	assert.Equal(t, "pat", got.Credential)
	assert.Equal(t, "https://ghe.example.com/api/v3", got.Endpoint)
	assert.JSONEq(t, `{"orgs":["acme"]}`, string(got.Config))
}

// Another kind's CR is not this kind's config. Returning it would prefill a
// GitHub wizard from a Slack source.
func TestExistingFor_IgnoresOtherKinds(t *testing.T) {
	dyn := fakeDyn(t, relationshipSource("sl", "slack", withAuth("slid", "bot")))
	got, err := directorycmd.ExistingFor(ctx, dyn, "default", "github")
	require.NoError(t, err)
	assert.Equal(t, relsync.ExistingConfig{}, got)
}

// More than one CR for a kind is legitimate (github.com plus a GHES host).
// Silently picking one would let a re-run overwrite the other, so refuse and
// name them.
func TestExistingFor_RefusesWhenMoreThanOneSourceNamesTheKind(t *testing.T) {
	dyn := fakeDyn(t,
		relationshipSource("source-one", "github", withAuth("i", "c")),
		relationshipSource("source-two", "github", withAuth("i", "c")))

	_, err := directorycmd.ExistingFor(ctx, dyn, "default", "github")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source-one")
	assert.Contains(t, err.Error(), "source-two")
}
