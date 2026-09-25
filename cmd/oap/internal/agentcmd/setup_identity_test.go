package agentcmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"

	// Wire all authkinds + all builtin flows.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"

	// Registers the static/oauth/federated credkind.Kinds so
	// setup.Store's credkindregistry.Get(cred.Type) dispatch resolves in
	// this package's tests (setup.Store is exercised end-to-end via
	// runAgentSetupIdentity below).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// makeAgentSetupClass builds a minimal AgentClass with the given toolspec
// names and MCP server refs, plus an agentIdentity reference.
func makeAgentSetupClass(ns, name, identityName string, toolspecs []string, mcpRefs []string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Description:   "test agent for GitHub PR review",
			AgentIdentity: identityName,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet-20241022",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "key", Key: "k"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are helpful"},
			Budget:       &spiceboxv1alpha1.BudgetConfig{MaxTurns: 10},
		},
	}
	if len(toolspecs) > 0 {
		ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
			{Name: "tools", Class: "default", Toolspecs: toolspecs},
		}
	}
	for _, ref := range mcpRefs {
		ac.Spec.MCPServers = append(ac.Spec.MCPServers, spiceboxv1alpha1.AgentClassMCPServerRef{
			Name: ref, Ref: ref,
		})
	}
	return ac
}

// makeGhToolspec builds a cluster-scoped SpiceboxToolspec that references the
// embedded "gh" toolkit. Used to drive the toolspec authkind → cli authkind
// → github-pat builtin path.
func makeGhToolspec(name string) *spiceboxv1alpha1.SpiceboxToolspec {
	return &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:    name,
			Intent:  "read GitHub pull requests",
			Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "gh", Revision: "2026-05-01"},
		},
	}
}

// TestAgentSetupIdentity_NotFound returns an error when the AgentClass is
// missing.
func TestAgentSetupIdentity_NotFound(t *testing.T) {
	c := aptest.IdentityClientBuilder(t).Build()
	err := runAgentSetupIdentity(context.Background(), io.Discard, io.Discard,
		strings.NewReader(""), c, "default", "nonexistent", "", "", false, identitycmd.SetupOptions{})
	require.Error(t, err, "missing AgentClass should error")
	assert.Contains(t, err.Error(), "get AgentClass", "error should mention 'get AgentClass'")
}

// TestAgentSetupIdentity_NoIdentity returns an error when neither
// spec.agentIdentity nor --identity is given.
func TestAgentSetupIdentity_NoIdentity(t *testing.T) {
	ac := makeAgentSetupClass("default", "my-class", "", nil, nil)
	ac.Spec.AgentIdentity = "" // explicitly clear
	c := aptest.IdentityClientBuilder(t).WithObjects(ac).Build()

	err := runAgentSetupIdentity(context.Background(), io.Discard, io.Discard,
		strings.NewReader(""), c, "default", "my-class", "", "", false, identitycmd.SetupOptions{})
	require.Error(t, err, "missing agentIdentity should error")
	assert.Contains(t, err.Error(), "no spec.agentIdentity", "error should mention 'no spec.agentIdentity'")
}

// TestAgentSetupIdentity_IdentityOverride verifies that --identity overrides
// the spec field.
func TestAgentSetupIdentity_IdentityOverride(t *testing.T) {
	// AgentClass with no spec.agentIdentity but --identity provided.
	ac := makeAgentSetupClass("default", "my-class", "", nil, nil)
	ac.Spec.AgentIdentity = ""
	c := aptest.IdentityClientBuilder(t).WithObjects(ac).Build()

	var out strings.Builder
	// No toolspecs/MCPs → "nothing to set up" early return.
	require.NoError(t,
		runAgentSetupIdentity(context.Background(), &out, io.Discard,
			strings.NewReader(""), c, "default", "my-class", "override-identity", "", false, identitycmd.SetupOptions{}),
		"runAgentSetupIdentity")
	assert.Contains(t, out.String(), "nothing to set up", "output should report nothing to set up")
}

