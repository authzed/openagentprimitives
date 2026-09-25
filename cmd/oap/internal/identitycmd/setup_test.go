package identitycmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"

	// Wire authkinds + builtin flows (may already be wired via blank imports
	// in agent_setup_identity_test.go in the same package; idempotent here).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/loader"
)

// TestIdentitySetupRequiresFlags asserts that calling setup without
// --toolkits or --mcps returns the documented hard error.
func TestIdentitySetupRequiresFlags(t *testing.T) {
	c := aptest.IdentityClientBuilder(t).Build()

	err := runIdentitySetup(context.Background(), io.Discard, io.Discard,
		strings.NewReader(""), c, "default", "my-bot",
		nil, nil, false, SetupOptions{})
	require.Error(t, err, "missing --toolkits/--mcps should error")
	assert.Contains(t, err.Error(), "setup requires --toolkits and/or --mcps", "error mentions required flags")
}

// TestIdentitySetupRequiresFlags_CobraShim verifies the cobra layer enforces
// the same check (ensures the RunE wrapper passes through).
func TestIdentitySetupRequiresFlags_CobraShim(t *testing.T) {
	out, err := runIdentity(t, "setup", "my-bot")
	assert.Errorf(t, err, "expected error; got output: %s", out)
}

// TestIdentitySetupHappyPath drives the github-pat builtin via --toolkits gh,
// injects a valid token through stdin, and asserts that an AgentIdentity +
// Secret are created in the fake cluster.
func TestIdentitySetupHappyPath(t *testing.T) {
	// Prevent the builtin from opening a real browser.
	browsertest.Record(t)

	// Stub the live-verification probe so the fixture ghp_ token below
	// doesn't trigger a real GET api.github.com/user (which would 401 and
	// consume the single-line stdin via the "Store it anyway?" prompt).
	aptest.InstallVerifyHTTPStub(t)

	// Ensure the github-pat flow is registered (loader import above handles
	// this, but guard against cross-test Reset() calls).
	if _, ok := builtins.Get("github-pat"); !ok {
		builtins.Register(github_pat.New())
		t.Cleanup(func() { builtins.Reset() })
	}

	c := aptest.IdentityClientBuilder(t).Build()

	// Feed a valid ghp_ token on stdin (>=36 chars after prefix).
	stdin := strings.NewReader("ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF\n")
	var out strings.Builder

	require.NoErrorf(t,
		runIdentitySetup(context.Background(), &out, io.Discard,
			stdin, c, "default", "my-bot",
			[]string{"gh"}, nil, false, SetupOptions{}),
		"runIdentitySetup; output: %s", out.String())

	// AgentIdentity should be created.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai),
		"get AgentIdentity")
	assert.NotEmptyf(t, ai.Spec.Credentials, "expected credentials; output:\n%s", out.String())
}

// TestIdentitySetupIdempotent verifies that running setup twice does not
// duplicate credentials — the second run skips the already-set-up credential.
func TestIdentitySetupIdempotent(t *testing.T) {
	browsertest.Record(t)

	// Stub the live-verification probe so the fixture ghp_ token below
	// doesn't trigger a real GET api.github.com/user (which would 401 and
	// consume the single-line stdin via the "Store it anyway?" prompt).
	aptest.InstallVerifyHTTPStub(t)

	if _, ok := builtins.Get("github-pat"); !ok {
		builtins.Register(github_pat.New())
		t.Cleanup(func() { builtins.Reset() })
	}

	c := aptest.IdentityClientBuilder(t).Build()

	runOnce := func() error {
		stdin := strings.NewReader("ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF\n")
		return runIdentitySetup(context.Background(), io.Discard, io.Discard,
			stdin, c, "default", "my-bot", []string{"gh"}, nil, false, SetupOptions{})
	}

	require.NoError(t, runOnce(), "first run")
	require.NoError(t, runOnce(), "second run")

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai),
		"get AgentIdentity")
	assert.Len(t, ai.Spec.Credentials, 1, "expected exactly 1 credential after 2 runs")
}

// TestIdentitySetupMCPsFlag verifies that --mcps entries are translated to
// "mcp:" targets. Since there is no MCPServer CR in the fake cluster, the
// engine returns an error (ErrTargetNotFound), which is the expected behavior.
func TestIdentitySetupMCPsFlag(t *testing.T) {
	c := aptest.IdentityClientBuilder(t).Build()

	err := runIdentitySetup(context.Background(), io.Discard, io.Discard,
		strings.NewReader(""), c, "default", "my-bot",
		nil, []string{"linear-readonly"}, false, SetupOptions{})
	require.Error(t, err, "missing MCPServer should error")
	assert.Contains(t, err.Error(), "linear-readonly", "error should mention 'linear-readonly'")
}
