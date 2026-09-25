package v1alpha1

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsApprovalCondition(t *testing.T) {
	assert.True(t, IsApprovalCondition(AgentSessionConditionToolApprovalPending))
	assert.True(t, IsApprovalCondition(AgentSessionConditionPermissionRequestPending))
	assert.False(t, IsApprovalCondition(AgentSessionConditionRunnerReady))
	assert.False(t, IsApprovalCondition(AgentSessionConditionIdle))
	// The channelsd-owned bookkeeping conditions ARE approval-owned (channelsd
	// writes them via SSA) even though they are not "awaiting a human decision".
	assert.True(t, IsApprovalCondition(AgentSessionConditionInteractPolicyApplied))
	assert.True(t, IsApprovalCondition(AgentSessionConditionCredentialRequestPublished))
}

// TestIsPendingApprovalCondition pins the strict subset of channelsd-owned
// conditions that genuinely mean "awaiting a human approval decision". The
// bookkeeping/lifecycle conditions (InteractPolicyApplied, CredentialRequestPublished)
// are channelsd-owned (IsApprovalCondition=true) but must NOT count as pending
// approval — otherwise a session with no interact policy
// (InteractPolicyApplied=True/NotConfigured) is stranded in AwaitingApproval.
func TestIsPendingApprovalCondition(t *testing.T) {
	assert.True(t, IsPendingApprovalCondition(AgentSessionConditionPermissionRequestPending))
	assert.True(t, IsPendingApprovalCondition(AgentSessionConditionToolApprovalPending))
	assert.True(t, IsPendingApprovalCondition(AgentSessionConditionInfoLeakageApprovalPending))
	// Bookkeeping conditions: channelsd-owned but NOT pending approval.
	assert.False(t, IsPendingApprovalCondition(AgentSessionConditionInteractPolicyApplied))
	assert.False(t, IsPendingApprovalCondition(AgentSessionConditionCredentialRequestPublished))
}

func TestApprovalConditionsActive(t *testing.T) {
	condTrue := func(typ string) *AgentSession {
		return &AgentSession{Status: AgentSessionStatus{Conditions: []metav1.Condition{{
			Type: typ, Status: metav1.ConditionTrue,
		}}}}
	}
	cases := []struct {
		name string
		sess *AgentSession
		want bool
	}{
		{"empty session: not active", &AgentSession{}, false},
		{
			"non-empty pending interactions queue: active",
			&AgentSession{Status: AgentSessionStatus{PendingInteractions: []PendingInteraction{{}}}},
			true,
		},
		{
			"non-empty pending requesters queue: active",
			&AgentSession{Status: AgentSessionStatus{PendingRequesters: []PendingRequester{{}}}},
			true,
		},
		{"PermissionRequestPending=True: active", condTrue(AgentSessionConditionPermissionRequestPending), true},
		{"ToolApprovalPending=True: active", condTrue(AgentSessionConditionToolApprovalPending), true},
		{"InfoLeakageApprovalPending=True: active", condTrue(AgentSessionConditionInfoLeakageApprovalPending), true},
		{
			"PermissionRequestPending=False with no queue: not active",
			&AgentSession{Status: AgentSessionStatus{Conditions: []metav1.Condition{{
				Type: AgentSessionConditionPermissionRequestPending, Status: metav1.ConditionFalse,
			}}}},
			false,
		},
		// Regression: a session with NO interact policy gets InteractPolicyApplied=True
		// (reason NotConfigured) at creation. That is channelsd bookkeeping, NOT a human
		// approval — it must not strand the session in AwaitingApproval.
		{"InteractPolicyApplied=True (NotConfigured) with no queue: not active", condTrue(AgentSessionConditionInteractPolicyApplied), false},
		// Regression: CredentialRequestPublished is a dedup flag for the separate
		// AwaitingCredentials phase, not an awaiting-approval condition.
		{"CredentialRequestPublished=True with no queue: not active", condTrue(AgentSessionConditionCredentialRequestPublished), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ApprovalConditionsActive(tc.sess))
		})
	}
}

// TestPendingRestartIsCoOwned verifies that pendingRestart is classified as
// co-owned: channelsd sets it (RestartTrigger path), operator clears it
// (ReconcileRestart/clearPendingRestart). It must NOT be in approvalOwnedStatusFields
// alone, because the operator writes it too.
// RED before coOwnedStatusFields is added and populated.
func TestPendingRestartIsCoOwned(t *testing.T) {
	assert.True(t, coOwnedStatusFields["pendingRestart"],
		"pendingRestart is co-owned (channelsd sets, operator clears) — "+
			"must be in coOwnedStatusFields, not approvalOwnedStatusFields alone")
	assert.False(t, approvalOwnedStatusFields["pendingRestart"],
		"pendingRestart must be removed from approvalOwnedStatusFields once co-owned")
}

// serializedStatusField is one field of AgentSessionStatus as it reaches the
// CRD schema: its json name, plus the Go path that produced it.
type serializedStatusField struct {
	goPath string
	json   string
}

