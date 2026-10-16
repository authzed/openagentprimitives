// pkg/agent/runner/loop_autofill_integration_test.go
//
// Verifies the flag-gated autofill behavior at the runner dispatch site.
//
//   - Flag OFF + class has authz.slots: no Fill applied (dispatch sees raw LLM args)
//   - Flag ON + binding present + tool matches: Fill applied (merged args reach Execute)
//   - Flag ON + binding present + tool does NOT match: args unchanged (no fill)
//   - Flag ON + no binding in memory: args unchanged (FillToolArgs no-ops on empty)
//
// This test replaces pkg/agent/runner/autofill_dispatch_test.go (deleted in
// Task 14). The new test routes through eng.FillToolArgs instead of the
// deleted autofill.Fill + AgentSession.Status.BoundEntities path.
//
// Implementation: the loop is exercised via its existing in-process driver
// using a llmfake.Provider script. The Engine is the real engine.Engine
// (engine.New) backed by an inmem memory backend. Bindings are seeded via
// binding.Record before Run is called.
package runner_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	eex "github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
)

// inputCaptureTool records the JSON input it received from Execute so the
// test can assert the autofilled vs raw args.
type inputCaptureTool struct {
	name    string
	perm    authz.Permission
	gotJSON atomic.Value // json.RawMessage
}

