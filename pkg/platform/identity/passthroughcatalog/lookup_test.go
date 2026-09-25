package passthroughcatalog

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func mcpServer(ns, name, credName, authType, provider string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://example.com/mcp",
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Credential: credName,
				Type:       authType,
				Provider:   provider,
			},
		},
	}
}

func TestLookupMCPServerByCredential(t *testing.T) {
	t.Run("match: explicit credential field", func(t *testing.T) {
		srv := mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "linear")
		c := newTestClient(t, srv)

		got, err := LookupMCPServerByCredential(context.Background(), c, "linear-oauth")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "linear-mcp", got.Name)
	})

	t.Run("no match: empty credential field never falls back to metadata.name", func(t *testing.T) {
		// Per the no-inference rule, an MCPServer without spec.auth.credential
		// is NOT discoverable by its metadata.name. Operators must declare
		// the credential name explicitly.
		srv := &spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "my-creds"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Name:    "my-creds",
				Version: "1.0",
				Server: spiceboxv1alpha1.MCPServerServer{
					URL:       "https://example.com/mcp",
					Transport: "http",
				},
				Auth: spiceboxv1alpha1.MCPServerAuth{
					Provider: "some-provider",
				},
			},
		}
		c := newTestClient(t, srv)

		_, err := LookupMCPServerByCredential(context.Background(), c, "my-creds")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "my-creds")
	})

	t.Run("no match: returns error naming the credential", func(t *testing.T) {
		srv := mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "linear")
		c := newTestClient(t, srv)

		_, err := LookupMCPServerByCredential(context.Background(), c, "nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("multiple matches: operator misconfiguration error", func(t *testing.T) {
		srv1 := mcpServer("ns-a", "linear-a", "linear-oauth", "oauth", "linear")
		srv2 := mcpServer("ns-b", "linear-b", "linear-oauth", "oauth", "linear")
		c := newTestClient(t, srv1, srv2)

		_, err := LookupMCPServerByCredential(context.Background(), c, "linear-oauth")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple")
	})
}

func TestLookupMCPServerByCredentialInNamespace(t *testing.T) {
	t.Run("match: chosen from the requested namespace", func(t *testing.T) {
		srv := mcpServer("workshop-w", "weather-mcp", "weather-key", "oauth", "weather")
		c := newTestClient(t, srv)

		got, err := LookupMCPServerByCredentialInNamespace(context.Background(), c, "workshop-w", "weather-key")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "weather-mcp", got.Name)
	})

	t.Run("scope: a same-named MCPServer in a DIFFERENT namespace is not chosen", func(t *testing.T) {
		// This is the security property the namespace-scoped lookup exists for:
		// a credential name that collides with a MCPServer in another
		// namespace (e.g. a production one) must never be selected when the
		// caller asked for one namespace only.
		prod := mcpServer("prod", "prod-weather-mcp", "weather-key", "oauth", "weather")
		c := newTestClient(t, prod)

		_, err := LookupMCPServerByCredentialInNamespace(context.Background(), c, "workshop-w", "weather-key")
		require.Error(t, err, "zero matches IN THE REQUESTED NAMESPACE must fail closed, even though a match exists elsewhere")
		assert.Contains(t, err.Error(), "weather-key")
	})

	t.Run("no MCPServer in the namespace at all: fails closed", func(t *testing.T) {
		c := newTestClient(t)

		_, err := LookupMCPServerByCredentialInNamespace(context.Background(), c, "workshop-w", "weather-key")
		require.Error(t, err)
	})

	t.Run("multiple matches within the SAME namespace: operator misconfiguration error", func(t *testing.T) {
		srv1 := mcpServer("workshop-w", "weather-a", "weather-key", "oauth", "weather")
		srv2 := mcpServer("workshop-w", "weather-b", "weather-key", "oauth", "weather")
		c := newTestClient(t, srv1, srv2)

		_, err := LookupMCPServerByCredentialInNamespace(context.Background(), c, "workshop-w", "weather-key")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple")
	})

	t.Run("empty namespace is refused rather than silently going cluster-wide", func(t *testing.T) {
		srv := mcpServer("prod", "prod-weather-mcp", "weather-key", "oauth", "weather")
		c := newTestClient(t, srv)

		_, err := LookupMCPServerByCredentialInNamespace(context.Background(), c, "", "weather-key")
		require.Error(t, err, "an empty namespace must never silently widen the search to cluster-wide")
	})
}

