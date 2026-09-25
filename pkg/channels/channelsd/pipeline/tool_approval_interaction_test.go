package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/grants"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// recordingGrantWriter satisfies the real grants.Writer surface
// (WriteRelationships / DeleteRelationships) — the same interface p.GrantWriter
// carries and grants.WriteToolGrant TOUCHes into. It records each
// WriteRelationships request so the test can decode the exact tuple the handler
// built. (The interface is NOT a WriteToolGrant method — WriteToolGrant is a
// free function over a grants.Writer.)
type recordingGrantWriter struct {
	writes []*v1.WriteRelationshipsRequest
}

// Both fakes must satisfy the real grants.Writer — the same interface
// p.GrantWriter carries — or toolApprovalHandler(gw grants.Writer, …) won't take
// them. A drift in grants.Writer breaks this at compile time.
var (
	_ grants.Writer = (*recordingGrantWriter)(nil)
	_ grants.Writer = failingGrantWriter{}
)

func (w *recordingGrantWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	w.writes = append(w.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}
func (w *recordingGrantWriter) DeleteRelationships(_ context.Context, _ *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	return &v1.DeleteRelationshipsResponse{}, nil
}

// failingGrantWriter fails the write so grants.WriteToolGrant returns an error,
// exercising the handler's error path (the pipe surfaces it via D4 render-error).
type failingGrantWriter struct{}

func (failingGrantWriter) WriteRelationships(_ context.Context, _ *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	return nil, context.DeadlineExceeded
}
func (failingGrantWriter) DeleteRelationships(_ context.Context, _ *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	return nil, nil
}

// decodeGrantWrite pulls the grant-tuple fields the handler stamped out of the
// recorded SpiceDB relationship: the grant subject IS <ResourceType>:<ResourceID>,
// the caveat context carries allowed_arguments_hash=<ArgsHash>, and
// OptionalExpiresAt encodes the effective TTL horizon.
func decodeGrantWrite(t *testing.T, req *v1.WriteRelationshipsRequest) (resType, resID, argsHash string, expiresAt time.Time) {
	t.Helper()
	require.Len(t, req.GetUpdates(), 1)
	rel := req.GetUpdates()[0].GetRelationship()
	resType = rel.GetSubject().GetObject().GetObjectType()
	resID = rel.GetSubject().GetObject().GetObjectId()
	argsHash = rel.GetOptionalCaveat().GetContext().GetFields()["allowed_arguments_hash"].GetStringValue()
	require.NotNil(t, rel.GetOptionalExpiresAt(), "grant tuple must always carry an expiry")
	expiresAt = rel.GetOptionalExpiresAt().AsTime()
	return
}

func toolDecision(actionID, stateImpact string) channelinteractions.Decision {
	det, _ := json.Marshal(channelevents.ToolApprovalDetails{
		Permission: "write", ResourceType: "repo", ResourceID: "r1", ArgsHash: "h1", StateImpact: stateImpact,
	})
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{Category: "tool_approval", RequestRef: "req-1", ActionID: actionID},
		Request: &channelevents.InteractionRequestPayload{Category: "tool_approval", RequestRef: "req-1", Details: det},
	}
}

// slotGrantRecorder captures what an approval bound, so a test can assert the
// tuple SHAPE rather than merely that something was written.
//
// It records through Relations(), not GrantSlots: the handler now binds via
// authz.BindApproved, which writes through the authz.RelWriter the pipeline's
// Relations() method hands it — GrantSlots on the Authz interface itself is
// no longer on this call path (it stays wired for other callers, e.g. thread
// adoption, via the embedded fakeAuthz).
type slotGrantRecorder struct {
	fakeAuthz
	fail      bool
	relations []authz.Relation
}

// Relations returns the recorder itself as the authz.RelWriter BindApproved
// writes through.
func (s *slotGrantRecorder) Relations() authz.RelWriter { return s }

func (s *slotGrantRecorder) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	if s.fail {
		return errors.New("spicedb unavailable")
	}
	s.relations = append(s.relations, rels...)
	return nil
}

func (s *slotGrantRecorder) DeleteRelationships(_ context.Context, _ []authz.Relation) error {
	return nil
}

// approvalPipeline builds the pipeline fixture the tool-approval tests share.
// MemReader is a real in-process memory.Memory (not a fake): BindApproved
// narrows session_scope through sessionscope.Get/Put, which needs a working
// query/put round-trip, not just a recorder.
func approvalPipeline(t *testing.T, rec *slotGrantRecorder) *Pipeline {
	t.Helper()
	return &Pipeline{Authz: rec, Now: func() time.Time { return time.Now() }, Mem: newTestMemory(t)}
}

