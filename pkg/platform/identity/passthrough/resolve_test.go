package passthrough

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newClient builds a fake controller-runtime client pre-loaded with objs.
func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		Build()
}

func reqByName(reqs []CredRequirement, name string) (CredRequirement, bool) {
	for _, r := range reqs {
		if r.Name == name {
			return r, true
		}
	}
	return CredRequirement{}, false
}

// Object-meta helpers — type alias keeps test field literals readable.
type metaObjectMeta = metav1.ObjectMeta

func objMeta(name, ns string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: ns} }
func metaName(n string) metaObjectMeta          { return objMeta(n, "") }
func metaNameNS(n, ns string) metaObjectMeta    { return objMeta(n, ns) }

func toolspecFor(name, toolkit string) *spiceboxv1alpha1.SpiceboxToolspec {
	return &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spiceboxv1alpha1.SpiceboxToolspecSpec{Name: name, Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkit}},
	}
}

func TestResolve(t *testing.T) {
	// A toolkit whose sensitive env var declares an explicit Title +
	// Description (tool field wins over everything).
	tkExplicit := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metaName("tk-explicit"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "tk-explicit",
			Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
				{Name: "X_TOKEN", Title: "Explicit Svc", Description: "explicit desc", Sensitive: true, Credential: "x-token"},
			}},
		},
	}
	tsExplicit := toolspecFor("ts-explicit", "tk-explicit")

	// A toolkit whose sensitive env var has only a Provider → catalog
	// supplies Title/Description.
	tkCatalog := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metaName("tk-catalog"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "tk-catalog",
			Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
				{Name: "GH_TOKEN", Sensitive: true, Provider: "github-pat", Credential: "github-token"},
			}},
		},
	}
	tsCatalog := toolspecFor("ts-catalog", "tk-catalog")

	// A toolkit whose sensitive env var has neither title nor provider →
	// humanized credential name as title, empty description.
	tkBare := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metaName("tk-bare"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: "tk-bare",
			Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
				{Name: "BARE_TOKEN", Sensitive: true, Credential: "bare-token"},
			}},
		},
	}
	tsBare := toolspecFor("ts-bare", "tk-bare")

	c := newClient(t, tkExplicit, tsExplicit, tkCatalog, tsCatalog, tkBare, tsBare)

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metaNameNS("ac", "default"),
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "b", Toolspecs: []string{"ts-explicit", "ts-catalog", "ts-bare"}},
			},
		},
	}

	reqs, err := Resolve(context.Background(), c, ac)
	require.NoError(t, err)

	t.Run("tool field wins: Title+Description from env var", func(t *testing.T) {
		r, ok := reqByName(reqs, "x-token")
		require.True(t, ok)
		assert.Equal(t, "Explicit Svc", r.Title)
		assert.Equal(t, "explicit desc", r.Description)
	})
	t.Run("catalog fallback: Title+Description from provider", func(t *testing.T) {
		r, ok := reqByName(reqs, "github-token")
		require.True(t, ok)
		assert.Equal(t, "GitHub", r.Title)
		assert.NotEmpty(t, r.Description)
		assert.Equal(t, "github-pat", r.ProviderID)
	})
	t.Run("humanize fallback: Title from cred name, empty Description", func(t *testing.T) {
		r, ok := reqByName(reqs, "bare-token")
		require.True(t, ok)
		assert.Equal(t, "Bare Token", r.Title)
		assert.Empty(t, r.Description)
	})

	t.Run("Required is the name projection of Resolve", func(t *testing.T) {
		names, err := Required(context.Background(), c, ac)
		require.NoError(t, err)
		assert.Equal(t, []string{"bare-token", "github-token", "x-token"}, names)
	})
}

func TestResolveRemapCollisionPrefersExactMatch(t *testing.T) {
	// git declares GIT_TOKEN→git-token (no provider); gh declares
	// GITHUB_TOKEN→github-token (provider github-pat). The bundle remaps
	// git-token→github-token. Both contribute to final github-token; the
	// exact-match (gh, declared==final) must win, giving Title "GitHub".
	tkGit := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metaName("git"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{Name: "git", Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
			{Name: "GIT_TOKEN", Sensitive: true, Credential: "git-token", Description: "raw git token"},
		}}},
	}
	tkGh := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metaName("gh"),
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{Name: "gh", Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
			{Name: "GITHUB_TOKEN", Sensitive: true, Provider: "github-pat", Credential: "github-token"},
		}}},
	}
	c := newClient(t, tkGit, toolspecFor("git-rw", "git"), tkGh, toolspecFor("gh-pr", "gh"))
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metaNameNS("ac", "default"),
		Spec: spiceboxv1alpha1.AgentClassSpec{ToolBundles: []spiceboxv1alpha1.ToolBundle{
			{Name: "gitlike", Toolspecs: []string{"git-rw", "gh-pr"}, CredentialRemap: map[string]string{"git-token": "github-token"}},
		}},
	}
	reqs, err := Resolve(context.Background(), c, ac)
	require.NoError(t, err)
	r, ok := reqByName(reqs, "github-token")
	require.True(t, ok)
	assert.Equal(t, "GitHub", r.Title)
	require.Len(t, reqs, 1, "git-token must collapse into github-token")
}

func TestHumanizeCredName(t *testing.T) {
	assert.Equal(t, "Github Token", HumanizeCredName("github-token"))
	assert.Equal(t, "Anthropic Oauth", HumanizeCredName("anthropic_oauth"))
	assert.Equal(t, "your account", HumanizeCredName(""))
}

func TestResolveBestEffort_SkipsDanglingRef(t *testing.T) {
	// A bundle with a dangling toolspec ref — best-effort logs and skips it,
	// returning an empty (non-error) result.
	c := newClient(t) // no objects seeded
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metaNameNS("ac", "default"),
		Spec: spiceboxv1alpha1.AgentClassSpec{
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "broken", Toolspecs: []string{"missing-ts"}},
			},
		},
	}
	reqs := ResolveBestEffort(context.Background(), c, ac, logr.Discard())
	assert.Empty(t, reqs)
}
