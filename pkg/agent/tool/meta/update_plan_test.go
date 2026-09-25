package meta_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// decodeEnvelope decodes a fakeNATSPublish capture ("subject\n<json>") into the
// outbound Envelope. Used to assert the producer-stamped Seq + SessionUID.
func decodeEnvelope(t *testing.T, capture []byte) channelevents.Envelope {
	t.Helper()
	nl := bytes.IndexByte(capture, '\n')
	require.GreaterOrEqual(t, nl, 0, "capture lacks subject/data separator")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(capture[nl+1:], &env))
	return env
}

// notificationText decodes a fakeNATSPublish capture ("subject\n<json>"),
// requires it to be a KindNotification envelope, and returns its Text. Used to
// assert update_plan mirrors the in_progress item's label into the in-place
// status caption.
func notificationText(t *testing.T, capture []byte) string {
	t.Helper()
	nl := bytes.IndexByte(capture, '\n')
	require.GreaterOrEqual(t, nl, 0, "capture lacks subject/data separator")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(capture[nl+1:], &env))
	require.Equal(t, channelevents.KindNotification, env.Kind, "expected a notification envelope")
	var pl channelevents.NotificationPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	return pl.Text
}

type fakeNATSPublish struct {
	subjects [][]byte // each entry is one publish: [subject, data]
}

func (f *fakeNATSPublish) publish(_ context.Context, subject string, data []byte) error {
	out := []byte{}
	out = append(out, []byte(subject)...)
	out = append(out, '\n')
	out = append(out, data...)
	f.subjects = append(f.subjects, out)
	return nil
}

type fakeAppendNote struct {
	notes []map[string]any
}

func (f *fakeAppendNote) append(_ context.Context, content map[string]any) error {
	f.notes = append(f.notes, content)
	return nil
}

// newSessForUpdatePlanTest builds a SessionContext carrying the state Kinds
// this binary's imports registered from init(), the plans Kind among them.
func newSessForUpdatePlanTest(t *testing.T) *tool.SessionContext {
	t.Helper()
	noteCap := &fakeAppendNote{}
	ops := operations.New(nil, nil)
	reg := state.NewRegistry(state.Deps{Operations: ops, AppendSystemNote: noteCap.append})

	return &tool.SessionContext{
		Namespace:  "default",
		Name:       "sess1",
		Operations: ops,
		State:      reg,
	}
}

func TestUpdatePlanTool_Name(t *testing.T) {
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	require.Equal(t, "update_plan", tl.Name())
}

func TestUpdatePlanTool_RejectsMissingName(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"items":[]}`), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "name")
}

func TestUpdatePlanTool_RejectsDuplicateItemIDs(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	args := `{"name":"main","items":[
		{"id":"a","label":"A","status":"pending"},
		{"id":"a","label":"B","status":"pending"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "unique")
}

func TestUpdatePlanTool_RejectsTwoInProgress(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	args := `{"name":"main","items":[
		{"id":"a","label":"A","status":"in_progress"},
		{"id":"b","label":"B","status":"in_progress"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "in_progress")
}

func TestUpdatePlanTool_HappyPathReturnsDiffAndPublishes(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	args := `{"name":"main","items":[
		{"id":"a","label":"A","status":"in_progress"},
		{"id":"b","label":"B","status":"pending"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "got error result: %s", res.Content)
	require.True(t, res.Trusted, "update_plan is a framework meta tool and must opt out of content-guard inspection")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content), &body))
	require.Equal(t, "main", body["plan"])
	ip := body["in_progress"].(map[string]any)
	require.Equal(t, "a", ip["item_id"])
	require.NotEmpty(t, ip["operation_id"])

	// Two NATS publishes: the plan_update snapshot, then a status
	// notification mirroring the in_progress item's label ("A") into the
	// in-place caption (so a plan transition doubles as an update_status).
	require.Len(t, pubCap.subjects, 2)
	require.Contains(t, string(pubCap.subjects[0]), "ap.session.default.sess1.out.plan_update")
	require.Contains(t, string(pubCap.subjects[1]), "ap.session.default.sess1.out.notification")
	require.Equal(t, "A", notificationText(t, pubCap.subjects[1]),
		"status notification mirrors the in_progress label")

	// Operation was opened with the plan-item parent ref.
	op, ok := sess.Operations.Get(ip["operation_id"].(string))
	require.True(t, ok)
	require.NotNil(t, op.Parent)
	require.NotNil(t, op.Parent.PlanItem)
	require.Equal(t, "main", op.Parent.PlanItem.Plan)
	require.Equal(t, "a", op.Parent.PlanItem.Item)
}