// TestAgentSetupIdentity_NothingToSetUp covers the case where the AgentClass
// has no toolspecs or MCPServers.
func TestAgentSetupIdentity_NothingToSetUp(t *testing.T) {
	ac := makeAgentSetupClass("default", "my-class", "my-bot", nil, nil)
	c := aptest.IdentityClientBuilder(t).WithObjects(ac).Build()

	var out strings.Builder
	require.NoError(t,
		runAgentSetupIdentity(context.Background(), &out, io.Discard,
			strings.NewReader(""), c, "default", "my-class", "", "", false, identitycmd.SetupOptions{}),
		"runAgentSetupIdentity")
	assert.Contains(t, out.String(), "nothing to set up", "output should report nothing to set up")
}

// TestAgentSetupIdentity_HappyPath builds an AgentClass with a single
// toolspec that references the embedded "gh" toolkit. The github-pat builtin
// is invoked via the toolspec → cli authkind path. We inject a valid-shape
// token via the stdin reader and assert an AgentIdentity + Secret are created.
func TestAgentSetupIdentity_HappyPath(t *testing.T) {
	// Prevent the builtin from opening a real browser.
	browsertest.Record(t)

	// Stub the live-verification probe so the fixture ghp_ token below
	// doesn't trigger a real GET api.github.com/user (which would 401 and
	// consume the single-line stdin via the "Store it anyway?" prompt).
	aptest.InstallVerifyHTTPStub(t)

	// We need the github-pat flow registered. The loader blank-import at the
	// top of this file registers all builtins once, but engine_test may have
	// called builtins.Reset(). Re-register to be safe.
	if _, ok := builtins.Get("github-pat"); !ok {
		builtins.Register(github_pat.New())
		t.Cleanup(func() { builtins.Reset() })
	}

	ts := makeGhToolspec("gh-readonly")
	ac := makeAgentSetupClass("default", "my-class", "my-bot", []string{"gh-readonly"}, nil)

	c := aptest.IdentityClientBuilder(t).WithObjects(ac, ts).Build()

	// Feed a valid ghp_ token on stdin (must have >=36 chars after "ghp_").
	stdin := strings.NewReader("ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF\n")
	var out strings.Builder

	require.NoErrorf(t,
		runAgentSetupIdentity(context.Background(), &out, io.Discard,
			stdin, c, "default", "my-class", "", "", false, identitycmd.SetupOptions{}),
		"runAgentSetupIdentity; output: %s", out.String())

	// AgentIdentity should have been created.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai),
		"get AgentIdentity")
	assert.NotEmptyf(t, ai.Spec.Credentials, "expected credentials; output:\n%s", out.String())

	// Confirm the Secret exists.
	if len(ai.Spec.Credentials) > 0 && ai.Spec.Credentials[0].Static != nil {
		var sec corev1.Secret
		assert.NoError(t,
			c.Get(context.Background(), client.ObjectKey{
				Namespace: "default",
				Name:      ai.Spec.Credentials[0].Static.SecretRef.Name,
			}, &sec),
			"get Secret backing static credential")
	}
}

// TestAgentSetupIdentity_OnlyFilterSkipsNonMatching verifies that --only
// filters the target list so unmatched entries are skipped (no-op).
func TestAgentSetupIdentity_OnlyFilterSkipsNonMatching(t *testing.T) {
	browsertest.Record(t)

	ts := makeGhToolspec("gh-readonly")
	ac := makeAgentSetupClass("default", "my-class", "my-bot", []string{"gh-readonly"}, nil)
	c := aptest.IdentityClientBuilder(t).WithObjects(ac, ts).Build()

	var out strings.Builder
	// --only with a non-matching value → all targets filtered out → success with no credentials created.
	require.NoError(t,
		runAgentSetupIdentity(context.Background(), &out, io.Discard,
			strings.NewReader("ghp_xxxx\n"), c, "default", "my-class", "",
			"toolspec:does-not-exist", false, identitycmd.SetupOptions{}),
		"runAgentSetupIdentity")

	// No AgentIdentity should have been created (nothing was processed).
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai); err == nil {
		assert.Empty(t, ai.Spec.Credentials, "expected no credentials when --only filter matched nothing")
	}
}