func (ct *inputCaptureTool) Name() string                                  { return ct.name }
func (ct *inputCaptureTool) Kind() tool.Kind                               { return tool.KindSandbox }
func (ct *inputCaptureTool) Description() string                           { return "input capture tool" }
func (ct *inputCaptureTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{}`) }
func (ct *inputCaptureTool) Permission() authz.Permission                  { return ct.perm }
func (ct *inputCaptureTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (ct *inputCaptureTool) Execute(_ context.Context, in json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	cp := make(json.RawMessage, len(in))
	copy(cp, in)
	ct.gotJSON.Store(cp)
	return tool.Result{Content: "ok"}, nil
}

// alwaysAllowSpiceDB always returns HasPermission.
type alwaysAllowSpiceDB struct{}

func (a *alwaysAllowSpiceDB) CheckPermission(_ context.Context, _ *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	return &v1.CheckPermissionResponse{
		Permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		CheckedAt:      &v1.ZedToken{Token: "tok-test"},
	}, nil
}

// allowAllToolChecker is a test-local engine.ToolChecker that always allows.
type allowAllToolChecker struct{}

func (a *allowAllToolChecker) CheckToolCall(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	return authz.Result{Outcome: authz.OutcomeAllowed}
}

// autofillLoopSpec holds the parameters for each autofill integration test case.
type autofillLoopSpec struct {
	autofillEnabled bool
	toolName        string // name of the tool the LLM will call
	seedBinding     bool   // whether to seed a github_repo slot grant
	inputJSON       string // LLM-emitted input args (missing repo when testing autofill)
	// seedExtracted, when non-empty, records an extracted_entity for turn 0 as
	// authzd's extractor would — a CANDIDATE, with no grant. Only the promotion
	// step on the dispatch path can turn it into one.
	seedExtracted string
	// slotFillFrom overrides the class slot's fillFrom.
	slotFillFrom []string
	// seedPin, when non-empty, pre-pins the single-occupancy github_repo slot to
	// this instance before Run, so a promotion of a DIFFERENT instance is refused
	// by the pin (authz.ErrSlotPinned).
	seedPin string
	// toolChecker overrides the engine's ToolChecker. Nil keeps the allow-all
	// default; a custom one lets a test deny the dispatch Check while still
	// admitting the promotion candidate.
	toolChecker engine.ToolChecker
}

// buildAutofillLoop constructs the Loop + capturingTool + session fixtures.
// Returns the loop and the capturing tool.
func buildAutofillLoop(t *testing.T, spec autofillLoopSpec) (*runner.Loop, *inputCaptureTool) {
	t.Helper()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "af-sess", Namespace: "default", Generation: 1,
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
			},
		},
	}
	cls := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Slots: []spiceboxv1alpha1.AuthzSlot{
					{
						ResourceType: "github_repo",
						Description:  "GitHub repo",
						Permission:   "read",
						FillFrom:     spec.slotFillFrom,
						AutoFillArgs: []spiceboxv1alpha1.AuthzSlotAutoFillArg{
							{ArgName: "repo", ToolNamePattern: "gh_*"},
						},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	key := memory.NamespacedName{Namespace: "default", Name: "af-sess"}
	backend := inmem.NewBackend()
	memLocal := memory.NewLocal(backend)
	store := runner.LocalMemoryAdapter(memLocal, key)

	// Seed a slot grant if requested. engine.FillToolArgs reads the session's
	// slot grants — the same store the per-tool Check consults — so the
	// binding must exist there, not in the Layer-2 scope document.
	slots := newSlotGrantStore()
	if spec.seedBinding {
		require.NoError(t, slots.WriteRelationships(context.Background(), []authz.Relation{
			authz.SlotGrantRelation("github_repo", "demo-org/demo-repo", "read",
				authz.SessionRef{Namespace: "default", Name: "af-sess"}),
		}))
	}
	if spec.seedExtracted != "" {
		require.NoError(t, eex.Record(memory.WithSystemApproval(context.Background(), "test"), memLocal,
			memory.Scope{Kind: "session", ID: "default/af-sess"},
			eex.Content{ResourceType: "github_repo", ResourceID: spec.seedExtracted, TurnIndex: 0}))
	}
	if spec.seedPin != "" {
		_, _, err := slots.EnsurePin(context.Background(), "github_repo", spec.seedPin,
			authz.SessionRef{Namespace: "default", Name: "af-sess"})
		require.NoError(t, err, "pre-seed pin")
	}

	var chk engine.ToolChecker = &allowAllToolChecker{}
	if spec.toolChecker != nil {
		chk = spec.toolChecker
	}
	spdb := &alwaysAllowSpiceDB{}
	zedCache := toolcheck.NewZedTokenCache()
	eng := engine.New(engine.Deps{
		ToolChecker: chk,
		RelWriter:   slots,
		SlotLister:  slots,
		Memory:      memLocal,
	})

	capTool := &inputCaptureTool{
		name: spec.toolName,
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}
	tools := append(meta.Load(), capTool)

	toolCallStep := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
				ID:    "tu_af",
				Name:  spec.toolName,
				Input: json.RawMessage(spec.inputJSON),
			}},
		},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
	provider := llmfake.New([]llmfake.Step{toolCallStep, workCompleteResp()})

	l := &runner.Loop{
		Provider:   provider,
		Memory:     store,
		Status:     runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:      tools,
		System:     "test",
		UserPrompt: "look at that PR",
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    5,
			MaxTokens:   10000,
			MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:      "claude-test",
		MaxTokens:  1024,
		SessionKey: key,
		AuthzCli:   spdb,
		AuthzCache: zedCache,
		Engine:     eng,
		AgentClass: cls,
		// The same store the engine grants through, so the pin-refusal record
		// can read the pinned instance back (and the mirror its pin state),
		// exactly as production wires one SpiceDB client into both.
		SlotBinder: slots,

		BindingAutofillEnabled:  spec.autofillEnabled,
		BindingAutofillDeadline: 100 * time.Millisecond,
	}
	l.ResolveAuthSubjects(sess, cls)
	return l, capTool
}

// TestAutofill_FlagOn_BindingPresent_ToolMatches: flag ON + binding in memory +
// tool name matches gh_* pattern → repo arg filled from the slot grant.
func TestAutofill_FlagOn_BindingPresent_ToolMatches(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedBinding:     true,
		inputJSON:       `{"prNumber":42}`,
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	assert.Equal(t, "demo-org/demo-repo", args["repo"], "repo arg should be autofilled")
	assert.Equal(t, float64(42), args["prNumber"], "prNumber should be preserved")
}

// TestAutofill_FlagOff_BindingPresent: flag OFF + binding in memory → dispatch
// receives raw LLM args (no fill). The repo arg remains absent.
func TestAutofill_FlagOff_BindingPresent(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: false,
		toolName:        "gh_pr_view",
		seedBinding:     true,
		inputJSON:       `{"prNumber":99}`,
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	_, hasRepo := args["repo"]
	assert.False(t, hasRepo, "repo should NOT be filled when flag is off")
	assert.Equal(t, float64(99), args["prNumber"])
}

// TestAutofill_FlagOn_NoBinding: flag ON + no binding in memory → FillToolArgs
// finds nothing and returns raw args unchanged. Execute receives original input.
func TestAutofill_FlagOn_NoBinding(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedBinding:     false,
		inputJSON:       `{"prNumber":7}`,
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	_, hasRepo := args["repo"]
	assert.False(t, hasRepo, "repo should remain absent when no binding exists")
	assert.Equal(t, float64(7), args["prNumber"])
}

// TestAutofill_FlagOn_ToolDoesNotMatch: flag ON + binding present + tool name
// does NOT match the gh_* pattern → no fill applied; args unchanged.
func TestAutofill_FlagOn_ToolDoesNotMatch(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "other_tool",
		seedBinding:     true,
		inputJSON:       `{"someArg":"val"}`,
	})
	// Swap the tool permission to stateless so the authz check doesn't
	// fail on a missing repo template (the tool name doesn't match gh_*
	// so the Check would fail without a repo; use stateless to bypass).
	capTool.perm = authz.Permission{StateImpact: authz.Stateless}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	_, hasRepo := args["repo"]
	assert.False(t, hasRepo, "repo should not be filled when tool name does not match pattern")
	assert.Equal(t, "val", args["someArg"])
}

// TestAutofill_PromotesExtractedCandidateOnDispatch is the wiring half of the
// query/extract fill source. The fixture seeds ONLY an extracted_entity — the
// proposal authzd records after running its extractor over the user's text —
// and NO grant. The arg can therefore only be filled if the dispatch path ran
// the promotion: Checked the candidate for the requester and wrote the grant
// that FillToolArgs then reads.
//
// Both halves are unit-tested in pkg/authz and would pass whether or not
// anything called them; this is the test that fails if the call is removed.
func TestAutofill_PromotesExtractedCandidateOnDispatch(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedExtracted:   "demo-org/from-the-message",
		inputJSON:       `{"prNumber":11}`,
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	assert.Equal(t, "demo-org/from-the-message", args["repo"],
		"a candidate the extractor proposed must bind and then fill the arg")
	assert.Equal(t, float64(11), args["prNumber"])
}

// TestAutofill_ExtractedCandidate_RefusedForASlotAdmittingNoExtractor: the same
// dispatch, with the class declaring the slot unfillable from a user message at
// all (default-only: the class pins its own ids). The candidate is still
// recorded — a stale or hand-written entry can exist — and must not bind.
//
// NB `ask` is deliberately NOT the example here: ask admits the extractor, since
// the agent prompting for a value and the user volunteering one are the same
// binding path.
func TestAutofill_ExtractedCandidate_RefusedForASlotAdmittingNoExtractor(t *testing.T) {
	l, capTool := buildAutofillLoop(t, autofillLoopSpec{
		autofillEnabled: true,
		toolName:        "gh_pr_view",
		seedExtracted:   "demo-org/from-the-message",
		slotFillFrom:    []string{"default"},
		inputJSON:       `{"prNumber":12}`,
	})
	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))

	got := capTool.gotJSON.Load()
	require.NotNil(t, got, "capturingTool.Execute was not invoked")
	var args map[string]any
	require.NoError(t, json.Unmarshal(got.(json.RawMessage), &args))
	_, hasRepo := args["repo"]
	assert.False(t, hasRepo, "a slot excluding query must not bind an extracted candidate")
	assert.Equal(t, float64(12), args["prNumber"])
}
