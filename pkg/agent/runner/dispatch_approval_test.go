package runner_test

// Integration-style tests for the slice-2 approval pause/resume wiring in
// dispatchToolUses. The flow exercised here:
//
//   1. LLM emits a tool_use whose Permission requires authz (denied by the
//      mock SpiceDB on the primary check).
//   2. Runner publishes a KindToolApprovalRequest envelope and blocks on
//      the Orchestrator.
//   3. Test asynchronously calls orchestrator.DeliverDecision (approve or
//      deny) with the captured request ID.
//   4. On approve: SpiceDB's secondary grant lookup returns HAS_PERMISSION
//      and Tool.Execute runs; the tool_result the LLM sees is success.
//   5. On deny: Tool.Execute is NOT called; the LLM sees an IsError result
//      naming the denying approver.

import (
	"context"
	"encoding/json"
	"sync"
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
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
)

// scriptedSpiceDB returns one Permissionship per CheckPermission call,
// in order. Used to script primary-deny + grant-allow / both-deny flows.
type scriptedSpiceDB struct {
	mu       sync.Mutex
	answers  []v1.CheckPermissionResponse_Permissionship
	requests []*v1.CheckPermissionRequest
}

func (s *scriptedSpiceDB) CheckPermission(_ context.Context, req *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(s.requests)
	s.requests = append(s.requests, req)
	if idx >= len(s.answers) {
		idx = len(s.answers) - 1
	}
	return &v1.CheckPermissionResponse{
		Permissionship: s.answers[idx],
		CheckedAt:      &v1.ZedToken{Token: "tok"},
	}, nil
}

