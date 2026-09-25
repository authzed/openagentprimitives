// pkg/platform/identity/passthrough/credentials_test.go
//
// White-box unit tests for Required + toolkitCredMetaForBundle —
// the resolution chain that turns AgentClass.toolBundles[].toolspecs[]
// (→ SpiceboxToolspec → SpiceboxToolkit) plus AgentClass.MCPServers
// into the deduplicated, remap-applied credential-name list every
// passthrough consumer (operator parker, identityd portal, slack Home,
// channelsd DM) shares.
package passthrough

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

// makeToolkit builds a SpiceboxToolkit CR with the given sensitive env
// vars. Each entry is a (name, declaredCredentialName) pair — pass an
// empty declaredCredentialName to exercise the verbatim-env-Name fallback.
// Non-sensitive env vars are also included via `nonSensitive` to verify
// they are filtered out.
func makeToolkit(name string, sensitive []spiceboxv1alpha1.ToolkitEnvVar, nonSensitive []spiceboxv1alpha1.ToolkitEnvVar) *spiceboxv1alpha1.SpiceboxToolkit {
	allowed := make([]spiceboxv1alpha1.ToolkitEnvVar, 0, len(sensitive)+len(nonSensitive))
	for _, e := range sensitive {
		e.Sensitive = true
		allowed = append(allowed, e)
	}
	for _, e := range nonSensitive {
		e.Sensitive = false
		allowed = append(allowed, e)
	}
	return &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            name,
			ToolkitRevision: "v1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/" + name},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "builtin", Name: name},
			Env:             spiceboxv1alpha1.ToolkitEnv{Allowed: allowed},
			Subcommands:     []spiceboxv1alpha1.ToolkitSubcommand{},
		},
	}
}

// makeToolspec builds a SpiceboxToolspec referencing the named toolkit.
func makeToolspec(name, toolkit string) *spiceboxv1alpha1.SpiceboxToolspec {
	return &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             name,
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkit, Revision: "v1"},
			AllowSubcommands: []string{},
		},
	}
}

// contribNames extracts the rawName from each contribution for name-only assertions.
func contribNames(cs []contribution) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.rawName)
	}
	return out
}

func TestToolkitCredMetaForBundle(t *testing.T) {
	ctx := context.Background()

	type seedFn func() []client.Object

	cases := []struct {
		name      string
		seed      seedFn
		bundle    spiceboxv1alpha1.ToolBundle
		wantNames []string
		wantErr   string
		// checkFirst asserts extra fields on the first contribution (optional).
		checkFirst func(t *testing.T, c contribution)
	}{
		{
			name: "single toolspec, one credential: returns one entry",
			seed: func() []client.Object {
				tk := makeToolkit("github-toolkit",
					[]spiceboxv1alpha1.ToolkitEnvVar{
						{Name: "GITHUB_TOKEN", Credential: "github-token", Title: "GitHub Token"},
					}, nil)
				ts := makeToolspec("gh-spec", "github-toolkit")
				return []client.Object{tk, ts}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "gh",
				Class:     "ignored-by-helper",
				Toolspecs: []string{"gh-spec"},
			},
			wantNames: []string{"github-token"},
			// Verify the explicit Title is carried through the contribution.
			checkFirst: func(t *testing.T, c contribution) {
				t.Helper()
				assert.Equal(t, "github-token", c.rawName)
				assert.Equal(t, "GitHub Token", c.title)
			},
		},
		{
			name: "multiple toolspecs, overlapping credentials: deduped",
			seed: func() []client.Object {
				tk1 := makeToolkit("gh-cli",
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "GITHUB_TOKEN", Credential: "github-token"}}, nil)
				tk2 := makeToolkit("gh-api",
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "GITHUB_TOKEN", Credential: "github-token"}}, nil)
				return []client.Object{
					tk1, tk2,
					makeToolspec("gh-cli-spec", "gh-cli"),
					makeToolspec("gh-api-spec", "gh-api"),
				}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "gh",
				Toolspecs: []string{"gh-cli-spec", "gh-api-spec"},
			},
			wantNames: []string{"github-token"},
		},
		{
			name: "empty-credential fallback: env Name used verbatim (no lowercasing/inference)",
			seed: func() []client.Object {
				tk := makeToolkit("anthropic-cli",
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "ANTHROPIC_API_KEY"}}, nil)
				return []client.Object{tk, makeToolspec("anth-spec", "anthropic-cli")}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "anth",
				Toolspecs: []string{"anth-spec"},
			},
			wantNames: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name: "non-sensitive env vars are filtered out",
			seed: func() []client.Object {
				tk := makeToolkit("docker-cli",
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "REGISTRY_TOKEN", Credential: "registry-token"}},
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "PATH"}, {Name: "HOME"}})
				return []client.Object{tk, makeToolspec("docker-spec", "docker-cli")}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "docker",
				Toolspecs: []string{"docker-spec"},
			},
			wantNames: []string{"registry-token"},
		},
		{
			name: "missing toolspec (dangling ref inside the bundle): error names the toolspec",
			seed: func() []client.Object { return []client.Object{} },
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "bad",
				Toolspecs: []string{"missing-ts"},
			},
			wantErr: "missing-ts",
		},
		{
			name: "missing toolkit (dangling ref from the toolspec): error names the toolkit",
			seed: func() []client.Object {
				return []client.Object{makeToolspec("ts-without-tk", "no-such-toolkit")}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "bad",
				Toolspecs: []string{"ts-without-tk"},
			},
			wantErr: "no-such-toolkit",
		},
		{
			name: "no toolspecs declare credentials: clean (nil, nil)",
			seed: func() []client.Object {
				tk := makeToolkit("plain-cli", nil,
					[]spiceboxv1alpha1.ToolkitEnvVar{{Name: "PATH"}})
				return []client.Object{tk, makeToolspec("plain-spec", "plain-cli")}
			},
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "plain",
				Toolspecs: []string{"plain-spec"},
			},
			wantNames: nil,
		},
		{
			name: "bundle with no toolspecs listed: clean (nil, nil)",
			seed: func() []client.Object { return nil },
			bundle: spiceboxv1alpha1.ToolBundle{
				Name:      "empty",
				Toolspecs: nil,
			},
			wantNames: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := tc.seed()
			c := fake.NewClientBuilder().
				WithScheme(testfixtures.NewScheme(t)).
				WithObjects(objs...).
				Build()

			got, err := toolkitCredMetaForBundle(ctx, c, tc.bundle)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.wantNames, contribNames(got))
			if tc.checkFirst != nil && len(got) > 0 {
				tc.checkFirst(t, got[0])
			}
		})
	}
}

