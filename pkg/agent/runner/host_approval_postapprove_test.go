package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// gatedToolCallAsk is a tool_call ask whose StateImpact is Readonly with a
// Check — unlike toolCallAsk's External, which SKIPS the post-approval
// re-check entirely. This is the shape that actually runs
// toolCallPostApprove.
func gatedToolCallAsk() pipeline.ApprovalAsk {
	payload := ToolCallApprovalPayload{
		SessNS:       "ns",
		SessName:     "s",
		ToolName:     "list_contacts",
		Permission:   "contact_access",
		ResourceType: "crm_company",
		ResourceID:   "acme-id",
		StateImpact:  "readonly",
		ArgsHash:     "h1",
		ArgsJSON:     `{"companyId":"acme-id"}`,
		Perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "crm_company",
				ResourceIDTemplate: "acme-id",
				Permission:         "contact_access",
			},
		},
		UseID: "tu-postapprove",
	}
	return pipeline.ApprovalAsk{
		Kind:    "tool_call",
		Summary: "read contacts on acme-id",
		Payload: payload.ToMap(),
	}
}

// TestAwaitDecision_PostApproveDenial_RecordsDeniedOutcome pins the audit
// ledger to what HAPPENED rather than to what was clicked.
//
// The human approves, the post-approval authorization re-check then denies,
// and AwaitDecision returns approved=false — but it used to return there
// without recording any outcome, leaving the append-only, signed approval log
// holding a request with no verdict while the lifecycle log's
// DecisionResolved said Approved=true. An auditor reading that pair sees an
// allowed call that was never allowed.
//
// Here the re-check denies because no SpiceDB client is wired (authz's
// fail-closed Cli==nil branch), which is the same Result.IsError() path a
// genuine schema-semantics denial takes.
func TestAwaitDecision_PostApproveDenial_RecordsDeniedOutcome(t *testing.T) {
	orch := approval.New()
	mem := memory.NewLocal(inmem.NewBackend())
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		Mem:               mem,
		// AuthzCli deliberately nil: the post-approve re-check must fail.
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error {
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), gatedToolCallAsk())
	require.NoError(t, err, "PublishApproval")

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err, "AwaitDecision must not error; the denial is a verdict")
	assert.False(t, approved, "a post-approve re-check denial must surface as approved=false")
	assert.False(t, timedOut, "this is a denial, not a timeout")
	assert.Equal(t, "alice", by, "the approver identity is still reported")

	pair, err := memapproval.ByToolCall(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: "ns/s"}, "tu-postapprove")
	require.NoError(t, err, "ByToolCall")
	require.NotNil(t, pair.Outcome,
		"the ledger must carry an outcome: a request with no verdict alongside DecisionResolved{Approved:true} reads as an allowed call")
	assert.Equal(t, "denied", pair.Outcome.Decision,
		"the recorded decision must be what happened to the CALL, not what the approver clicked")
	assert.Equal(t, "alice", pair.Outcome.Approver)
	assert.Contains(t, pair.Outcome.Reason, "post-approval authorization re-check denied",
		"the reason must name the re-check so an auditor can tell this from an approver's own denial")
}
