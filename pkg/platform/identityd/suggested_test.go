package identityd

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// suggestedTestClient builds a fake client pre-seeded with the given objects
// using the same scheme helper as the portal handler tests.
func suggestedTestClient(t *testing.T, objs ...clientpkg.Object) clientpkg.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
}

// makePassthroughAgentClass returns an AgentClass in identityMode=userPassthrough
// with the given MCPServer refs. Each element of mcpRefs is used as both the
// LLM-prefix Name and the CR Ref (sufficient for unit tests; prod CRs may differ).
func makePassthroughAgentClass(namespace, name string, mcpRefs ...string) *spiceboxv1alpha1.AgentClass {
	refs := make([]spiceboxv1alpha1.AgentClassMCPServerRef, 0, len(mcpRefs))
	for _, r := range mcpRefs {
		refs = append(refs, spiceboxv1alpha1.AgentClassMCPServerRef{Name: r, Ref: r})
	}
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			MCPServers:   refs,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test",
				Name:     "test",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "test", Key: "key"},
			},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    10,
				MaxTokens:   1000,
				MaxDuration: metav1.Duration{},
			},
		},
	}
}

// makeAgentModeAgentClass returns an AgentClass in identityMode=agent (not passthrough).
func makeAgentModeAgentClass(namespace, name string, mcpRefs ...string) *spiceboxv1alpha1.AgentClass {
	ac := makePassthroughAgentClass(namespace, name, mcpRefs...)
	ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeAgent
	return ac
}

// makeMCPServerWithCred returns an MCPServer with an explicit credential name
// and provider label.
func makeMCPServerWithCred(namespace, name, credName, provider string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "http://localhost:8080", Transport: "http"},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Credential: credName,
				Provider:   provider,
			},
		},
	}
}

// makeMCPServerWithAuthType returns an MCPServer with an explicit credential
// name, provider label, and Spec.Auth.Type (e.g. "static" or "oauth").
func makeMCPServerWithAuthType(namespace, name, credName, provider, authType string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "http://localhost:8080", Transport: "http"},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Credential: credName,
				Provider:   provider,
				Type:       authType,
			},
		},
	}
}

// TestSuggestedCredentialsForUser covers the aggregator with seven table cases.
func TestSuggestedCredentialsForUser(t *testing.T) {
	const ns = "default"

	cases := []struct {
		name          string
		objs          []clientpkg.Object
		alreadyLinked []string          // credential names the user already has linked
		wantNames     []string          // expected credential names (sorted)
		wantLabels    map[string]string // expected label per name (checked for keys present in map)
	}{
		{
			name: "single passthrough AgentClass with one MCPServer credential → result includes credential + provider label",
			objs: []clientpkg.Object{
				makePassthroughAgentClass(ns, "bot-a", "github-srv"),
				makeMCPServerWithCred(ns, "github-srv", "github-pat", "GitHub"),
			},
			wantNames:  []string{"github-pat"},
			wantLabels: map[string]string{"github-pat": "GitHub"},
		},
		{
			name: "AgentClass in agent mode (not userPassthrough) → contributes nothing",
			objs: []clientpkg.Object{
				makeAgentModeAgentClass(ns, "agent-bot", "github-srv"),
				makeMCPServerWithCred(ns, "github-srv", "github-pat", "GitHub"),
			},
			wantNames: []string{},
		},
		{
			name: "two AgentClasses requiring the same credential (different provider labels) → dedup to one entry; first-seen label wins",
			objs: []clientpkg.Object{
				// bot-a is inserted first; its MCPServer label takes priority
				// under portalLabelMap's first-writer-wins policy.
				makePassthroughAgentClass(ns, "bot-a", "gh-srv-a"),
				makePassthroughAgentClass(ns, "bot-b", "gh-srv-b"),
				makeMCPServerWithCred(ns, "gh-srv-a", "github-pat", "GitHub (primary)"),
				makeMCPServerWithCred(ns, "gh-srv-b", "github-pat", "GitHub (secondary)"),
			},
			wantNames: []string{"github-pat"},
			// First MCPServer to register the cred name wins; that is gh-srv-a.
			wantLabels: map[string]string{"github-pat": "GitHub (primary)"},
		},
		{
			name: "user already linked the credential → not in suggestions",
			objs: []clientpkg.Object{
				makePassthroughAgentClass(ns, "bot-a", "github-srv"),
				makeMCPServerWithCred(ns, "github-srv", "github-pat", "GitHub"),
			},
			alreadyLinked: []string{"github-pat"},
			wantNames:     []string{},
		},
		{
			name: "result is sorted alphabetically by credential name",
			objs: []clientpkg.Object{
				makePassthroughAgentClass(ns, "bot-a", "zzz-srv", "aaa-srv"),
				makeMCPServerWithCred(ns, "zzz-srv", "zzz-cred", "ZZZ Provider"),
				makeMCPServerWithCred(ns, "aaa-srv", "aaa-cred", "AAA Provider"),
			},
			wantNames: []string{"aaa-cred", "zzz-cred"},
		},
		{
			name: "AgentClass with dangling MCPServer ref → best-effort: skip missing ref, don't crash",
			objs: []clientpkg.Object{
				// missing-srv is NOT added to the client; real-srv is.
				makePassthroughAgentClass(ns, "bot-a", "missing-srv", "real-srv"),
				makeMCPServerWithCred(ns, "real-srv", "real-cred", "Real Provider"),
			},
			wantNames:  []string{"real-cred"},
			wantLabels: map[string]string{"real-cred": "Real Provider"},
		},
		{
			name:      "empty cluster → empty result",
			objs:      []clientpkg.Object{},
			wantNames: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := suggestedTestClient(t, tc.objs...)

			linked := map[string]bool{}
			for _, n := range tc.alreadyLinked {
				linked[n] = true
			}

			got, err := suggestedCredentialsForUser(context.Background(), c, linked, logr.Discard())
			require.NoError(t, err)

			gotNames := make([]string, 0, len(got))
			for _, pc := range got {
				gotNames = append(gotNames, pc.Name)
			}
			assert.Equal(t, tc.wantNames, gotNames,
				"credential names must match and be sorted")

			if tc.wantLabels != nil {
				gotByName := map[string]portalCredential{}
				for _, pc := range got {
					gotByName[pc.Name] = pc
				}
				for credName, wantLabel := range tc.wantLabels {
					assert.Equal(t, wantLabel, gotByName[credName].Label,
						"provider label for %q", credName)
				}
			}
		})
	}
}