// serializedStatusFields returns every json field a status struct serializes,
// FLATTENING inlined and embedded members the way controller-gen flattens them
// into the CRD schema. The second return is the Go paths of fields that are
// serialized but cannot be named.
//
// The obvious walk is one non-recursive pass that `continue`s on a field with
// no json name — and that silently exempts exactly the construct whose fields
// cannot be named individually. controller-gen flattens an inlined struct into
// the parent's schema, so its fields are all serialized and all real, yet a
// skipping walk classifies none of them and the suite stays green. Inline
// embedding is idiomatic in this API (a shared SettingsStatus across two CRDs),
// so this is a reachable growth path. Recurse instead, and REPORT what still
// cannot be named rather than skipping it.
//
// A near-identical walker lives in pkg/controllers/webhooks/agentsession's test, guarding
// the runner-forgery partition over the same type. It is duplicated rather than
// shared because this one is an internal test (package v1alpha1), so importing
// any package that itself imports v1alpha1 would be a cycle — the same trade
// the local copy of jsonFieldName in that file already makes.
func serializedStatusFields(t reflect.Type) (fields []serializedStatusField, unnameable []string) {
	return walkSerializedStatusFields(t, nil)
}

func walkSerializedStatusFields(t reflect.Type, onPath []reflect.Type) (fields []serializedStatusField, unnameable []string) {
	onPath = append(onPath, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue // genuinely never serialized; the one safe skip
		}
		if name := jsonFieldName(f); name != "" {
			fields = append(fields, serializedStatusField{goPath: f.Name, json: name})
			continue
		}
		_, opts, _ := strings.Cut(tag, ",")
		if !f.Anonymous && !slices.Contains(strings.Split(opts, ","), "inline") {
			// A named field with no json tag: encoding/json still serializes it
			// under its Go name, so it is real and must be classified.
			unnameable = append(unnameable, f.Name)
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct || slices.Contains(onPath, ft) {
			unnameable = append(unnameable, f.Name)
			continue
		}
		inner, innerUnnameable := walkSerializedStatusFields(ft, onPath)
		for _, sf := range inner {
			fields = append(fields, serializedStatusField{goPath: f.Name + "." + sf.goPath, json: sf.json})
		}
		for _, u := range innerUnnameable {
			unnameable = append(unnameable, f.Name+"."+u)
		}
	}
	return fields, unnameable
}

// TestSerializedStatusFieldsFlattensInlinedMembers drives the walk itself,
// because AgentSessionStatus has no inlined member TODAY — so nothing in
// TestEveryStatusFieldHasExactlyOneOwner can show whether the walk would
// classify one or quietly skip it.
func TestSerializedStatusFieldsFlattensInlinedMembers(t *testing.T) {
	got, unnameable := serializedStatusFields(reflect.TypeOf(inlineProbeStatus{}))

	byJSON := map[string]string{}
	for _, f := range got {
		byJSON[f.json] = f.goPath
	}

	assert.Contains(t, byJSON, "phase", "a plainly-tagged field must be returned")
	assert.Contains(t, byJSON, "sharedCeiling",
		"a field of a `json:\",inline\"` member is flattened into the CRD schema, so it is a real status field with a real "+
			"owner; skipping the member leaves every field it carries unclassified with the suite green")
	assert.Contains(t, byJSON, "embeddedCeiling",
		"an anonymous member with no json tag is flattened by encoding/json and by controller-gen alike; it must be walked too")
	assert.Equal(t, "InlineProbeShared.SharedCeiling", byJSON["sharedCeiling"],
		"the Go path must name the member the field came from, so a failure says where to look")
	assert.NotContains(t, byJSON, "-", "a json:\"-\" field is never serialized and must not be returned")

	assert.Equal(t, []string{"Untagged"}, unnameable,
		"a named field with no json tag is still serialized under its Go name, so it must be REPORTED rather than skipped")
}

// inlineProbe* are fixtures for TestSerializedStatusFieldsFlattensInlinedMembers
// only. Exported names so the embedded field names are exported, matching the
// shape a real API type would have.
type InlineProbeShared struct {
	SharedCeiling []string `json:"sharedCeiling,omitempty"`
}

type InlineProbeEmbedded struct {
	EmbeddedCeiling string `json:"embeddedCeiling,omitempty"`
}

type inlineProbeStatus struct {
	Phase             string `json:"phase,omitempty"`
	InlineProbeShared `json:",inline"`
	InlineProbeEmbedded
	Untagged string
	Skipped  string `json:"-"`
}

// TestEveryStatusFieldHasExactlyOneOwner is the drift guard: it reflects over
// AgentSessionStatus and fails if any field (other than the per-type-owned
// `conditions`) is not classified into exactly one ownership map. Adding a new
// status field therefore forces the author to assign its owner. Co-owned fields
// (written by both operator and channelsd) belong in coOwnedStatusFields; they
// must be absent from the other two maps.
func TestEveryStatusFieldHasExactlyOneOwner(t *testing.T) {
	live, unnameable := serializedStatusFields(reflect.TypeOf(AgentSessionStatus{}))
	assert.Emptyf(t, unnameable,
		"AgentSessionStatus has serialized field(s) %v with no json name, so this guard cannot assign them an owner. Give each "+
			"a json tag (or `json:\"-\"` if it must not serialize). Reporting rather than skipping is deliberate: the skip is "+
			"what let an inlined member's fields reach the CRD schema unclassified", unnameable)

	for _, f := range live {
		if f.json == "conditions" {
			continue // owned per condition TYPE, not as a whole field
		}
		op, ap, co := operatorOwnedStatusFields[f.json], approvalOwnedStatusFields[f.json], coOwnedStatusFields[f.json]
		inExactlyOne := (op && !ap && !co) || (!op && ap && !co) || (!op && !ap && co)
		assert.Truef(t, inExactlyOne,
			"AgentSessionStatus.%s (json %q) must be classified in exactly one of "+
				"{operatorOwnedStatusFields, approvalOwnedStatusFields, coOwnedStatusFields} "+
				"(operator=%v approvals=%v co-owned=%v) — classify it in agentsession_ownership.go",
			f.goPath, f.json, op, ap, co)
	}
}