func TestToolApprovalHandler(t *testing.T) {
	// Approving binds a SLOT grant — the tuple on the RESOURCE pointing at the
	// session — and carries the PERMISSION, which is what keeps an approved read
	// from becoming a write on the same instance.
	t.Run("approve: binds one slot grant carrying resource, id and permission", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		out, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("approve", "readwrite"))
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeApproved, out.Result)
		require.Len(t, rec.relations, 1)
		rel := rec.relations[0]
		assert.Equal(t, "repo", rel.ResourceType)
		assert.Equal(t, "r1", rel.ResourceID)
		assert.Equal(t, authz.SlotGrantRelationName("write"), rel.Relation)
		assert.Equal(t, "agentsession", rel.SubjectType)
		assert.Equal(t, "default/demo-session", rel.SubjectID)
	})
	t.Run("approve external: keeps the short 30s leash, not the session-wide horizon", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("approve", "external"))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.WithinDuration(t, time.Now().Add(30*time.Second), rec.relations[0].ExpiresAt, time.Minute,
			"an external effect is approved for the moment, not for the session")
	})
	t.Run("approve non-external: bounded by the session horizon, never unbounded", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("approve", "readwrite"))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.False(t, rec.relations[0].ExpiresAt.IsZero(), "a slot grant with no expiry is the leak the expiry exists to prevent")
		assert.True(t, rec.relations[0].ExpiresAt.After(time.Now().Add(time.Hour)),
			"and it must outlast the call that prompted it")
	})
	t.Run("deny writes nothing, OutcomeDenied", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		out, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("deny", "external"))
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeDenied, out.Result)
		assert.Empty(t, rec.relations)
	})
	t.Run("grant-write failure returns error (pipe surfaces render-error via D4)", func(t *testing.T) {
		rec := &slotGrantRecorder{fail: true}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("approve", "external"))
		require.Error(t, err)
	})
	t.Run("unknown actionID returns error", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), toolDecision("bogus", "external"))
		require.Error(t, err)
		assert.Empty(t, rec.relations)
	})
}

// crmCompanyDecision builds an approve decision for crm_company:4210 — the
// resource-owner "who owns this company" flow the JIT tool-approval path
// gates. A separate fixture value (not a toolDecision variant) because the
// scope-narrowing assertion below reads back by resourceType, and repo/r1
// would collide with the fixed session_scope key TestToolApprovalHandler's
// cases already exercise if they ever ran against a shared scope.
func crmCompanyDecision() channelinteractions.Decision {
	det, _ := json.Marshal(channelevents.ToolApprovalDetails{
		Permission: "view", ResourceType: "crm_company", ResourceID: "4210", ArgsHash: "h1", StateImpact: "readonly",
	})
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{Category: "tool_approval", RequestRef: "req-2", ActionID: "approve"},
		Request: &channelevents.InteractionRequestPayload{Category: "tool_approval", RequestRef: "req-2", Details: det},
	}
}

// The JIT approval is a BINDING: a human was shown one company and said yes to
// it. Recording that as a grant alone leaves the others merely un-granted
// rather than excluded, so the card's promise ("this company") is weaker than
// it reads.
func TestToolApprovalDecision_narrowsScopeToTheApprovedInstance(t *testing.T) {
	rec := &slotGrantRecorder{}
	p := approvalPipeline(t, rec)

	out, err := toolApprovalHandler(p)(context.Background(), crmCompanyDecision())
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	got, _, err := sessionscope.Get(
		memory.WithSystemApproval(context.Background(), "test"),
		p.Mem, memory.Scope{Kind: "session", ID: "default/demo-session"})
	require.NoError(t, err)

	var ids []string
	for _, r := range got.Resources {
		if r.ResourceType == "crm_company" {
			ids = append(ids, r.IDs...) // ScopeResource.IDs is a []string
		}
	}
	assert.Equal(t, []string{"4210"}, ids,
		"approving this company must exclude the others, not merely permit this one")
}

// TestToolApprovalHandler_ResourcelessCall pins the case observed live on
// oap-desktop: the builder called workshop_apply — a sidecar tool with no
// resource handle, so its approval details carry no permission, type or id
// — a human clicked Approve, and the handler tried to bind a slot grant from
// three empty fields. GrantSlots refuses that (rightly: an empty tuple names
// nothing), the refusal became the decision's error, the click was rejected,
// and the session stayed parked on an approval that had already been given.
// A call that names no instance has no slot to bind: the approval record is
// the whole authorization, and the decision must land.
func TestToolApprovalHandler_ResourcelessCall(t *testing.T) {
	det, _ := json.Marshal(channelevents.ToolApprovalDetails{ToolName: "workshop_apply", StateImpact: "external", ArgsHash: "h1"})
	d := channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Payload: channelevents.InteractionDecisionPayload{Category: "tool_approval", RequestRef: "req-3", ActionID: "approve"},
		Request: &channelevents.InteractionRequestPayload{Category: "tool_approval", RequestRef: "req-3", Details: det},
	}
	rec := &slotGrantRecorder{}
	out, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), d)
	require.NoError(t, err, "a human's approval of a call that names no instance must land")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)
	assert.Empty(t, rec.relations, "no instance means no slot grant; nothing to point a tuple at and nothing to exclude")
}
