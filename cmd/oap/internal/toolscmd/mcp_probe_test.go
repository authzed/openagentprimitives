package toolscmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func newMCPProbeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// aiWithCredential builds an AgentIdentity carrying a static credential
// whose name equals the server name portion of a former "mcp:<server>"
// binding-match string. The helper name is preserved for test clarity.
func aiWithBinding(name, ns, match string) *spiceboxv1alpha1.AgentIdentity {
	// match is still passed as "mcp:<server>" from call sites; strip the prefix.
	credName := match
	if after, ok := strings.CutPrefix(match, "mcp:"); ok {
		credName = after
	}
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "dummy", Key: "token"},
				},
			}},
		},
	}
}

func TestPickIdentityForMCPServer_SingleMatch(t *testing.T) {
	scheme := newMCPProbeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aiWithBinding("hubspot-creds", "default", "mcp:hubspot-companies")).
		Build()
	got, err := pickIdentityForMCPServer(context.Background(), c, "default", "hubspot-companies")
	require.NoError(t, err, "pickIdentityForMCPServer")
	assert.Equal(t, "hubspot-creds", got, "matched identity name")
}

func TestPickIdentityForMCPServer_NoMatch(t *testing.T) {
	scheme := newMCPProbeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aiWithBinding("linear-creds", "default", "mcp:linear")).
		Build()
	_, err := pickIdentityForMCPServer(context.Background(), c, "default", "hubspot-companies")
	require.Error(t, err, "expected no-match error")
	assert.Contains(t, err.Error(), "no AgentIdentity", "error should mention 'no AgentIdentity'")
}

func TestPickIdentityForMCPServer_Ambiguous(t *testing.T) {
	scheme := newMCPProbeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			aiWithBinding("a", "default", "mcp:hubspot-companies"),
			aiWithBinding("b", "default", "mcp:hubspot-companies"),
		).
		Build()
	_, err := pickIdentityForMCPServer(context.Background(), c, "default", "hubspot-companies")
	require.Error(t, err, "expected ambiguity error")
	assert.Contains(t, err.Error(), "multiple AgentIdentities", "error should mention ambiguity")
	assert.Contains(t, err.Error(), "--via", "error should mention --via to disambiguate")
}

func TestPickIdentityForMCPServer_NamespaceScoped(t *testing.T) {
	scheme := newMCPProbeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(aiWithBinding("hubspot-creds", "other", "mcp:hubspot-companies")).
		Build()
	_, err := pickIdentityForMCPServer(context.Background(), c, "default", "hubspot-companies")
	assert.Error(t, err, "expected no-match when identity is in a different namespace")
}

func TestProbeToolSections(t *testing.T) {
	tools := []probe.Tool{
		{
			Name:         "search_owners",
			Description:  "Lists owners.\nMore detail on the next line.",
			InputSchema:  []byte(`{"type":"object","properties":{"ownerIds":{"type":"array"}}}`),
			OutputSchema: []byte(`{"type":"object","properties":{"resolved":{"type":"integer"}}}`),
			Annotations:  probe.Annotations{ReadOnlyHint: true},
		},
		{
			Name:        "manage_crm_objects",
			Description: "Mutates records.",
			Annotations: probe.Annotations{DestructiveHint: true},
		},
	}

	cases := []struct {
		name        string
		showSchemas bool
		check       func(t *testing.T, secs []contract.Section)
	}{
		{
			name:        "schemas off: title carries hints, body is description first line only",
			showSchemas: false,
			check: func(t *testing.T, secs []contract.Section) {
				require.Len(t, secs, 2, "one section per tool")
				assert.Equal(t, "search_owners [read-only]", secs[0].Title)
				assert.Equal(t, "Lists owners.", secs[0].Body)
				assert.Equal(t, "manage_crm_objects [destructive]", secs[1].Title)
				assert.Equal(t, "Mutates records.", secs[1].Body)
			},
		},
		{
			name:        "schemas on: body appends indented input + output schema, '(none)' when absent",
			showSchemas: true,
			check: func(t *testing.T, secs []contract.Section) {
				require.Len(t, secs, 2, "one section per tool")
				assert.Contains(t, secs[0].Body, "Lists owners.", "keeps description")
				assert.Contains(t, secs[0].Body, "input schema:", "labels the input schema block")
				assert.Contains(t, secs[0].Body, "\"ownerIds\"", "includes pretty-printed input schema")
				assert.Contains(t, secs[0].Body, "output schema:", "labels the output schema block")
				assert.Contains(t, secs[0].Body, "\"resolved\"", "includes pretty-printed output schema")
				assert.Contains(t, secs[1].Body, "input schema:\n(none)", "missing input schema renders as (none)")
				assert.Contains(t, secs[1].Body, "output schema:\n(none)", "missing output schema renders as (none)")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, probeToolSections(tools, tc.showSchemas))
		})
	}
}
