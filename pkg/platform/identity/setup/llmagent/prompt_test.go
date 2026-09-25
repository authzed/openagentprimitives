package llmagent_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent"
)

func TestBuildSystemPromptProvider(t *testing.T) {
	p := &provider.Provider{
		ID:      "github-pat",
		Prompt:  "go to settings",
		DocsURL: "https://example/docs",
		TokenShape: &provider.TokenShape{
			Pattern:     "^ghp_",
			Description: "github prefix",
		},
		RunShellAllowlist: []provider.ShellAllow{
			{Regex: `^gh auth token$`, Description: "read token"},
		},
	}
	got := llmagent.BuildSystemPrompt(p, "", "user wants read access",
		authkind.CredentialRequirement{IsBearer: false})

	for _, want := range []string{
		"You are a setup agent",
		"PROVIDER GUIDANCE",
		"go to settings",
		"DOCS: https://example/docs",
		"USER INTENT",
		"user wants read access",
		"TOKEN SHAPE: pattern=^ghp_",
		"RUN_SHELL ALLOWLIST",
		"^gh auth token$",
		"STORE_CREDENTIAL",
		"OAUTH CODE FLOWS",
		"STOP CONDITION",
	} {
		assert.Contains(t, got, want, "missing %q\nFULL:\n%s", want, got)
	}

	// Verify ordering: PROVIDER GUIDANCE before DOCS before USER INTENT
	// before TOKEN SHAPE before RUN_SHELL ALLOWLIST before STORE_CREDENTIAL
	// before STOP CONDITION.
	ordered := []string{
		"PROVIDER GUIDANCE", "DOCS:", "USER INTENT", "TOKEN SHAPE:",
		"RUN_SHELL ALLOWLIST", "STORE_CREDENTIAL", "OAUTH CODE FLOWS",
		"STOP CONDITION",
	}
	indices := make(map[string]int, len(ordered))
	for _, marker := range ordered {
		idx := strings.Index(got, marker)
		require.GreaterOrEqual(t, idx, 0, "marker %q not found in prompt", marker)
		indices[marker] = idx
	}
	for i := 1; i < len(ordered); i++ {
		assert.Less(t, indices[ordered[i-1]], indices[ordered[i]],
			"ordering violation: %q (pos %d) appears after %q (pos %d)",
			ordered[i-1], indices[ordered[i-1]], ordered[i], indices[ordered[i]])
	}
}

func TestBuildSystemPromptInlineFallback(t *testing.T) {
	got := llmagent.BuildSystemPrompt(nil, "see internal docs", "use the API key",
		authkind.CredentialRequirement{})
	assert.Contains(t, got, "TOOLKIT GUIDANCE", "inline-fallback content marker")
	assert.Contains(t, got, "see internal docs", "inline-fallback content body")
	assert.Contains(t, got, "run_shell is unavailable",
		"expected run_shell-unavailable note for inline-prompt path")
	// PROVIDER GUIDANCE should NOT appear for inline path.
	assert.NotContains(t, got, "PROVIDER GUIDANCE",
		"PROVIDER GUIDANCE must not appear in inline-prompt path")
}
