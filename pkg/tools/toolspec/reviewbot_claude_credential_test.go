package toolspec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
)

// reviewbotAnthropicCredentialName is a hand-maintained PARALLEL COPY of the
// AgentCredential name declared for the agent-owned Anthropic key on
// examples/reviewbot/manifests/agentidentity.yaml ("anthropic-api-key"). It
// exists here, as a Go constant, only because no test in this repo may
// reference anything under examples/ (AGENTS.md is explicit about that) — see
// ghReviewEndpointCEL above for the established precedent this follows.
//
// The two copies are kept in sync BY HAND. What this test does NOT have to
// hand-maintain is the OTHER half of the agreement — which credential NAME
// the "claude" toolkit's ANTHROPIC_API_KEY env var maps to — because that
// half is derived below from the real, shared toolkit definition
// (toolkits/claude.yaml, loaded through the same authkind/cli production code
// path the identity-linking flow uses), not retyped as a second literal.
const reviewbotAnthropicCredentialName = "anthropic-api-key"

// TestClaudeToolspec_SensitiveEnvAgreesWithTheAgentOwnedCredential proves the
// claim in toolspecs.yaml's header comment: reviewbot's `claude` toolspec's
// sensitive env (ANTHROPIC_API_KEY) and the credential name
// examples/reviewbot/manifests/agentidentity.yaml declares
// ("anthropic-api-key") actually agree — deriving the ENV-VAR-TO-CREDENTIAL-
// NAME mapping from the real "claude" toolkit rather than hardcoding it twice.
// A drift here (e.g. someone renaming the credential in agentidentity.yaml,
// or the claude toolkit's declared credential name, without updating the
// other) would silently break credential injection: the toolkit would ask for
// a credential the identity does not have, and Descriptors would fail at
// session-start with ErrCredentialMissing — this test exists so that
// disagreement is caught here instead.
func TestClaudeToolspec_SensitiveEnvAgreesWithTheAgentOwnedCredential(t *testing.T) {
	spec := loadToolspec(t, "claude")
	require.Equal(t, []string{"ANTHROPIC_API_KEY"}, spec.Spec.Sensitive.Env,
		"the claude toolspec's declared sensitive env must be exactly what reviewbot's agentidentity.yaml supplies")

	// Resolve the REAL "claude" toolkit through the same production code path
	// authkind/cli uses to compute setup requirements — nil client sends it to
	// the embedded /toolkits/ catalog (no cluster needed), the same fallback a
	// fresh install with no SpiceboxToolkit CR override would take.
	ctx := context.Background()
	tgt, err := cli.New().ResolveTarget(ctx, nil, "", "claude")
	require.NoError(t, err, `resolve the embedded "claude" toolkit`)
	reqs := cli.New().SetupRequirements(ctx, tgt)
	require.NotEmpty(t, reqs, `the "claude" toolkit must declare at least one sensitive env credential requirement`)

	var found bool
	for _, r := range reqs {
		if _, ok := r.BindingEnv["ANTHROPIC_API_KEY"]; !ok {
			continue
		}
		found = true
		assert.Equal(t, reviewbotAnthropicCredentialName, r.SuggestedName,
			"the claude toolkit's ANTHROPIC_API_KEY requirement must name the same credential reviewbot's agentidentity.yaml declares")
	}
	assert.True(t, found, `the "claude" toolkit did not declare a requirement for ANTHROPIC_API_KEY — the toolspec's sensitive.env would then name an env var no credential resolution covers`)
}
