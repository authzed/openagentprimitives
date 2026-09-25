package directorycmd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
)

// agentIdentityGVR mirrors the one in credentials.go — kept separate so the
// test fixtures do not reach into the package's unexported var.
var agentIdentityGVR = schema.GroupVersionResource{
	Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentidentities",
}

// agentIdentity builds an unstructured AgentIdentity fixture named name, with
// one spec.credentials entry per credName, per the CRD shape in
// pkg/apis/v1alpha1/agentidentity_types.go.
func agentIdentity(name string, credNames ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	obj.SetKind("AgentIdentity")
	obj.SetName(name)
	obj.SetNamespace("default")

	creds := make([]interface{}, 0, len(credNames))
	for _, c := range credNames {
		creds = append(creds, map[string]interface{}{"name": c, "type": "static"})
	}
	_ = unstructured.SetNestedSlice(obj.Object, creds, "spec", "credentials")
	return obj
}

// Every credential on every AgentIdentity in the namespace is selectable —
// the sync's credential is not tied to a channel.
func TestAvailableCredentials_ListsEveryCredentialOnEveryIdentity(t *testing.T) {
	dyn := fakeDyn(t,
		agentIdentity("a", "tok1", "tok2"),
		agentIdentity("b", "tok3"))

	got, err := directorycmd.AvailableCredentials(ctx, dyn, "default")
	require.NoError(t, err)
	assert.ElementsMatch(t, []directorycmd.CredentialRef{
		{Identity: "a", Credential: "tok1"},
		{Identity: "a", Credential: "tok2"},
		{Identity: "b", Credential: "tok3"},
	}, got)
}

// None is not an error — it is the state the wizard must be able to explain,
// because writing a CR naming a credential that does not exist produces a
// source that reports Ready=False forever.
func TestAvailableCredentials_NoneIsNotAnError(t *testing.T) {
	got, err := directorycmd.AvailableCredentials(ctx, fakeDyn(t), "default")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// The order must be stable, or a re-run's prefilled default moves under the
// operator between runs.
func TestAvailableCredentials_IsDeterministicallyOrdered(t *testing.T) {
	dyn := fakeDyn(t, agentIdentity("b", "z", "a"), agentIdentity("a", "y"))
	first, err := directorycmd.AvailableCredentials(ctx, dyn, "default")
	require.NoError(t, err)
	second, err := directorycmd.AvailableCredentials(ctx, dyn, "default")
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, directorycmd.CredentialRef{Identity: "a", Credential: "y"}, first[0])
}