func TestUpdatePlanStampsSeqOnCaptionNotification(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	// The per-tool-call IDs context (Task 4) carries the durable memory turn
	// index, the block index, and the session UID. update_plan must stamp
	// PackSeq(MemTurnIndex, BlockIndex) + SessionUID onto BOTH outbound
	// envelopes so the channelsd status state machine can order and dedup them.
	ctx := sandbox.WithIDs(context.Background(), sandbox.IDs{
		MemTurnIndex: 4, BlockIndex: 2, SessionUID: "uid-x",
	})
	args := `{"name":"main","items":[{"id":"a","label":"A","status":"in_progress"}]}`
	res, err := tl.Execute(ctx, json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	require.Len(t, pubCap.subjects, 2)
	planEnv := decodeEnvelope(t, pubCap.subjects[0])
	capEnv := decodeEnvelope(t, pubCap.subjects[1])

	require.Equal(t, channelevents.KindNotification, capEnv.Kind)
	require.Equal(t, channelevents.PackSeq(4, 2), capEnv.Seq, "caption notification carries the stamped Seq")
	require.Equal(t, "uid-x", capEnv.SessionUID, "caption notification carries the session UID")
	require.Equal(t, channelevents.PackSeq(4, 2), planEnv.Seq, "plan snapshot carries the stamped Seq")
	require.Equal(t, "uid-x", planEnv.SessionUID, "plan snapshot carries the session UID")
}

func TestUpdatePlanTool_MirrorsFullInProgressLabelToStatus(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	// A label longer than any channel's caption width. update_plan sends it
	// in FULL — clamping/fallback is the channel kind's job (e.g. Slack),
	// not this channel-agnostic layer's.
	long := strings.Repeat("x", 175) // schema caps label at 200
	args := fmt.Sprintf(`{"name":"main","items":[{"id":"a","label":%q,"status":"in_progress"}]}`, long)
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	require.Len(t, pubCap.subjects, 2)
	require.Equal(t, long, notificationText(t, pubCap.subjects[1]),
		"update_plan sends the FULL label; size guards belong in the channel kind")
}

func TestUpdatePlanTool_NoInProgressItemSkipsStatusNotification(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	// All pending: nothing is "happening right now", so there is no caption
	// to mirror — only the plan_update snapshot is published.
	args := `{"name":"main","items":[
		{"id":"a","label":"A","status":"pending"},
		{"id":"b","label":"B","status":"pending"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	require.Len(t, pubCap.subjects, 1, "no in_progress item ⇒ no status notification")
	require.Contains(t, string(pubCap.subjects[0]), "ap.session.default.sess1.out.plan_update")
}

func TestUpdatePlanTool_NilNATSPublishStillSucceeds(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: nil})

	args := `{"name":"main","items":[
		{"id":"a","label":"A","status":"pending"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)
}

func TestUpdatePlanTool_DeletePlanWithEmptyItems(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	// Create.
	_, err := tl.Execute(context.Background(),
		json.RawMessage(`{"name":"main","items":[{"id":"a","label":"A","status":"pending"}]}`),
		sess,
	)
	require.NoError(t, err)

	// Delete via empty items.
	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"name":"main","items":[]}`),
		sess,
	)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)

	// Plan is gone.
	_, ok := plans.From(sess).Get("main")
	require.False(t, ok)

	// Two publishes total (create + delete).
	require.Len(t, pubCap.subjects, 2)
}

func TestUpdatePlanTool_ParentItemWithoutPlanRejected(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	args := `{"name":"main","parent_item":"x","items":[
		{"id":"a","label":"A","status":"pending"}
	]}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "parent_plan")
}

func TestUpdatePlan_AcceptsErrorStatusAndDetailsOutput(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	pubCap := &fakeNATSPublish{}
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{NATSPublish: pubCap.publish})

	raw := json.RawMessage(`{
		"name":"main",
		"items":[
			{"id":"a","label":"A","status":"error","details":"why","output":"stderr"}
		]
	}`)
	res, err := tl.Execute(context.Background(), raw, sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "result=%q", res.Content)

	require.Len(t, pubCap.subjects, 1, "one envelope published")

	// fakeNATSPublish concatenates "subject\n<data>"; split on the first
	// newline to recover the envelope JSON.
	captured := pubCap.subjects[0]
	nl := -1
	for i, b := range captured {
		if b == '\n' {
			nl = i
			break
		}
	}
	require.GreaterOrEqual(t, nl, 0, "captured publish lacks subject/data separator")
	envBytes := captured[nl+1:]

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(envBytes, &env))

	var pl channelevents.PlanUpdatePayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	require.Len(t, pl.Items, 1)
	require.Equal(t, "error", pl.Items[0].Status)
	require.Equal(t, "why", pl.Items[0].Details)
	require.Equal(t, "stderr", pl.Items[0].Output)
}

func TestUpdatePlan_RejectsUnknownStatus(t *testing.T) {
	sess := newSessForUpdatePlanTest(t)
	tl := meta.NewUpdatePlan(meta.UpdatePlanConfig{})
	raw := json.RawMessage(`{
		"name":"main",
		"items":[{"id":"a","label":"A","status":"halfway"}]
	}`)
	res, err := tl.Execute(context.Background(), raw, sess)
	require.NoError(t, err)
	require.True(t, res.IsError, "unknown status must produce an IsError result; got %q", res.Content)
}

func TestPublishPlanSnapshot_BuildsPausedEnvelope(t *testing.T) {
	var captured channelevents.Envelope
	pub := func(_ context.Context, _ string, payload []byte) error {
		return json.Unmarshal(payload, &captured)
	}
	plan := plans.Plan{
		Name:  "main",
		Items: []plans.Item{{ID: "a", Label: "A", Status: plans.StatusInProgress}},
	}
	err := meta.PublishPlanSnapshot(context.Background(), pub, nil, "default", "sess1",
		plan, channelevents.PlanDiff{}, true, channelevents.PauseCauseRetry,
		channelevents.PackSeq(7, channelevents.SeqBlockEnd), "uid-y")
	require.NoError(t, err)

	require.Equal(t, channelevents.KindPlanUpdate, captured.Kind)
	require.Equal(t, channelevents.PackSeq(7, channelevents.SeqBlockEnd), captured.Seq)
	require.Equal(t, "uid-y", captured.SessionUID)
	var got channelevents.PlanUpdatePayload
	require.NoError(t, json.Unmarshal(captured.Payload, &got))
	require.Equal(t, "main", got.PlanName)
	require.True(t, got.Paused)
	require.Equal(t, "awaiting_retry", got.PauseCause)
	require.Len(t, got.Items, 1)
}

// silence unused-import linter (json/channelevents pulled in for clarity).
var _ = channelevents.KindPlanUpdate
