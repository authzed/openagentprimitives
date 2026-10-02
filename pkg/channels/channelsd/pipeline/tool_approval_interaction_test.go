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
	pins      map[string]string // keyed by "<ns>/<name>\x00<type>"
}

// Relations returns the recorder itself as the authz.RelWriter BindApproved
// writes through. The recorder also implements authz.SlotPinner: the approval
// binds a single-occupancy slot (empty occupancy defaults to single), which the
// GrantSlots gate refuses to write through a plain RelWriter. The pinned write
// records into the same `relations` slice as the plain path, so an assertion on
// the written tuple reads the same whichever path a binding took.
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

func slotGrantRecorderPinKey(scope authz.SessionRef, resourceType string) string {
	return scope.String() + "\x00" + resourceType
}

func (s *slotGrantRecorder) EnsurePin(_ context.Context, resourceType, resourceID string, scope authz.SessionRef) (bool, string, error) {
	if s.pins == nil {
		s.pins = map[string]string{}
	}
	key := slotGrantRecorderPinKey(scope, resourceType)
	if cur, ok := s.pins[key]; ok {
		return true, cur, nil
	}
	s.pins[key] = resourceID
	return false, resourceID, nil
}

func (s *slotGrantRecorder) WriteGrantsPinned(_ context.Context, rels []authz.Relation, _, _ string, _ authz.SessionRef) error {
	if s.fail {
		return errors.New("spicedb unavailable")
	}
	s.relations = append(s.relations, rels...)
	return nil
}

func (s *slotGrantRecorder) MovePin(_ context.Context, resourceType, _, toID string, _ []authz.Relation, scope authz.SessionRef) error {
	if s.pins == nil {
		s.pins = map[string]string{}
	}
	s.pins[slotGrantRecorderPinKey(scope, resourceType)] = toID
	return nil
}

func (s *slotGrantRecorder) ReadPin(_ context.Context, resourceType string, scope authz.SessionRef) (string, error) {
	return s.pins[slotGrantRecorderPinKey(scope, resourceType)], nil
}

func (s *slotGrantRecorder) ListGrantsFor(_ context.Context, _, _ string, _ authz.SessionRef) ([]authz.Relation, error) {
	return nil, nil
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

// TestToolApprovalDecision_CarriesOccupancyFromDetailsToTheGate proves the
// occupancy resolved at request-record time (ToolApprovalDetails.Occupancy)
// reaches the SlotBinding the handler binds, and so the GrantSlots gate: a
// "multi" record takes the plain unpinned write, while a single (empty) record
// pins the slot. If det.Occupancy stopped propagating onto the binding, the
// multi approval would pin — caught by the pin's presence, not by the grant
// merely landing.
func TestToolApprovalDecision_CarriesOccupancyFromDetailsToTheGate(t *testing.T) {
	decisionWith := func(occupancy string) channelinteractions.Decision {
		det, _ := json.Marshal(channelevents.ToolApprovalDetails{
			Permission: "apply", ResourceType: "label", ResourceID: "bug", ArgsHash: "h1",
			StateImpact: "readonly", Occupancy: occupancy,
		})
		return channelinteractions.Decision{
			Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
			Payload: channelevents.InteractionDecisionPayload{Category: "tool_approval", RequestRef: "req-occ", ActionID: "approve"},
			Request: &channelevents.InteractionRequestPayload{Category: "tool_approval", RequestRef: "req-occ", Details: det},
		}
	}

	t.Run("multi: binds without pinning", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), decisionWith("multi"))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.Empty(t, rec.pins, "a multi-occupancy approval must not pin the slot")
	})
	t.Run("single (empty): pins the slot", func(t *testing.T) {
		rec := &slotGrantRecorder{}
		_, err := toolApprovalHandler(approvalPipeline(t, rec))(context.Background(), decisionWith(""))
		require.NoError(t, err)
		require.Len(t, rec.relations, 1)
		assert.NotEmpty(t, rec.pins, "a single-occupancy approval (empty occupancy) must pin the slot")
	})
}

// TestToolApprovalDecision_pinRefusedRoutesToNewSession: when an approved JIT
// bind hits a single-occupancy slot already committed to a DIFFERENT instance,
// the tool_approval card has NO plan route to move the pin (it is the escalation
// path for classes with no plan gate). The handler must refuse with a message
// that names the target and routes to a NEW SESSION — and keep wrapping
// ErrSlotPinned so the decision pipe classifies it as a pin refusal, not a
// generic handler error — rather than GrantSlots' "propose an updated plan"
// advice, which this class cannot follow.
func TestToolApprovalDecision_pinRefusedRoutesToNewSession(t *testing.T) {
	rec := &slotGrantRecorder{}
	p := approvalPipeline(t, rec)
	// The slot's type is already pinned to a DIFFERENT instance for this session,
	// so the approved different-instance bind is refused by the pin.
	rec.pins = map[string]string{
		slotGrantRecorderPinKey(authz.SessionRef{Namespace: "default", Name: "demo-session"}, "repo"): "other-repo",
	}

	_, err := toolApprovalHandler(p)(context.Background(), toolDecision("approve", "readwrite"))
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrSlotPinned,
		"the refusal must stay errors.Is-able so the decision pipe routes it as a pin refusal, not a render error")
	assert.Contains(t, err.Error(), "start a new session",
		"a class with no plan gate can only move to a new target via a new session")
	assert.Contains(t, err.Error(), "repo:r1",
		"the refusal names the target this approval was for")
	assert.NotContains(t, err.Error(), "propose an updated plan",
		"a class with no plan gate must not be told to re-plan")
	assert.Empty(t, rec.relations, "a refused bind grants nothing")
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
