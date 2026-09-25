// Package llmagent runs the LLM-driven setup agent for any requirement that
// lacks a builtin Go flow.
package llmagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
)

// MaxTurns caps the setup agent's loop against runaway conversations.
// Exported as a var so tests can override it to force budget exhaustion.
var MaxTurns = 30

// defaultModelID is the model used when LLMProviderFactory returns a provider
// with no default of its own. Tracks pkg/agent/llm/anthropic's DefaultModelID;
// bump together.
const defaultModelID = "claude-opus-4-7"

// ErrUserAborted is returned when the agent called agent_work_complete
// without having called store_credential first — meaning the user declined
// or the agent gave up.
var ErrUserAborted = errors.New("llmagent: agent completed without storing a credential (user may have aborted)")

// ErrBudgetExhausted is returned by Run when the agent's per-invocation budget
// is reached without storing a credential. The engine surfaces it as a soft
// "skipped" so other credentials in the same setup invocation continue.
var ErrBudgetExhausted = errors.New("llmagent: budget exhausted")

// Request is what the setup engine hands Run: the builtins.Request describing
// the credential, plus the raw streams and styler this agent prompts over.
//
// The streams are carried here rather than on builtins.Request because a builtin
// FLOW owns no I/O — it describes screens the sequencer presents. This agent is
// not a flow: it is an LLM driving tools, several of which (prompt_user,
// run_shell) talk to the user directly, so it needs the terminal handed to it.
type Request struct {
	builtins.Request

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Theme  *tui.Theme
}

