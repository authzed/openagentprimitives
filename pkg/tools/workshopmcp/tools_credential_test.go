package workshopmcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newCredServer builds a Server around c, scoped to the workshop namespace w
// with a builder session identity (sessionNS/sessionName) — mirrors
// newTestRunServer (tools_testrun_test.go), but takes the session identity
// explicitly since request_credential's own key (the Workshop CR) lives in
// SessionNamespace, not the fixed "b"/"x" newTestRunServer hardcodes.
func newCredServer(w, sessionNS, sessionName string, c client.Client) *Server {
	return &Server{
		K8s: c,
		Identity: WorkshopIdentity{
			Namespace:        w,
			SessionNamespace: sessionNS,
			SessionName:      sessionName,
			WorkshopID:       w,
		},
		FieldOwner: defaultFieldOwner,
	}
}

// TestRequestCredential_AppendsKeyedEntryIdempotent is the happy path:
// request_credential appends a keyed (identity, credential) entry to the
// Workshop CR living in the BUILDER session's namespace (not the workshop
// namespace W), and a re-call with the same key is a no-op — never a
// duplicate entry.
func TestRequestCredential_AppendsKeyedEntryIdempotent(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	ai := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-abc123", Name: "weather-ai"}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, ai).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "api_key", AuthKind: "pat"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, "weather-ai", body["identity"])
	assert.Equal(t, "api_key", body["credential"])
	assert.Equal(t, "requested", body["status"])

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.Len(t, got.Spec.CredentialRequests, 1)
	assert.Equal(t, "weather-ai", got.Spec.CredentialRequests[0].Identity)
	assert.Equal(t, "api_key", got.Spec.CredentialRequests[0].Credential)
	assert.Equal(t, "pat", got.Spec.CredentialRequests[0].AuthKind)

	res2 := callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "api_key", AuthKind: "pat"})
	require.False(t, res2.IsError)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	assert.Len(t, got.Spec.CredentialRequests, 1, "re-request of the same (identity,credential) is a no-op")
}

// TestRequestCredential_DistinctCredentialAppendsSecondEntry proves the
// dedupe key is (identity, credential), not identity alone: a second,
// DIFFERENT credential on the same identity is a genuine new entry.
func TestRequestCredential_DistinctCredentialAppendsSecondEntry(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	ai := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-abc123", Name: "weather-ai"}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, ai).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	_ = callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "api_key", AuthKind: "pat"})
	_ = callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "webhook_secret", AuthKind: "static"})

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.Len(t, got.Spec.CredentialRequests, 2, "a distinct credential on the same identity is a new entry")
}

// mcpServerWithCred returns a minimal, valid MCPServer declaring credName as
// its spec.auth.credential — the shape oauth-mcp's precondition check looks
// for via passthroughcatalog.LookupMCPServerByCredentialInNamespace. Mirrors
// pkg/platform/identityd/oauth_discovery_test.go's helper of the same name.
func mcpServerWithCred(ns, name, credName, serverURL string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       serverURL,
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "oauth",
				Credential: credName,
			},
		},
	}
}

// TestRequestCredential_OAuthMCP_RecordsKeyedEntry proves shared OAuth-MCP
// credentials are recorded the same way pat/static are — a keyed
// (identity, credential) entry on the Workshop CR with AuthKind preserved —
// now that the agent-owned OAuth flow (identityd authorize/callback +
// admind write) exists to service the request, GIVEN a backing MCPServer
// already applied in W (the precondition oauth-mcp now requires).
func TestRequestCredential_OAuthMCP_RecordsKeyedEntry(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	ai := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-abc123", Name: "weather-ai"}}
	srv := mcpServerWithCred("ws-abc123", "weather-mcp", "oauth_token", "https://weather.example/mcp")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, ai, srv).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "oauth_token", AuthKind: "oauth-mcp"})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Equal(t, "weather-ai", body["identity"])
	assert.Equal(t, "oauth_token", body["credential"])
	assert.Equal(t, "requested", body["status"])

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	require.Len(t, got.Spec.CredentialRequests, 1)
	assert.Equal(t, "weather-ai", got.Spec.CredentialRequests[0].Identity)
	assert.Equal(t, "oauth_token", got.Spec.CredentialRequests[0].Credential)
	assert.Equal(t, "oauth-mcp", got.Spec.CredentialRequests[0].AuthKind)
}

// TestRequestCredential_OAuthMCP_RequiresBackingMCPServer pins a regression:
// in the wild, a builder session called request_credential(authKind:
// "oauth-mcp") for a credential with no MCPServer ever applied in the
// workshop. The card was minted and delivered anyway, and only 400'd
// ("No matching service" / "No service is configured for this credential in
// this workshop") when the person clicked Connect, since
// handleLinkAgentOAuthGet's namespace-scoped MCPServer lookup
// (passthroughcatalog.LookupMCPServerByCredentialInNamespace) had nothing to
// find. request_credential must catch this at request time, not leave a
// dead-on-arrival card for the person to discover.
func TestRequestCredential_OAuthMCP_RequiresBackingMCPServer(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	ai := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-abc123", Name: "posthog-identity"}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, ai).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{Identity: "posthog-identity", Credential: "posthog_token", AuthKind: "oauth-mcp"})
	require.True(t, res.IsError, "oauth-mcp with no backing MCPServer must fail closed, not mint a dead card")
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "posthog_token")
	assert.Contains(t, body["error"], "MCPServer")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	assert.Empty(t, got.Spec.CredentialRequests, "a rejected oauth-mcp request must write nothing to the Workshop CR")
}

// TestRequestCredential_IdentityMustExistInW proves the target AgentIdentity
// must already exist in the workshop namespace W (authored via
// workshop_apply) — request_credential does not create it.
func TestRequestCredential_IdentityMustExistInW(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "api_key", AuthKind: "pat"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "weather-ai")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}, &got))
	assert.Empty(t, got.Spec.CredentialRequests, "a missing AgentIdentity must write nothing to the Workshop CR")
}

// TestRequestCredential_RequiresAllArgs pins the argument guard: identity,
// credential, and authKind are all required.
func TestRequestCredential_RequiresAllArgs(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "identity, credential, and authKind are required")
}

// TestRequestCredential_UnknownAuthKind pins the authKind enum guard.
func TestRequestCredential_UnknownAuthKind(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{
		Namespace: "builder-b", Name: spiceboxv1alpha1.WorkshopName("builder-x")}}
	ai := &spiceboxv1alpha1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-abc123", Name: "weather-ai"}}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ws, ai).Build()
	s := newCredServer("ws-abc123", "builder-b", "builder-x", c)

	res := callTool(t, s.handleRequestCredential, credArgs{Identity: "weather-ai", Credential: "api_key", AuthKind: "bogus"})
	require.True(t, res.IsError)
	body := decodeResultBody(t, res)
	assert.Contains(t, body["error"], "authKind must be pat, static, or oauth-mcp")
}
