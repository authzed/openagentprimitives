package toolkits_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

func TestAll_HasCoreSet(t *testing.T) {
	tks := toolkits.All()
	assert.GreaterOrEqual(t, len(tks), 6, "at least 6 built-in toolkits")
	found := map[string]bool{}
	for _, tk := range tks {
		found[tk.Name] = true
	}
	for _, name := range []string{"cat", "echo", "git", "docker", "gh", "kubectl"} {
		assert.Truef(t, found[name], "built-in %q present", name)
	}
}

func TestAll_HaveRevisions(t *testing.T) {
	for _, tk := range toolkits.All() {
		assert.NotEmptyf(t, tk.ToolkitRevision, "toolkit %q has non-empty toolkitRevision", tk.Name)
	}
}

func TestRawYAML_KnownName(t *testing.T) {
	data, ok := toolkits.RawYAML("gh")
	assert.True(t, ok, "RawYAML(gh) ok")
	assert.NotEmpty(t, data, "RawYAML(gh) bytes")
}

func TestRawYAML_UnknownName(t *testing.T) {
	_, ok := toolkits.RawYAML("nope")
	assert.False(t, ok, "RawYAML(nope) must not be ok")
}

func TestGitToolkit_AllowsHardeningEnv(t *testing.T) {
	var git *toolkit.Toolkit
	for i := range toolkits.All() {
		tk := toolkits.All()[i]
		if tk.Name == "git" {
			git = &tk
		}
	}
	if git == nil {
		t.Fatal("git toolkit not present")
	}
	allowed := map[string]bool{}
	for _, e := range git.Env.Allowed {
		allowed[e.Name] = true
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_TERMINAL_PROMPT"} {
		assert.Truef(t, allowed[want],
			"git toolkit must allow %q in env.allowed (have %v)", want, keysOf(allowed))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestClaudeOAuthToolkit(t *testing.T) {
	var tk *toolkit.Toolkit
	for i := range toolkits.All() {
		if toolkits.All()[i].Name == "claude-oauth" {
			tk = &toolkits.All()[i]
		}
	}
	if tk == nil {
		t.Fatal("claude-oauth toolkit must be embedded")
	}

	var found bool
	for _, e := range tk.Env.Allowed {
		if e.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
			found = true
			assert.True(t, e.Sensitive)
			assert.Equal(t, "anthropic-oauth", e.Provider)
			assert.Equal(t, "anthropic-oauth", e.Credential)
		}
	}
	assert.True(t, found, "claude-oauth must declare CLAUDE_CODE_OAUTH_TOKEN")
}

// TestClaudeToolkits_CapClaudeCodeRetries pins the hardening for the defect
// where a permanently-invalid Claude credential cost ~3 minutes per tool call:
// the Claude Code CLI treats HTTP 401 as retryable and defaults to ten retries
// with exponential backoff, so every attempt on a dead credential burned the
// full budget before the agent's turn loop simply called it again. The knob is
// env-only — the shipped binary has no --max-retries flag — so the toolkit that
// describes the CLI is where the cap is declared.
func TestClaudeToolkits_CapClaudeCodeRetries(t *testing.T) {
	for _, name := range []string{"claude", "claude-oauth"} {
		t.Run(name+": envDefaults caps CLAUDE_CODE_MAX_RETRIES", func(t *testing.T) {
			var tk *toolkit.Toolkit
			for i := range toolkits.All() {
				if toolkits.All()[i].Name == name {
					tk = &toolkits.All()[i]
				}
			}
			require.NotNilf(t, tk, "%q toolkit must be embedded", name)
			assert.Equal(t, "1", tk.EnvDefaults["CLAUDE_CODE_MAX_RETRIES"],
				"%q must cap the CLI's own retry loop (have %v)", name, tk.EnvDefaults)
		})
	}
}