func (s *scriptedSpiceDB) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// makeApprovalLoop returns a Loop wired with:
//   - the supplied tools and LLM script
//   - the supplied SpiceDB stub as AuthzCli
//   - a fresh approval.Orchestrator
//   - InteractionRequestPublish capturing the published envelope to capturedEnv
//     (Slice C2: tool_call publishes the generic interaction_request)
//   - SpiceDBLookupSubjects returning a fixed resource/session owner so the
//     interaction publisher can resolve Audience.Approvers (it fails closed on
//     an empty pool, mirroring the raise-time approver check)
func makeApprovalLoop(
	t *testing.T,
	tools []tool.Tool,
	script []llmfake.Step,
	spdb toolcheck.Client,
	capturedEnv *channelevents.Envelope,
	capturedMu *sync.Mutex,
	envCh chan struct{},
) (*runner.Loop, *approval.Orchestrator) {
	t.Helper()
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "approve-sess", Namespace: "default", Generation: 1,
			Annotations: annotations,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	provider := llmfake.New(script)
	key := memory.NamespacedName{Namespace: "default", Name: "approve-sess"}

	orch := approval.New()

	l := &runner.Loop{
		// Declared, because the router refuses a type with no published standing.
		ResourceStandings: map[string]runner.ResourceStanding{
			"repo":        {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"github_repo": {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"issue":       {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"mailbox":     {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"crm_company": {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"r":           {Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: "owner"},
			"git_repo":    {Standing: spiceboxv1alpha1.StandingSessionOnly},
		},
		Provider:   provider,
		Memory:     runner.LocalMemoryAdapter(memory.NewLocal(inmem.NewBackend()), key),
		Status:     runner.NewStatusPatcher(c, client.ObjectKeyFromObject(sess)),
		Tools:      tools,
		System:     "test",
		UserPrompt: "test",
		Budget: runner.NewBudget(spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    10,
			MaxTokens:   100000,
			MaxDuration: metav1.Duration{Duration: time.Hour},
		}, nil, time.Now()),
		Model:       "claude-test",
		MaxTokens:   1024,
		SessionKey:  key,
		AuthzCli:    spdb,
		AuthzCache:  toolcheck.NewZedTokenCache(),
		Approval:    orch,
		ChannelKind: "slack",
		// The interaction publisher resolves Audience.Approvers via
		// SpiceDBLookupSubjects (resource #owner or session approve-set) and
		// fails closed on an empty pool — return a fixed owner so the tool_call
		// gate can build a deliverable prompt.
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@example.com"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _ string, _ string, env channelevents.Envelope) error {
			capturedMu.Lock()
			*capturedEnv = env
			capturedMu.Unlock()
			// Notify the test that an envelope was published so it can
			// resolve the matching RequestRef via DeliverDecision.
			select {
			case envCh <- struct{}{}:
			default:
			}
			return nil
		},
	}
	cls := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, cls)
	return l, orch
}

// scriptDenyTool returns an LLM script that (1) calls the supplied tool
// with the given args, then (2) calls agent_work_complete on the next turn.
func scriptDenyTool(toolName string, toolUseID string, args string) []llmfake.Step {
	return []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "text", Text: "I need to write to the repo to land that fix."},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID:    toolUseID,
					Name:  toolName,
					Input: json.RawMessage(args),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
}

// TestDispatch_ApprovalPause_ApproveResumes verifies that:
//   - primary Check denial pauses the dispatch
//   - the published envelope is a KindToolApprovalRequest with the
//     expected payload (tool name, permission, justification)
//   - DeliverDecision(approve) triggers a secondary Check (which passes)
//     and the tool's Execute runs
//   - the tool_result fed back to the LLM is the tool's normal output
func TestDispatch_ApprovalPause_ApproveResumes(t *testing.T) {
	// Primary check denies → orchestrator pause → on approve, grant
	// check allows → Execute runs.
	spdb := &scriptedSpiceDB{
		answers: []v1.CheckPermissionResponse_Permissionship{
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,  // primary
			v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, // grant
		},
	}

	writeTool := &countingTool{
		name: "write_repo",
		perm: authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "write",
			},
		},
	}
	tools := append(meta.Load(), writeTool)
	script := scriptDenyTool("write_repo", "tu_write", `{"repo":"spicedb"}`)

	var capturedEnv channelevents.Envelope
	var capturedMu sync.Mutex
	envCh := make(chan struct{}, 1)
	l, orch := makeApprovalLoop(t, tools, script, spdb, &capturedEnv, &capturedMu, envCh)

	// Drive the loop in a goroutine; deliver decision once we see the
	// publish callback fire.
	done := make(chan error, 1)
	go func() {
		done <- l.Run(memory.WithSystemApproval(context.Background(), "test"))
	}()

	select {
	case <-envCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ApprovalPublish callback")
	}

	capturedMu.Lock()
	env := capturedEnv
	capturedMu.Unlock()

	assert.Equal(t, channelevents.KindInteractionRequest, env.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, categories.ToolApproval, pl.Category)
	// The grant-write inputs now ride Details; the resource-owner standing input
	// rides Resources.
	var det channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(pl.Details, &det))
	assert.Equal(t, "write", det.Permission)
	assert.Equal(t, "github_repo", det.ResourceType)
	assert.Equal(t, "spicedb", det.ResourceID)
	assert.NotEmpty(t, det.ArgsHash)
	assert.Contains(t, det.Justification, "write to the repo")
	require.Len(t, pl.Resources, 1)
	assert.Equal(t, "github_repo", pl.Resources[0].Type)
	assert.Equal(t, "spicedb", pl.Resources[0].ID)
	require.NotEmpty(t, pl.RequestRef, "payload.RequestRef is empty; cannot resolve decision")

	// Deliver an approve decision.
	orch.DeliverDecision(pl.RequestRef, approval.Decision{
		Approved:   true,
		ApproverID: "U_APPROVER",
	})

	select {
	case err := <-done:
		require.NoError(t, err, "loop.Run")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loop to finish after approve")
	}

	assert.Equal(t, 1, writeTool.executes, "write_repo.Execute should fire once on approve")
	assert.Equal(t, 2, spdb.calls(), "expected 2 SpiceDB calls (primary + grant)")
}