func TestCredentialAuthType(t *testing.T) {
	cases := []struct {
		name string
		srv  *spiceboxv1alpha1.MCPServer
		want string
	}{
		{
			name: "nil server defaults to static",
			srv:  nil,
			want: AuthTypeStatic,
		},
		{
			name: "empty auth type defaults to static",
			srv:  mcpServer("default", "x", "x", "", "p"),
			want: AuthTypeStatic,
		},
		{
			name: "explicit static",
			srv:  mcpServer("default", "x", "x", "static", "p"),
			want: AuthTypeStatic,
		},
		{
			name: "explicit oauth",
			srv:  mcpServer("default", "x", "x", "oauth", "p"),
			want: AuthTypeOAuth,
		},
		{
			name: "unknown values normalize to static",
			srv:  mcpServer("default", "x", "x", "weird-value", "p"),
			want: AuthTypeStatic,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CredentialAuthType(tc.srv))
		})
	}
}

// TestResolveLinkedServices — table-driven coverage of the linked-services
// resolver and the label projection layered on it. It powers the
// security-critical bullet list shown to a userPassthrough session's owner when
// another user asks to join; regressions in the label set or its filtering
// behaviour would silently weaken the warning UI.
//
// Each row asserts the PAIRS as well as the labels, because the pairs carry the
// property callers actually depend on and cannot recover afterwards: which
// credential each surviving label came from. wantServices is routinely shorter
// than credentialList and in a different order — that is the resolver's
// contract, not an edge case, and the Slack App Home card renderer once assumed
// the opposite and indexed a label slice under a credential slice's length
// guard.
func TestResolveLinkedServices(t *testing.T) {
	cases := []struct {
		name           string
		mcpServers     []*spiceboxv1alpha1.MCPServer
		credentialList []string
		classCreds     map[string]struct{}
		wantServices   []LinkedService
		want           []string
	}{
		{
			name:           "empty credential list: empty result",
			credentialList: nil,
			wantServices:   nil,
			want:           nil,
		},
		{
			name: "happy path: intersect credentials with MCPServer providers, dedupe + sort",
			mcpServers: []*spiceboxv1alpha1.MCPServer{
				mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "Linear"),
				mcpServer("default", "github-mcp", "github-token", "static", "GitHub"),
			},
			credentialList: []string{"github-token", "linear-oauth"},
			wantServices: []LinkedService{
				{CredentialName: "github-token", Label: "GitHub"},
				{CredentialName: "linear-oauth", Label: "Linear"},
			},
			want: []string{"GitHub", "Linear"}, // sorted
		},
		{
			name: "credential with no backing MCPServer: skipped",
			mcpServers: []*spiceboxv1alpha1.MCPServer{
				mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "Linear"),
			},
			credentialList: []string{"linear-oauth", "ghost-cred"},
			wantServices:   []LinkedService{{CredentialName: "linear-oauth", Label: "Linear"}},
			want:           []string{"Linear"}, // ghost-cred has no MCPServer → not surfaced
		},
		{
			name: "EVERY credential unbacked: empty result, not a same-length list with holes",
			// The App Home shape: a userPassthrough AgentClass whose credentials
			// all come from a ToolBundle. Toolkit-derived credentials have no
			// MCPServer by construction, so one credential in yields zero pairs
			// out — the length mismatch a caller must not be able to mis-index.
			mcpServers:     nil,
			credentialList: []string{"toolkit-token"},
			wantServices:   nil,
			want:           nil,
		},
		{
			name: "classCreds filter: only credentials this AgentClass uses appear",
			mcpServers: []*spiceboxv1alpha1.MCPServer{
				mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "Linear"),
				mcpServer("default", "github-mcp", "github-token", "static", "GitHub"),
				mcpServer("default", "salesforce-mcp", "sf-oauth", "oauth", "Salesforce"),
			},
			credentialList: []string{"linear-oauth", "github-token", "sf-oauth"},
			classCreds:     map[string]struct{}{"linear-oauth": {}, "github-token": {}},
			wantServices: []LinkedService{
				{CredentialName: "github-token", Label: "GitHub"},
				{CredentialName: "linear-oauth", Label: "Linear"},
			},
			want: []string{"GitHub", "Linear"}, // sf-oauth excluded by class filter
		},
		{
			name: "duplicate providers: deduped, and the FIRST credential keeps the label",
			mcpServers: []*spiceboxv1alpha1.MCPServer{
				mcpServer("default", "linear-mcp-a", "linear-a", "oauth", "Linear"),
				mcpServer("default", "linear-mcp-b", "linear-b", "oauth", "Linear"),
			},
			credentialList: []string{"linear-a", "linear-b"},
			wantServices:   []LinkedService{{CredentialName: "linear-a", Label: "Linear"}},
			want:           []string{"Linear"},
		},
		{
			name: "missing Provider label: falls back to MCPServer name",
			mcpServers: []*spiceboxv1alpha1.MCPServer{
				mcpServer("default", "no-provider-mcp", "no-provider-cred", "oauth", ""),
			},
			credentialList: []string{"no-provider-cred"},
			wantServices:   []LinkedService{{CredentialName: "no-provider-cred", Label: "no-provider-mcp"}},
			want:           []string{"no-provider-mcp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.mcpServers))
			for _, m := range tc.mcpServers {
				objs = append(objs, m)
			}
			c := newTestClient(t, objs...)

			services, err := ResolveLinkedServices(context.Background(), c, tc.credentialList, tc.classCreds)
			require.NoError(t, err)
			assert.Equal(t, tc.wantServices, services,
				"each surviving label must arrive with the credential it actually resolved from")

			got, err := ResolveLinkedServiceLabels(context.Background(), c, tc.credentialList, tc.classCreds)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, LinkedServiceLabels(services),
				"the label list must stay exactly the pair list's projection — one implementation, not two")
		})
	}
}

