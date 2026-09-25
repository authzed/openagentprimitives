package llmagent_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

// toolUseResp builds a single-tool-use llm.Response for scripted fake steps.
func toolUseResp(t *testing.T, id, name string, args interface{}) llm.Response {
	t.Helper()
	input, err := json.Marshal(args)
	require.NoError(t, err, "toolUseResp: marshal args")
	return llm.Response{
		Content: []llm.ContentBlock{
			{
				Type: "tool_use",
				ToolUse: &llm.ToolUseBlock{
					ID:    id,
					Name:  name,
					Input: json.RawMessage(input),
				},
			},
		},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
	}
}

// installFakeLLM swaps in a scripted fake LLM provider for the duration
// of the test. Restoration happens via t.Cleanup.
func installFakeLLM(t *testing.T, script []llmfake.Step) {
	t.Helper()
	fakeProv := llmfake.New(script)
	llmagent.LLMProviderFactory = func(_ context.Context) (llm.Provider, error) {
		return fakeProv, nil
	}
	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })
}

// stubBrowserOpener makes the agent's open_browser tool succeed without
// launching anything.
//
// browser.Open would decline on its own here (it refuses to reach the OS inside
// a test binary), but declining is an ERROR, and the scripted runs below assert
// on what the agent does after a successful open. The recorder is what makes
// the tool report success.
func stubBrowserOpener(t *testing.T) {
	t.Helper()
	browsertest.Record(t)
}

// TestAgentScriptedHappyPath drives Run with a scripted fake LLM provider
// that sequences: open_browser → prompt_user → store_credential → agent_work_complete.
// Asserts that store_credential was called with the expected value.
func TestAgentScriptedHappyPath(t *testing.T) {
	stubBrowserOpener(t)

	// Override the prompt_user seam to return a scripted PAT.
	oldPromptUser := tools.PromptUser
	tools.PromptUser = func(_ context.Context, _ string, _ string, _ []string, _ string, _ io.Reader, _ io.Writer) (string, error) {
		return "ghp_xxx_yyy_zzz_aaa_bbb_ccc_ddd_eee", nil
	}
	t.Cleanup(func() { tools.PromptUser = oldPromptUser })

	stored := builtins.StoreValue{}
	storeCalled := false
	store := func(_ context.Context, v builtins.StoreValue) error {
		stored = v
		storeCalled = true
		return nil
	}

	// Script the LLM: four turns, each emitting exactly one tool_use.
	installFakeLLM(t, []llmfake.Step{
		// Turn 1: open the browser.
		{Resp: toolUseResp(t, "tu_1", "open_browser", map[string]string{
			"url": "https://github.com/settings/tokens/new",
		})},
		// Turn 2: prompt the user for their PAT.
		{Resp: toolUseResp(t, "tu_2", "prompt_user", map[string]interface{}{
			"message": "Paste your GitHub PAT here",
			"kind":    "secret",
		})},
		// Turn 3: store the credential.
		{Resp: toolUseResp(t, "tu_3", "store_credential", map[string]interface{}{
			"shape": "bearer",
			"value": "ghp_xxx_yyy_zzz_aaa_bbb_ccc_ddd_eee",
		})},
		// Turn 4: signal completion.
		{Resp: toolUseResp(t, "tu_4", "agent_work_complete", map[string]string{
			"summary": "GitHub PAT stored successfully",
		})},
	})

	prov := &provider.Provider{
		ID:      "github-pat",
		Builtin: "",
		Shape:   "bearer",
		Prompt:  "Go to https://github.com/settings/tokens/new and create a PAT.",
		DocsURL: "https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/creating-a-personal-access-token",
		TokenShape: &provider.TokenShape{
			Pattern:     `^ghp_`,
			Description: "GitHub PAT prefix",
		},
	}
	req := llmagent.Request{
		Request: builtins.Request{
			Provider: prov,
			Requirement: authkind.CredentialRequirement{
				SuggestedName: "github-pat",
				BindingEnv:    map[string]string{"GH_TOKEN": ""},
			},
			UserIntent:   "authenticate gh CLI",
			Store:        store,
			IdentityName: "my-bot",
			Namespace:    "default",
		},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}

	require.NoError(t, llmagent.Run(context.Background(), req), "Run")
	require.True(t, storeCalled, "store was not called")
	assert.True(t, strings.HasPrefix(stored.Bearer, "ghp_"),
		"stored.Bearer = %q, want ghp_... prefix", stored.Bearer)
}

// TestAgentBudgetExhaustion verifies that when the runner's budget is exhausted
// before the agent calls store_credential or agent_work_complete, Run returns a
// wrapped ErrBudgetExhausted so the engine can soft-skip the requirement.
func TestAgentBudgetExhaustion(t *testing.T) {
	// Clamp MaxTurns to 1 so the post-tool-dispatch budget check fires
	// immediately after the first turn.
	oldMaxTurns := llmagent.MaxTurns
	llmagent.MaxTurns = 1
	t.Cleanup(func() { llmagent.MaxTurns = oldMaxTurns })

	stubBrowserOpener(t)

	// Script the LLM to emit one non-terminal tool call (open_browser).
	// After this turn, MaxTurns=1 means the post-dispatch budget check fires
	// before any further LLM call, resulting in ErrBudgetExhausted.
	installFakeLLM(t, []llmfake.Step{
		{Resp: toolUseResp(t, "tu_1", "open_browser", map[string]string{
			"url": "https://example.com",
		})},
	})

	storeCalled := false
	store := func(_ context.Context, _ builtins.StoreValue) error {
		storeCalled = true
		return nil
	}

	req := llmagent.Request{
		Request: builtins.Request{
			Requirement: authkind.CredentialRequirement{
				SuggestedName: "budget-cred",
				InlinePrompt:  "Get the API key.",
			},
			Store: store,
		},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}

	err := llmagent.Run(context.Background(), req)
	require.Error(t, err, "expected ErrBudgetExhausted")
	assert.ErrorIs(t, err, llmagent.ErrBudgetExhausted, "expected wrapped ErrBudgetExhausted")
	assert.False(t, storeCalled, "store should not have been called on budget exhaustion")
}

// TestAgentAbortWithoutStore verifies that if the LLM calls agent_work_complete
// without calling store_credential first, Run returns ErrUserAborted.
func TestAgentAbortWithoutStore(t *testing.T) {
	installFakeLLM(t, []llmfake.Step{
		// The agent goes straight to agent_work_complete with no store.
		{Resp: toolUseResp(t, "tu_1", "agent_work_complete", map[string]string{
			"summary": "User declined to provide a token",
		})},
	})

	storeCalled := false
	store := func(_ context.Context, _ builtins.StoreValue) error {
		storeCalled = true
		return nil
	}

	req := llmagent.Request{
		Request: builtins.Request{
			Requirement: authkind.CredentialRequirement{
				SuggestedName: "my-cred",
				InlinePrompt:  "Get the API key from your dashboard.",
			},
			Store: store,
		},
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
	}

	err := llmagent.Run(context.Background(), req)
	require.Error(t, err, "expected ErrUserAborted")
	// Either errors.Is(ErrUserAborted) or message contains "aborted" — the
	// production code wraps in a few ways across callsites.
	matches := assert.ObjectsAreEqual(err, llmagent.ErrUserAborted) ||
		strings.Contains(err.Error(), "aborted")
	assert.True(t, matches, "expected ErrUserAborted (or 'aborted' in message); got: %v", err)
	assert.False(t, storeCalled, "store should not have been called")
}