// TestDispatch_ApprovalPause_VariantResolvedOnEnvelope is a cross-cutting
// regression test: when a tool's PermissionVariants route to a Readonly Check
// (with EnforceMode=always to force approval under enforcing mode), the
// published ToolApprovalRequest envelope MUST carry the variant's
// (resourceType, permission), NOT the singular t.Permission() fallback.
// Carrying the fallback would publish an envelope referencing an often-empty
// resourceType + permission, and run the post-approval re-check against the
// wrong tuple.
func TestDispatch_ApprovalPause_VariantResolvedOnEnvelope(t *testing.T) {
	// Primary check denies on the variant tuple; grant lookup answers
	// HAS_PERMISSION after the approve decision so Execute fires.
	spdb := &scriptedSpiceDB{
		answers: []v1.CheckPermissionResponse_Permissionship{
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
			v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		},
	}

	variantTool := &countingTool{
		name: "crm_search",
		// Fallback is passthrough — the singular Permission has no Check
		// at all. If the dispatcher used this fallback to build the
		// approval envelope, ResourceType/Permission would be empty.
		perm: authz.Permission{StateImpact: authz.Passthrough},
		variants: []authz.PermissionVariant{
			{
				When: `args.objectType == "contacts"`,
				Check: authz.Permission{
					StateImpact: authz.Readonly,
					Check: &authz.PermissionCheck{
						ResourceType:       "crm_company",
						ResourceIDTemplate: "acme",
						Permission:         "contact_access",
						// EnforceMode=always elevates a readonly deny
						// past the permissive bypass so the approval
						// flow always fires.
						EnforceMode: authz.EnforceAlways,
					},
				},
			},
		},
	}
	tools := append(meta.Load(), variantTool)
	script := scriptDenyTool("crm_search", "tu_contacts", `{"objectType":"contacts"}`)

	var capturedEnv channelevents.Envelope
	var capturedMu sync.Mutex
	envCh := make(chan struct{}, 1)
	l, orch := makeApprovalLoop(t, tools, script, spdb, &capturedEnv, &capturedMu, envCh)

	done := make(chan error, 1)
	go func() {
		done <- l.Run(memory.WithSystemApproval(context.Background(), "test"))
	}()

	select {
	case <-envCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ApprovalPublish callback")
	}

	capturedMu.Lock()
	env := capturedEnv
	capturedMu.Unlock()

	assert.Equal(t, channelevents.KindInteractionRequest, env.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, categories.ToolApproval, pl.Category)
	// The load-bearing assertions: every grant-write + standing input MUST
	// reflect the VARIANT, not the passthrough fallback.
	var det channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(pl.Details, &det))
	assert.Equal(t, "crm_company", det.ResourceType, "variant should win over passthrough fallback")
	assert.Equal(t, "contact_access", det.Permission)
	assert.Equal(t, "acme", det.ResourceID)
	assert.Equal(t, string(authz.Readonly), det.StateImpact)
	// Resource-owner standing: Resources carries the gated resource so the
	// DecideResourceOwners policy routes/gates on the resource #owner (replacing
	// the legacy ApproverSubject "crm_company:acme#owner" field).
	require.Len(t, pl.Resources, 1)
	assert.Equal(t, "crm_company", pl.Resources[0].Type,
		"Resources must carry the VARIANT's gated resource (the resource-owner standing input)")
	assert.Equal(t, "acme", pl.Resources[0].ID)

	// Deliver approve so the loop can finish cleanly.
	orch.DeliverDecision(pl.RequestRef, approval.Decision{
		Approved:   true,
		ApproverID: "U_APPROVER",
	})

	select {
	case err := <-done:
		require.NoError(t, err, "loop.Run")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loop to finish after approve")
	}

	assert.Equal(t, 1, variantTool.executes, "crm_search.Execute should fire once on approve")
}

// TestDispatch_ApprovalPause_DenyReturnsIsError verifies that a deny
// decision short-circuits Execute and produces an IsError tool_result
// naming the approver.
func TestDispatch_ApprovalPause_DenyReturnsIsError(t *testing.T) {
	// Primary deny → pause → DeliverDecision(deny) → Execute does NOT fire.
	// We only need the first answer for the primary; the grant lookup
	// never happens.
	spdb := &scriptedSpiceDB{
		answers: []v1.CheckPermissionResponse_Permissionship{
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		},
	}

	writeTool := &countingTool{
		name: "write_repo",
		perm: authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "write",
			},
		},
	}
	tools := append(meta.Load(), writeTool)
	script := scriptDenyTool("write_repo", "tu_deny", `{"repo":"prod"}`)

	var capturedEnv channelevents.Envelope
	var capturedMu sync.Mutex
	envCh := make(chan struct{}, 1)
	l, orch := makeApprovalLoop(t, tools, script, spdb, &capturedEnv, &capturedMu, envCh)

	provider := l.Provider.(*llmfake.Provider)
	done := make(chan error, 1)
	go func() {
		done <- l.Run(memory.WithSystemApproval(context.Background(), "test"))
	}()

	select {
	case <-envCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ApprovalPublish callback")
	}

	capturedMu.Lock()
	env := capturedEnv
	capturedMu.Unlock()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))

	orch.DeliverDecision(pl.RequestRef, approval.Decision{
		Approved:   false,
		ApproverID: "alice",
	})

	select {
	case err := <-done:
		require.NoError(t, err, "loop.Run")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loop to finish after deny")
	}

	assert.Equal(t, 0, writeTool.executes, "write_repo.Execute should NOT fire on deny")
	assert.Equal(t, 1, spdb.calls(), "expected exactly 1 SpiceDB call (primary, no grant lookup)")

	// The deny message should be the LLM's next tool_result, IsError=true,
	// naming "alice" as the approver.
	reqs := provider.Requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	var denyMsg string
	for _, msg := range reqs[1].Messages {
		for _, blk := range msg.Content {
			if blk.ToolResult != nil && blk.ToolResult.IsError {
				denyMsg = blk.ToolResult.Content
			}
		}
	}
	require.NotEmpty(t, denyMsg, "no IsError tool_result found in second LLM request")
	// New contract (see pkg/agent/runner/approval_outcome.go): the
	// deny message uses the load-bearing DENIED keyword and names the
	// approver, so the LLM can't confabulate this as a timeout.
	assert.Contains(t, denyMsg, "DENIED")
	assert.Contains(t, denyMsg, "alice")
}

