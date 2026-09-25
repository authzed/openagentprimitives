package lifecycle

import "sort"

// ConditionView is a plain read-model condition with no k8s types. Callers that
// write AgentSession status translate these into metav1.Condition values.
type ConditionView struct {
	Type   string // matches a condition type constant in pkg/apis/v1alpha1/conditions.go
	Status string // "True" or "False"
	Reason string
}

// StatusView is the pure read-model derived from a folded State. It is a value
// type with no k8s, NATS, or time dependencies. The operator and channelsd map
// it onto CR status fields after calling Project.
type StatusView struct {
	Phase      string
	Conditions []ConditionView

	// ToolCallGated counts tool calls that were denied by a hook but not
	// surfaced to an approver. Non-zero values emit a ToolCallGated condition
	// so the count is readable from CR status without grepping logs.
	ToolCallGated int

	// ScopeReviewPending mirrors State.ScopeReviewPending: a cold-start
	// scope_review decision is outstanding and turn-0 execution is blocked.
	ScopeReviewPending bool

	// AwaitingUserInput mirrors State.AwaitingUserInput: the runner has
	// yielded inside a turn waiting for a follow-up user message. The pod
	// stays alive; the phase stays Running. The operator classifies this as
	// awaitingUserInputSince on the CR.
	AwaitingUserInput bool

	// FailureReason carries State.FailureReason when Phase == "Failed".
	FailureReason string

	// PendingByKind maps each decision kind to its outstanding request IDs.
	// The channelsd watchdog reads this to select the correct silence budget:
	// leakage_share requests use WaitLeakage policy while tool_call and join
	// use WaitVisible. Both produce phase AwaitingDecision, so coarsening to
	// just the phase would collapse the distinction and break the policy selection.
	PendingByKind map[DecisionKind][]string
}

// kindConditionType maps each DecisionKind to the AgentSession condition type
// string that signals it is pending. These strings intentionally match the
// constants in pkg/apis/v1alpha1/conditions.go; they are reproduced here as
// plain literals to keep this package free of k8s imports.
var kindConditionType = map[DecisionKind]string{
	DecisionToolCall:       "ToolApprovalPending",
	DecisionLeakageShare:   "InfoLeakageApprovalPending",
	DecisionContentInspect: "ContentInspectionApprovalPending",
	DecisionScopeReview:    "ScopeReviewPending",
	DecisionJoin:           "PermissionRequestPending",
}

// Project derives a StatusView from the folded State. It is pure and
// deterministic: the same State always produces the same StatusView.
func Project(s State) StatusView {
	v := StatusView{
		Phase:              string(s.Phase),
		ToolCallGated:      s.Gated,
		ScopeReviewPending: s.ScopeReviewPending,
		AwaitingUserInput:  s.AwaitingUserInput,
		FailureReason:      s.FailureReason,
	}

	// Build PendingByKind and derive one True condition per kind that appears
	// at least once in the pending set.
	if len(s.Pending) > 0 {
		v.PendingByKind = make(map[DecisionKind][]string)
		seen := make(map[DecisionKind]bool)
		for _, p := range s.Pending {
			v.PendingByKind[p.Kind] = append(v.PendingByKind[p.Kind], p.RequestID)
			seen[p.Kind] = true
		}
		for kind := range seen {
			if condType, ok := kindConditionType[kind]; ok {
				v.Conditions = append(v.Conditions, ConditionView{
					Type:   condType,
					Status: "True",
					Reason: "Pending",
				})
			}
		}
	}

	// Emit a ToolCallGated condition when hook-denied tool calls accumulate.
	// This makes the count visible on the CR without requiring operators to grep
	// logs — a no-silent-errors requirement for denied-but-not-approved calls.
	if s.Gated > 0 {
		v.Conditions = append(v.Conditions, ConditionView{
			Type:   "ToolCallGated",
			Status: "True",
			Reason: "HookDenied",
		})
	}

	// Sort conditions by Type so the output is deterministic. Without this,
	// iterating the `seen` map above produces random order, which thrashes
	// CR .status.conditions on every reconcile even when nothing changed.
	sort.Slice(v.Conditions, func(i, j int) bool {
		return v.Conditions[i].Type < v.Conditions[j].Type
	})

	return v
}
