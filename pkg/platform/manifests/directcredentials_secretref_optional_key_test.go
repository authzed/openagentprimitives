package manifests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// crdByName finds the bundled CustomResourceDefinition document with the
// given metadata.name, failing the test if it is not present. Every CRD in
// this repo ships as exactly one document in the embedded install bundle, so
// "not found" always means a typo'd name, never a legitimately absent CRD.
func crdByName(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	for _, d := range allBundledDocs(t) {
		if d.GetKind() == "CustomResourceDefinition" && d.GetName() == name {
			return d
		}
	}
	t.Fatalf("CustomResourceDefinition %q not found in bundled install.yaml", name)
	return nil
}

// crdSchemaRoot returns the openAPIV3Schema of a CRD's sole v1alpha1 version.
// Every CRD in this repo ships exactly one version, so versions[0] is always
// it; a CRD that ever grows a second version will need this updated anyway.
func crdSchemaRoot(t *testing.T, crd *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found, "%s: spec.versions", crd.GetName())
	require.NotEmpty(t, versions, "%s: spec.versions is empty", crd.GetName())
	v0, ok := versions[0].(map[string]interface{})
	require.True(t, ok, "%s: spec.versions[0] must be a map", crd.GetName())
	schema, found, err := unstructured.NestedMap(v0, "schema", "openAPIV3Schema")
	require.NoError(t, err)
	require.True(t, found, "%s: versions[0].schema.openAPIV3Schema", crd.GetName())
	return schema
}

// descend walks a chain of property names, each stepping into
// properties.<name> of the current node. Use withItems first when a step is
// an array field (credentials is spec.credentials: []AgentCredential).
func descend(t *testing.T, node map[string]interface{}, names ...string) map[string]interface{} {
	t.Helper()
	for _, n := range names {
		next, found, err := unstructured.NestedMap(node, "properties", n)
		require.NoErrorf(t, err, "properties.%s", n)
		require.Truef(t, found, "properties.%s not found", n)
		node = next
	}
	return node
}

// withItems steps from an array field's node into its items schema.
func withItems(t *testing.T, node map[string]interface{}) map[string]interface{} {
	t.Helper()
	items, found, err := unstructured.NestedMap(node, "items")
	require.NoError(t, err, "items")
	require.True(t, found, "items not found — caller's node is not an array field")
	return items
}

func requiredOf(t *testing.T, node map[string]interface{}) []string {
	t.Helper()
	req, _, err := unstructured.NestedStringSlice(node, "required")
	require.NoError(t, err, "required")
	return req
}

// TestDirectCredentialsSecretRefKeyIsOptional is the regression for a desktop
// hand-test finding: AgentCredential.DirectCredentials.SecretRef used the same
// SecretKeyRef type as every other credential source, whose Key field carried
// no `+optional` marker. controller-gen therefore emitted secretRef.key as
// STRUCTURALLY REQUIRED even though
// pkg/platform/identity/credkind/directcredentials's ValidateSpec has always
// accepted (indeed required) an EMPTY key for source=secret — a multi-key
// login Secret has no single named value key. The apiserver rejected an
// omitted key with a confusing "secretRef.key: Required value" before
// ValidateSpec ever ran.
//
// The fix gave DirectCredentialsSource its own DirectCredentialSecretRef type
// (pkg/apis/v1alpha1/directcredentials_types.go) rather than loosening the
// shared SecretKeyRef — see TestSecretKeyRefStaysRequiredElsewhere for why.
func TestDirectCredentialsSecretRefKeyIsOptional(t *testing.T) {
	crd := crdByName(t, "agentidentities.agentprimitives.authzed.com")
	root := crdSchemaRoot(t, crd)

	credItems := withItems(t, descend(t, root, "spec", "credentials"))

	dcSecretRef := descend(t, credItems, "directCredentials", "secretRef")
	dcReq := requiredOf(t, dcSecretRef)
	assert.NotContains(t, dcReq, "key",
		"directCredentials.secretRef.key must NOT be structurally required — "+
			"a secret-source credential's ValidateSpec requires it be empty, so "+
			"the apiserver must accept an omitted key")
	assert.Contains(t, dcReq, "name", "directCredentials.secretRef.name stays required")

	// REGRESSION GUARD: static's secretRef is a genuinely different field
	// (StaticCredentialSource.SecretRef, still typed SecretKeyRef) and must
	// keep requiring both key and name — only directCredentials grew its own
	// optional-key ref type.
	staticSecretRef := descend(t, credItems, "static", "secretRef")
	staticReq := requiredOf(t, staticSecretRef)
	assert.Contains(t, staticReq, "key", "static.secretRef.key must stay required")
	assert.Contains(t, staticReq, "name", "static.secretRef.name must stay required")
}

// TestSecretKeyRefStaysRequiredElsewhere pins the regression-guard decision:
// SecretKeyRef (pkg/apis/v1alpha1/agentidentity_types.go) is NOT made
// optional at the type level, because it is also embedded by
// ClusterSkillSource.Spec.Auth.SecretRef and AgentClass's
// ModelConfig.APIKey/Settings' DefaultModel.APIKey — neither of which has any
// ValidateSpec-equivalent Go-level enforcement of key presence; the apiserver's
// structural `required` marker is the ONLY thing stopping a keyless ref there
// today. Loosening the shared type would have silently deferred their
// key-presence failures from an immediate apply-time apiserver rejection to an
// async reconcile-time status condition — a real regression outside this
// task's scope. Only DirectCredentialsSource got a dedicated type instead.
func TestSecretKeyRefStaysRequiredElsewhere(t *testing.T) {
	// ClusterSkillSource.spec.auth.secretRef
	cssCRD := crdByName(t, "clusterskillsources.agentprimitives.authzed.com")
	cssRoot := crdSchemaRoot(t, cssCRD)
	cssSecretRef := descend(t, cssRoot, "spec", "auth", "secretRef")
	cssReq := requiredOf(t, cssSecretRef)
	assert.Contains(t, cssReq, "key", "ClusterSkillSource.spec.auth.secretRef.key must stay required")
	assert.Contains(t, cssReq, "name", "ClusterSkillSource.spec.auth.secretRef.name must stay required")

	// AgentClass.spec.model.apiKey
	acCRD := crdByName(t, "agentclasses.agentprimitives.authzed.com")
	acRoot := crdSchemaRoot(t, acCRD)
	acAPIKey := descend(t, acRoot, "spec", "model", "apiKey")
	acReq := requiredOf(t, acAPIKey)
	assert.Contains(t, acReq, "key", "AgentClass.spec.model.apiKey.key must stay required")
	assert.Contains(t, acReq, "name", "AgentClass.spec.model.apiKey.name must stay required")
}