// TestDispatch_ApprovalRequest_PersistsPayloadForShowDetails verifies the
// request-time memory record carries the full channel payload, tagged by
// the envelope's RequestID, in the envelope's session scope — the record
// channelsd's Show Details fallback reads (see pkg/channels/channelkinds/slack).
func TestDispatch_ApprovalRequest_PersistsPayloadForShowDetails(t *testing.T) {
	// Primary check denies → orchestrator pause → on approve, grant
	// check allows → Execute runs. Mirrors
	// TestDispatch_ApprovalPause_ApproveResumes; the new pieces are the
	// injected l.Mem and the post-run memapproval.RequestByID assertions.
	spdb := &scriptedSpiceDB{
		answers: []v1.CheckPermissionResponse_Permissionship{
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,  // primary
			v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, // grant
		},
	}

	writeTool := &countingTool{
		name: "write_repo",
		perm: authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "write",
			},
		},
	}
	tools := append(meta.Load(), writeTool)
	script := scriptDenyTool("write_repo", "tu_details", `{"repo":"spicedb"}`)

	var capturedEnv channelevents.Envelope
	var capturedMu sync.Mutex
	envCh := make(chan struct{}, 1)
	l, orch := makeApprovalLoop(t, tools, script, spdb, &capturedEnv, &capturedMu, envCh)
	mem := memory.NewLocal(inmem.NewBackend())
	l.Mem = mem

	// Drive the loop in a goroutine; deliver decision once we see the
	// publish callback fire.
	done := make(chan error, 1)
	go func() {
		done <- l.Run(memory.WithSystemApproval(context.Background(), "test"))
	}()

	select {
	case <-envCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ApprovalPublish callback")
	}

	capturedMu.Lock()
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(capturedEnv.Payload, &pl), "decode published request payload")
	capturedMu.Unlock()
	require.NotEmpty(t, pl.RequestRef, "payload.RequestRef is empty; cannot resolve decision")

	// Deliver an approve decision.
	orch.DeliverDecision(pl.RequestRef, approval.Decision{
		Approved:   true,
		ApproverID: "U_APPROVER",
	})

	select {
	case err := <-done:
		require.NoError(t, err, "loop.Run")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loop to finish after approve")
	}

	got, err := memapproval.RequestByID(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: "default/approve-sess"}, pl.RequestRef)
	require.NoError(t, err, "RequestByID after publish")
	require.NotNil(t, got, "request record must exist, keyed by the envelope's RequestRef")
	// Slice C2: the durable record now carries the generic Resources + Details
	// (the grant-write + resource-owner authority the decision pipe reads on a
	// cache miss), not the legacy full Payload.
	assert.Equal(t, "write_repo", got.ToolName)
	require.Len(t, got.Resources, 1)
	assert.Equal(t, "github_repo", got.Resources[0].Type)
	assert.Equal(t, "spicedb", got.Resources[0].ID)
	require.NotEmpty(t, got.Details, "record must carry the grant-write Details")
	var det channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(got.Details, &det))
	assert.Equal(t, "github_repo", det.ResourceType)
	assert.Equal(t, "spicedb", det.ResourceID)
	assert.NotEmpty(t, det.ArgsJSON, "record Details carries the raw args for Show Details")
}