func TestProviderLabel(t *testing.T) {
	cases := []struct {
		name string
		srv  *spiceboxv1alpha1.MCPServer
		want string
	}{
		{
			name: "nil server returns empty",
			srv:  nil,
			want: "",
		},
		{
			name: "explicit provider used",
			srv:  mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "linear"),
			want: "linear",
		},
		{
			name: "name fallback when provider empty",
			srv:  mcpServer("default", "my-mcp", "creds", "oauth", ""),
			want: "my-mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ProviderLabel(tc.srv))
		})
	}
}

// TestCredentialNameForServer_Explicit codifies the no-inference contract:
// spec.auth.credential is read verbatim; there is NO fallback to
// metadata.name. setup-identity and the runner descriptor must reach the
// same answer through this function.
func TestCredentialNameForServer_Explicit(t *testing.T) {
	cases := []struct {
		name string
		srv  *spiceboxv1alpha1.MCPServer
		want string
	}{
		{
			name: "nil server returns empty",
			srv:  nil,
			want: "",
		},
		{
			name: "explicit credential returned verbatim",
			srv:  mcpServer("default", "linear-mcp", "linear-oauth", "oauth", "linear"),
			want: "linear-oauth",
		},
		{
			name: "empty credential with provider set: NO metadata.name fallback",
			srv:  mcpServer("default", "linear-mcp", "", "oauth", "oauth-mcp"),
			want: "",
		},
		{
			name: "empty credential with no provider: empty (truly unauthenticated)",
			srv:  mcpServer("default", "linear-mcp", "", "", ""),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CredentialNameForServer(tc.srv))
		})
	}
}
