package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// buildExplanationAC assembles an AgentClass that references one MCPServer
// (so the passthrough resolver picks up its auth metadata) plus optional
// declared credential reasons.
func buildExplanationAC(name, display string, explanations ...spiceboxv1alpha1.CredentialExplanationSpec) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: display,
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "srv", Ref: "srv"},
			},
			CredentialExplanations: explanations,
		},
	}
}

// mcpServerWithAuth builds an MCPServer named "srv" with the given auth
// metadata. The resolver derives the credential name from auth.credential.
func mcpServerWithAuth(auth spiceboxv1alpha1.MCPServerAuth) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "srv", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "srv",
			Auth: auth,
		},
	}
}

func TestBuildExplanationItems(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name             string
		mcpServer        *spiceboxv1alpha1.MCPServer
		ac               *spiceboxv1alpha1.AgentClass
		missing          []string
		wantCredential   string
		wantTitle        string
		wantDescNonEmpty bool
		wantWhy          string // exact expected Why ("" ⇒ render-time fallback)
	}{
		{
			name: "tool-field Title + declared reason: Title from auth.title, Why verbatim",
			mcpServer: mcpServerWithAuth(spiceboxv1alpha1.MCPServerAuth{
				Credential:  "linear-oauth",
				Title:       "Linear",
				Description: "Linear API access token",
			}),
			ac: buildExplanationAC("cls", "Triage Bot", spiceboxv1alpha1.CredentialExplanationSpec{
				Credential: "linear-oauth",
				Reason:     "So Triage Bot can file your issues.",
			}),
			missing:          []string{"linear-oauth"},
			wantCredential:   "linear-oauth",
			wantTitle:        "Linear",
			wantDescNonEmpty: true,
			wantWhy:          "So Triage Bot can file your issues.",
		},
		{
			name: "provider-catalog Title: github-pat → GitHub, undeclared reason ⇒ empty Why",
			mcpServer: mcpServerWithAuth(spiceboxv1alpha1.MCPServerAuth{
				Credential: "github-token",
				Provider:   "github-pat", // catalog id → Title "GitHub"
			}),
			ac:               buildExplanationAC("cls", "Bot"),
			missing:          []string{"github-token"},
			wantCredential:   "github-token",
			wantTitle:        "GitHub",
			wantDescNonEmpty: true, // catalog supplies a description
			wantWhy:          "",
		},
		{
			name: "no Title, unknown provider: humanized credential name, empty Why",
			mcpServer: mcpServerWithAuth(spiceboxv1alpha1.MCPServerAuth{
				Credential: "bare-cred",
				Provider:   "SomeService", // not a catalog id
			}),
			ac:               buildExplanationAC("cls", "Bot"),
			missing:          []string{"bare-cred"},
			wantCredential:   "bare-cred",
			wantTitle:        "Bare Cred",
			wantDescNonEmpty: false,
			wantWhy:          "",
		},
		{
			name: "credential not referenced by the class: humanized name, empty Why",
			mcpServer: mcpServerWithAuth(spiceboxv1alpha1.MCPServerAuth{
				Credential: "linear-oauth",
				Title:      "Linear",
			}),
			ac:               buildExplanationAC("cls", "Bot"),
			missing:          []string{"mystery-cred"},
			wantCredential:   "mystery-cred",
			wantTitle:        "Mystery Cred",
			wantDescNonEmpty: false,
			wantWhy:          "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testfixtures.NewScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.mcpServer).Build()

			expl, err := BuildExplanation(ctx, c, tc.ac, tc.missing)
			require.NoError(t, err)
			require.NotNil(t, expl)
			require.Len(t, expl.Items, 1)

			it := expl.Items[0]
			assert.Equal(t, tc.wantCredential, it.Credential, "Credential")
			assert.Equal(t, tc.wantTitle, it.Title, "Title")
			if tc.wantDescNonEmpty {
				assert.NotEmpty(t, it.Description, "Description")
			} else {
				assert.Empty(t, it.Description, "Description")
			}
			assert.Equal(t, tc.wantWhy, it.Why, "Why")
		})
	}
}

// TestBuildExplanationEmptyMissing verifies the empty-missing case yields
// an empty (non-nil) Items slice.
func TestBuildExplanationEmptyMissing(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "EmptyBot"},
	}

	expl, err := BuildExplanation(ctx, c, ac, nil)
	require.NoError(t, err)
	require.NotNil(t, expl)
	assert.Empty(t, expl.Items, "no missing credentials ⇒ no items")
}

// TestBuildExplanationEnterpriseSentinel verifies the enterprise sign-in
// sentinel gets a clean Title rather than being humanized.
func TestBuildExplanationEnterpriseSentinel(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{DisplayName: "Bot"},
	}

	expl, err := BuildExplanation(ctx, c, ac, []string{enterpriseSignInSentinel})
	require.NoError(t, err)
	require.Len(t, expl.Items, 1)
	assert.Equal(t, enterpriseSignInSentinel, expl.Items[0].Credential)
	assert.Equal(t, "Enterprise sign-in", expl.Items[0].Title,
		"sentinel must get a clean label, not a humanized one")
	assert.Empty(t, expl.Items[0].Description)
}
