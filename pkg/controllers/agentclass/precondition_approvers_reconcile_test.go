// pkg/controllers/agentclass/precondition_approvers_reconcile_test.go
//
// Untagged (unit tier): drives the real Reconcile against a fake client to pin
// the CALL SITE of validatePreconditionApprovers, not just the function. The
// direct-function test in precondition_approvers_test.go proves the rule; this
// proves the reconciler actually invokes it, so deleting the call at
// controller.go regresses a scenario rather than passing silently.
//
// The reconcile-reachable shape is a precondition that SETS an approvers list
// naming no subject (a blank entry). An explicit `approvers: []` cannot be used:
// the CRD field is omitempty, so an empty list serializes away and reaches the
// reconciler as unset. A blank ENTRY ("") survives the round-trip, and — because
// a set list overrides the standing default — it is refused even though the
// slot's type carries a declared standing, which is what keeps the class
// otherwise-admitted and this rule the ONLY refuser.
package agentclass_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// preconditionApproverClass builds an otherwise-admitted class: a gatedType slot
// (which declaringServer classifies with a session-only standing, so
// resolveSlotValueKeying admits it) whose precondition reads a fact a producer
// gated ELSEWHERE records (so resolveFactSources admits it). The ONLY thing the
// caller varies is the precondition's approvers, so whatever the reconcile
// verdict is, this rule decided it.
func preconditionApproverClass(name string, approvers []string) (*spiceboxv1alpha1.AgentClass, *spiceboxv1alpha1.MCPServer) {
	ac := requiringClass(name, "review_is_clean")
	ac.Spec.Authz.Slots[0].Requires[0].Approvers = approvers
	srv := declaringServer(name+"-srv",
		observingTool("check_review", unrelatedTyp, "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: srv.Name, Ref: srv.Name}}
	return ac, srv
}

// A precondition whose SET approvers name no subject makes the waiver
// unroutable, and the reconciler must refuse the class for it. Reconcile rather
// than the resolver alone: a check the reconciler forgets to call passes every
// direct-function test and still ships a class that crashes at first dispatch —
// the exact gap this row exists to close.
func TestReconcile_RefusesAPreconditionWithBlankApprovers(t *testing.T) {
	ac, srv := preconditionApproverClass("precond-approver-blank", []string{""})

	got := reconcileClass(t, ac, srv)

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "the class must carry a Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a precondition whose approvers name no subject must refuse the class; message=%s", cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotPreconditionUnroutable, cond.Reason)

	// The unique message substring, not the reason alone: several rules in this
	// controller reject with a slot-shaped reason, so a reason-only assertion
	// would pass with the detection deleted. This phrase appears nowhere else.
	assert.Contains(t, cond.Message, "waiver card no subject is authorized to answer",
		"the message must say WHY the waiver is unroutable")
	assert.Contains(t, cond.Message, "authz.slots[code_review].requires[0]",
		"address the entry the author wrote")
}

// The same fixture with a real approver subject reconciles to a VALID class:
// the gate fires selectively on the approver routing, not on the mere presence
// of a precondition. This is what proves the refusal above is discriminating —
// the fixture is otherwise-admitted, and only the blank approvers made it fail.
func TestReconcile_AdmitsAPreconditionWithRealApprovers(t *testing.T) {
	ac, srv := preconditionApproverClass("precond-approver-ok",
		[]string{"agentsession:default/precond-approver-ok#approve"})

	got := reconcileClass(t, ac, srv)

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a routable precondition must not fail the class; message=%s", cond.Message)
}