// TestPortalSuggestedSection_RoutesOAuthToLinkOAuth verifies that a credential
// backed by an MCPServer with Spec.Auth.Type == "oauth" has NeedsRefresh == true,
// so the portal template routes it to /link/oauth/<credname>.
func TestPortalSuggestedSection_RoutesOAuthToLinkOAuth(t *testing.T) {
	const ns = "default"
	c := suggestedTestClient(t,
		makePassthroughAgentClass(ns, "bot-a", "linear-mcp"),
		makeMCPServerWithAuthType(ns, "linear-mcp", "linear-oauth", "Linear", "oauth"),
	)

	got, err := suggestedCredentialsForUser(context.Background(), c, map[string]bool{}, logr.Discard())
	require.NoError(t, err)
	require.Len(t, got, 1, "exactly one suggested credential")

	cred := got[0]
	assert.Equal(t, "linear-oauth", cred.Name)
	assert.True(t, cred.NeedsRefresh, "MCPServer with type=oauth must set NeedsRefresh=true")
}

// TestPortalSuggestedSection_PATKeepsPATRoute verifies that a credential backed
// by an MCPServer with Spec.Auth.Type == "static" (or no type at all) has
// NeedsRefresh == false, so the portal template routes it to /my/accounts/<credname>/link.
func TestPortalSuggestedSection_PATKeepsPATRoute(t *testing.T) {
	const ns = "default"

	cases := []struct {
		name     string
		authType string // "" or "static"
	}{
		{name: "type=static → NeedsRefresh false", authType: "static"},
		{name: "type=empty (legacy) → NeedsRefresh false", authType: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := suggestedTestClient(t,
				makePassthroughAgentClass(ns, "bot-a", "github-mcp"),
				makeMCPServerWithAuthType(ns, "github-mcp", "github-pat", "GitHub", tc.authType),
			)

			got, err := suggestedCredentialsForUser(context.Background(), c, map[string]bool{}, logr.Discard())
			require.NoError(t, err)
			require.Len(t, got, 1, "exactly one suggested credential")

			cred := got[0]
			assert.Equal(t, "github-pat", cred.Name)
			assert.False(t, cred.NeedsRefresh, "MCPServer with type=%q must set NeedsRefresh=false", tc.authType)
		})
	}
}