// TestRequired_Toolkit_RemapAndUnion exercises toolkitCredMetaForBundle
// via its caller Required to verify end-to-end behavior:
//   - per-bundle CredentialRemap applies AFTER credentials are extracted
//     (so two raw names can collapse to one final name);
//   - MCP-required + toolkit-required credentials are unioned + sorted.
func TestRequired_Toolkit_RemapAndUnion(t *testing.T) {
	ctx := context.Background()

	tk := makeToolkit("gh-toolkit",
		[]spiceboxv1alpha1.ToolkitEnvVar{
			{Name: "GH_READ_TOKEN", Credential: "gh-read"},
			{Name: "GH_WRITE_TOKEN", Credential: "gh-write"},
		}, nil)
	ts := makeToolspec("gh-spec", "gh-toolkit")
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "linear",
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "linear-oauth"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(tk, ts, srv).
		Build()

	t.Run("union of MCP + toolkit credentials, sorted", func(t *testing.T) {
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
					{Name: "linear", Ref: "linear"},
				},
				ToolBundles: []spiceboxv1alpha1.ToolBundle{
					{Name: "gh", Class: "x", Toolspecs: []string{"gh-spec"}},
				},
			},
		}
		got, err := Required(ctx, c, ac)
		require.NoError(t, err)
		assert.Equal(t, []string{"gh-read", "gh-write", "linear-oauth"}, got)
	})

	t.Run("CredentialRemap on bundle applies before dedup: two raw names collapse to one", func(t *testing.T) {
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				ToolBundles: []spiceboxv1alpha1.ToolBundle{
					{
						Name: "gh", Class: "x",
						Toolspecs: []string{"gh-spec"},
						CredentialRemap: map[string]string{
							"gh-read":  "gh-token",
							"gh-write": "gh-token",
						},
					},
				},
			},
		}
		got, err := Required(ctx, c, ac)
		require.NoError(t, err)
		assert.Equal(t, []string{"gh-token"}, got)
	})

	t.Run("zero ToolBundles + zero MCPServers: clean empty result", func(t *testing.T) {
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		}
		got, err := Required(ctx, c, ac)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("dangling bundle toolspec ref: error wraps bundle name + toolspec name", func(t *testing.T) {
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				ToolBundles: []spiceboxv1alpha1.ToolBundle{
					{Name: "broken", Class: "x", Toolspecs: []string{"missing-ts"}},
				},
			},
		}
		_, err := Required(ctx, c, ac)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "broken")
		assert.Contains(t, err.Error(), "missing-ts")
	})
}

// TestRequired_SkipsFederatedMCPServers verifies that a federated MCPServer
// does not contribute a user-linked credential name to Required's output.
func TestRequired_SkipsFederatedMCPServers(t *testing.T) {
	ctx := context.Background()

	federated := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear-fed", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:   "linear-fed",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.linear.app"},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:     "federated",
				Resource: "https://linear.app",
			},
		},
	}
	static := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github",
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "github-pat"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(federated, static).
		Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "linear-fed", Ref: "linear-fed"},
				{Name: "github", Ref: "github"},
			},
		},
	}

	got, err := Required(ctx, c, ac)
	require.NoError(t, err)
	// Only the static credential appears; the federated server is skipped.
	assert.Equal(t, []string{"github-pat"}, got)
}