// Run is the entry point the setup engine invokes when no builtin flow handles a
// requirement. It assembles the tool palette, builds a system prompt, and drives
// the gen-agent runner loop until agent_work_complete fires or the budget is
// exhausted.
//
// Returns nil if store_credential was called during the session, ErrUserAborted
// if agent_work_complete fired without one, and other errors for provider or
// budget failures.
func Run(ctx context.Context, req Request) error {
	prov, err := LLMProviderFactory(ctx)
	if err != nil {
		return fmt.Errorf("llmagent: provider: %w", err)
	}

	systemPrompt := BuildSystemPrompt(req.Provider,
		req.Requirement.InlinePrompt, req.UserIntent, req.Requirement)

	// Compile the run_shell allowlist (empty for inline-prompt requirements).
	var shellState *tools.RunShellState
	if req.Provider != nil && len(req.Provider.RunShellAllowlist) > 0 {
		allowlist, aerr := tools.CompileAllowlist(req.Provider)
		if aerr != nil {
			return fmt.Errorf("llmagent: %w", aerr)
		}
		shellState = &tools.RunShellState{Allowlist: allowlist}
	} else {
		shellState = &tools.RunShellState{}
	}

	// Track whether store_credential was called during this session.
	var (
		credMu     sync.Mutex
		credStored bool
	)

	// Wrap req.Store so we can gate on verification and detect when it fires.
	// guardStore's subject id rides the context into req.Store rather than as a
	// parameter: req.Store's signature is shared verbatim by every llmagent/tools
	// callback, so it has no room to grow one, and re-verifying to recover the id
	// a second time risks disagreeing with the first check (a rate limit or a
	// revocation landing in the gap) — see ContextWithSubjectID's doc.
	wrappedStore := func(ctx context.Context, v builtins.StoreValue) error {
		subjectID, err := guardStore(ctx, req.Provider, req.Stdout, req.Theme, v)
		if err != nil {
			return err
		}
		if err := req.Store(ContextWithSubjectID(ctx, subjectID), v); err != nil {
			return err
		}
		credMu.Lock()
		credStored = true
		credMu.Unlock()
		return nil
	}

	// Build the tool list as tool.Tool implementations.
	toolList := []tool.Tool{
		&funcTool{
			name:        tools.OpenBrowserName,
			description: "Open a URL in the user's default browser. Only http/https URLs are accepted.",
			schema:      json.RawMessage(tools.OpenBrowserSchema),
			fn:          func(ctx context.Context, raw json.RawMessage) (string, error) { return tools.OpenBrowserRun(ctx, raw) },
		},
		&funcTool{
			name:        tools.PromptUserName,
			description: "Prompt the user for input. kind must be text, secret, or choice. For secrets, use kind=secret so the user knows to protect the value.",
			schema:      json.RawMessage(tools.PromptUserSchema),
			fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return tools.PromptUserRun(ctx, raw, req.Stdin, req.Stdout)
			},
		},
		&funcTool{
			name:        tools.FetchURLName,
			description: "Fetch the content of an http/https URL. Use to retrieve documentation pages.",
			schema:      json.RawMessage(tools.FetchURLSchema),
			fn:          func(ctx context.Context, raw json.RawMessage) (string, error) { return tools.FetchURLRun(ctx, raw) },
		},
		&funcTool{
			name:        tools.LocalCallbackName,
			description: "Start a localhost callback listener for OAuth redirect flows. Returns the listener URL and, after the first request arrives, the query parameters. An OAuth authorization code is never returned: pass oauth_exchange (token_endpoint, client_id, optional client_secret/code_verifier) and the tool exchanges the code and stores the resulting token itself — no store_credential call needed afterwards.",
			schema:      json.RawMessage(tools.LocalCallbackSchema),
			fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return tools.LocalCallbackRun(ctx, raw, req.Stderr, wrappedStore)
			},
		},
		&funcTool{
			name:        tools.StoreCredentialName,
			description: "Persist the credential. Call this once you have a validated value from the user. shape must be bearer, oauth, or kubeconfig.",
			schema:      json.RawMessage(tools.StoreCredentialSchema),
			fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return tools.StoreCredentialRun(ctx, raw, wrappedStore)
			},
		},
	}

	// web_search is registered only when the client is configured: with a nil
	// client the tool is absent from the list, so the LLM never sees it.
	if tools.WebSearchClient != nil {
		toolList = append(toolList, &funcTool{
			name:        tools.WebSearchName,
			description: "Search the web for documentation or instructions. Use when you need to find setup URLs or step-by-step guides.",
			schema:      json.RawMessage(tools.WebSearchSchema),
			fn:          func(ctx context.Context, raw json.RawMessage) (string, error) { return tools.WebSearchRun(ctx, raw) },
		})
	}

	// run_shell is only registered when the provider has an allowlist.
	if len(shellState.Allowlist) > 0 {
		state := shellState // capture
		toolList = append(toolList, &funcTool{
			name:        tools.RunShellName,
			description: "Run a shell command from the provider-defined allowlist. Requires per-call user confirmation.",
			schema:      json.RawMessage(tools.RunShellSchema),
			fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
				return tools.RunShellRun(ctx, raw, state, req.Stdin, req.Stdout)
			},
		})
	}

	// Add agent_work_complete (the terminal meta tool).
	toolList = append(toolList, meta.NewAgentWorkComplete(meta.CompletionConfig{}))

	sessionKey := memory.NamespacedName{Namespace: "setup", Name: "llmagent"}
	status := runner.LocalStatusPatcher()

	loop := &runner.Loop{
		Provider:   prov,
		Memory:     runner.LocalMemoryStore(sessionKey),
		Status:     status,
		Tools:      toolList,
		System:     systemPrompt,
		UserPrompt: fmt.Sprintf("Please walk me through setting up the %q credential.", req.Requirement.SuggestedName),
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    int32(MaxTurns),
			MaxTokens:   0,
			MaxDuration: metav1.Duration{Duration: 0},
		}, nil, time.Now()),
		Model:           defaultModelID,
		MaxTokens:       16384,
		SessionKey:      sessionKey,
		ChannelAttached: false,
		AgentName:       "setup-agent",
		Progress: func(format string, args ...any) {
			if req.Stdout != nil {
				fmt.Fprintf(req.Stdout, format+"\n", args...)
			}
		},
		SessionContext: &tool.SessionContext{
			Namespace: sessionKey.Namespace,
			Name:      sessionKey.Name,
		},
	}

	// A single local in-process identity-setup session against an in-process
	// memory Local: clear its capability door with a system approval, there being
	// no per-user boundary in this flow.
	ctx = memory.WithSystemApproval(ctx, "ap:identity-setup")
	if err := loop.Run(ctx); err != nil {
		return fmt.Errorf("llmagent: loop: %w", err)
	}

	// With LocalStatusPatcher, loop.Run swallows budget/provider failures into the
	// patcher's local state. Surface them to the caller.
	if status.LocalResult() == nil {
		if f := status.LocalFailure(); f != nil {
			if f.Reason == spiceboxv1alpha1.ReasonAgentSessionBudget {
				return fmt.Errorf("%w: %s — %s", ErrBudgetExhausted, f.Reason, f.Message)
			}
			return fmt.Errorf("llmagent: agent failed: %s — %s", f.Reason, f.Message)
		}
		return errors.New("llmagent: loop ended without agent_work_complete (runner bug)")
	}

	// Loop succeeded (agent_work_complete fired); did anything get stored?
	credMu.Lock()
	stored := credStored
	credMu.Unlock()

	if !stored {
		return ErrUserAborted
	}
	return nil
}

// funcTool adapts a plain function to the tool.Tool interface. The setup agent's
// tools are standalone functions rather than Tool structs, so unit tests can call
// them directly without the runner machinery; this adapter bridges the two.
type funcTool struct {
	name        string
	description string
	schema      json.RawMessage
	fn          func(ctx context.Context, raw json.RawMessage) (string, error)
}

func (t *funcTool) Name() string                 { return t.name }
func (t *funcTool) Kind() tool.Kind              { return tool.KindMeta }
func (t *funcTool) Description() string          { return t.description }
func (t *funcTool) InputSchema() json.RawMessage { return t.schema }
func (*funcTool) Permission() authz.Permission {
	// Setup-agent tools run in a privileged, non-user-session context (the
	// credential-provisioning wizard), so they are not subject to the per-session
	// SpiceDB check path.
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*funcTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (t *funcTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	content, err := t.fn(ctx, args)
	if err != nil {
		// Tool-level errors come back as IsError results so the model can
		// self-correct (invalid args, allowlist rejection). Only transport-level
		// runner failures are Go errors.
		return tool.Result{Content: fmt.Sprintf("%s: %v", t.name, err), IsError: true}, nil
	}
	return tool.Result{Content: content}, nil
}