// TestRequiredFederated_AppliesCredentialRemap verifies that RequiredFederated
// applies ref.CredentialRemap to the synthesized credential name so it matches
// the name credresolve.Descriptors looks up at runtime.
func TestRequiredFederated_AppliesCredentialRemap(t *testing.T) {
	ctx := context.Background()

	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear-fed", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:   "linear-fed",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.linear.app"},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "federated",
				Resource:   "https://linear.app",
				Credential: "linear-raw",
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(srv).
		Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{
					Name: "linear-fed", Ref: "linear-fed",
					CredentialRemap: map[string]string{"linear-raw": "linear-remapped"},
				},
			},
		},
	}

	got, err := RequiredFederated(ctx, c, ac)
	require.NoError(t, err)
	require.Len(t, got, 1)
	// CredentialName must be the REMAPPED name so it matches what
	// credresolve.Descriptors looks up at runtime.
	assert.Equal(t, "linear-remapped", got[0].CredentialName, "CredentialRemap must be applied")
	assert.Equal(t, "https://linear.app", got[0].Resource)
	assert.Equal(t, "https://mcp.linear.app", got[0].ResourceServerURL)
}

// TestRequiredFederated verifies that RequiredFederated returns one
// FederatedTarget per federated MCPServer and skips non-federated ones.
func TestRequiredFederated(t *testing.T) {
	ctx := context.Background()

	federated := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear-fed", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:   "linear-fed",
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://mcp.linear.app"},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "federated",
				Resource:   "https://linear.app",
				Credential: "linear",
			},
		},
	}
	static := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "github",
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "github-pat"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(federated, static).
		Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "linear-fed", Ref: "linear-fed"},
				{Name: "github", Ref: "github"},
			},
		},
	}

	got, err := RequiredFederated(ctx, c, ac)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "linear", got[0].CredentialName)
	assert.Equal(t, "https://linear.app", got[0].Resource)
	assert.Equal(t, "https://mcp.linear.app", got[0].ResourceServerURL)
}

// TestUnauthenticated pins the ONE predicate Resolve and validate_spec
// (pkg/tools/workshopmcp) share: a server whose auth stanza names no
// credential a person could ever be asked to link. The two must not drift —
// validate_spec's refusal is only correct while it names exactly the shape
// this resolver drops.
func TestUnauthenticated(t *testing.T) {
	cases := []struct {
		name string
		auth spiceboxv1alpha1.MCPServerAuth
		want bool
	}{
		{name: "oauth naming neither field: unauthenticated", auth: spiceboxv1alpha1.MCPServerAuth{Type: "oauth"}, want: true},
		{name: "static naming neither field: unauthenticated", auth: spiceboxv1alpha1.MCPServerAuth{Type: "static"}, want: true},
		{name: "no type and neither field: unauthenticated", auth: spiceboxv1alpha1.MCPServerAuth{}, want: true},
		{name: "oauth naming a credential: authenticated", auth: spiceboxv1alpha1.MCPServerAuth{Type: "oauth", Credential: "demo-token"}, want: false},
		{name: "oauth naming a provider: authenticated", auth: spiceboxv1alpha1.MCPServerAuth{Type: "oauth", Provider: "oauth-mcp"}, want: false},
		{name: "federated naming neither field: not unauthenticated, its credential is minted", auth: spiceboxv1alpha1.MCPServerAuth{Type: "federated"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Unauthenticated(&spiceboxv1alpha1.MCPServerSpec{Auth: tc.auth}))
		})
	}
	assert.False(t, Unauthenticated(nil), "a nil spec declares nothing and is not a finding")
}

// TestResolve_SkipsExactlyTheUnauthenticatedShape is the anti-drift half: the
// servers Resolve contributes nothing for are exactly the ones Unauthenticated
// reports true for, plus federated (skipped by its own rule, one line above).
func TestResolve_SkipsExactlyTheUnauthenticatedShape(t *testing.T) {
	ctx := context.Background()

	unauth := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-connector", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "demo-connector",
			Auth: spiceboxv1alpha1.MCPServerAuth{Type: "oauth"},
		},
	}
	linked := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-tracker", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "demo-tracker",
			Auth: spiceboxv1alpha1.MCPServerAuth{Type: "static", Credential: "demo-tracker-token"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(unauth, linked).
		Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "demo-connector", Ref: "demo-connector"},
				{Name: "demo-tracker", Ref: "demo-tracker"},
			},
		},
	}

	got, err := Required(ctx, c, ac)
	require.NoError(t, err)
	assert.Equal(t, []string{"demo-tracker-token"}, got,
		"the unauthenticated server contributes nothing — nobody is ever asked to link it")
	assert.True(t, Unauthenticated(&unauth.Spec), "the predicate must name the shape Resolve dropped")
	assert.False(t, Unauthenticated(&linked.Spec), "and must not name the one it kept")
}
