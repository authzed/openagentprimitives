package subagentrequest

// The exchange budget: the half of SubagentRequestSpec.Mode that says how many
// questions a delegation will carry from its child to its parent. `task` gets
// one, `chat` gets as many as it asks for, and `single_turn` never reaches this
// code at all.
//
// Its own file for the same reason controller_awaiting_parent_test.go is one:
// that file owns the resumable LIFECYCLE (park, answer, time out), this one
// owns the CEILING over it, and a change to either must fail on its own terms.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// budgetFixture seeds a conversational delegation in the given mode, already
// past the create step, plus its child carrying pe.
func budgetFixture(t *testing.T, mode string, pe *v1.ParentExchange) (*Reconciler, client.Client) {
	t.Helper()
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
	sr.Spec.Mode = mode
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase:          v1.AgentSessionPhaseRunning,
		ParentExchange: pe,
	})
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child,
	)
	return r, c
}

func getChild(t *testing.T, c client.Client, name string) v1.AgentSession {
	t.Helper()
	var got v1.AgentSession
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "ns", Name: name}, &got))
	return got
}

// askAndAnswer drives one full exchange against the fixture: the child records
// question n, the controller observes it, then the child clears Pending the way
// its runner does on resume and the controller observes that too. It returns
// the request as it stands after the pair.
func askAndAnswer(t *testing.T, r *Reconciler, c client.Client, n int64, question string) v1.SubagentRequest {
	t.Helper()
	setChildExchange(t, c, "req1-child", &v1.ParentExchange{Exchange: n, Pending: true, Question: question})
	reconcileOnce(t, r, "req1")
	asked := getRequest(t, c, "req1")

	setChildExchange(t, c, "req1-child", &v1.ParentExchange{Exchange: n, Pending: false, Question: question})
	reconcileOnce(t, r, "req1")
	require.Equal(t, v1.SubagentRequestPhaseRunning, getRequest(t, c, "req1").Status.Phase,
		"the child resumed, so the request must leave the awaiting state before the next question")
	return asked
}

func setChildExchange(t *testing.T, c client.Client, name string, pe *v1.ParentExchange) {
	t.Helper()
	child := getChild(t, c, name)
	child.Status.ParentExchange = pe
	require.NoError(t, c.Status().Update(t.Context(), &child))
}

// Brief test 1: a task child's FIRST question is an ordinary honoured
// exchange — the budget bounds the second one, not the first.
func TestReconcileChild_Task_FirstQuestionIsHonouredAndSpendsTheBudget(t *testing.T) {
	r, c := budgetFixture(t, v1.SubagentModeTask, &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "which of the two repos?",
	})

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, got.Status.Phase,
		"a task delegation carries one question, and this is it")
	assert.Equal(t, "which of the two repos?", got.Status.Message)
	require.NotNil(t, got.Status.ExchangesRemaining,
		"a bounded mode must record what is left, or nothing can refuse the next question")
	assert.Equal(t, int64(0), *got.Status.ExchangesRemaining,
		"one question is the whole budget, so honouring it spends it")
	assert.Equal(t, v1.AgentSessionPhaseRunning, getChild(t, c, "req1-child").Status.Phase,
		"an honoured question does not disturb the child")
}

// Brief test 2 (controller half): the SECOND question is refused, and the
// refusal is neither of the two things that would be wrong — the delegation is
// not ended and the child is not left advertising a question nothing will
// answer.
func TestReconcileChild_Task_SecondQuestionIsRefusedWithoutEndingTheDelegation(t *testing.T) {
	r, c := budgetFixture(t, v1.SubagentModeTask, nil)
	askAndAnswer(t, r, c, 1, "which of the two repos?")

	setChildExchange(t, c, "req1-child", &v1.ParentExchange{
		Exchange: 2, Pending: true, Question: "and which branch?",
	})
	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a refused exchange leaves the delegation running; the child keeps working")
	assert.NotEqual(t, v1.SubagentRequestPhaseAwaitingParent, got.Status.Phase,
		"the parent must never be shown a question the delegation refused to carry")
	assert.Equal(t, "which of the two repos?", got.Status.Message,
		"Message is the record of what was last HONOURED, not of what was refused")
	assert.Equal(t, int64(1), got.Status.Exchange, "the refused question was never carried, so it is not an exchange")
	assert.Nil(t, got.Status.AwaitingParentSince,
		"nothing is outstanding, so nothing is measured against the parent-reply bound")
	assert.Contains(t, got.Status.Determination, "refused")
	assert.Contains(t, got.Status.Determination, "task")

	assert.False(t, getChild(t, c, "req1-child").Status.ParentExchange.Pending,
		"the refused question must not stay pending — it is not coming back with an answer")
	assertExists(t, c, "req1-child", true, "a refusal must not throw away the work the child has already done")
}

// The refusal is IDEMPOTENT and self-limiting: a second reconcile over the same
// state has nothing left to refuse, because the first one cleared the question.
// Without that, every pass would re-refuse and the child would keep an
// outstanding question that nothing bounds.
func TestReconcileChild_Task_ARefusedExchangeIsNotRefusedAgain(t *testing.T) {
	r, c := budgetFixture(t, v1.SubagentModeTask, nil)
	askAndAnswer(t, r, c, 1, "q1")
	setChildExchange(t, c, "req1-child", &v1.ParentExchange{Exchange: 2, Pending: true, Question: "q2"})
	reconcileOnce(t, r, "req1")
	first := getRequest(t, c, "req1")

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
	assert.Equal(t, first.Status.Determination, got.Status.Determination,
		"a settled refusal must stop producing new determinations")
}

// The ceiling is measured against the CONTROLLER's own count, never against the
// number the child writes. A child that restates a spent exchange number is
// asking a second question by another name, and the party a ceiling constrains
// must not be able to widen it.
func TestReconcileChild_Task_AChildThatRewindsItsExchangeNumberIsStillRefused(t *testing.T) {
	r, c := budgetFixture(t, v1.SubagentModeTask, nil)
	askAndAnswer(t, r, c, 1, "which of the two repos?")

	// Not exchange 2 — exchange 1 again, with different words. Everything the
	// child authors says "this is the first question".
	setChildExchange(t, c, "req1-child", &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "and which branch?",
	})
	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"restating the exchange number must not buy a second question")
	assert.Equal(t, "which of the two repos?", got.Status.Message,
		"the parent must never see the words of a refused question")
}

// Brief test 3: chat is unbounded. Three further questions after the first are
// each honoured on their own terms, and nothing ever records a remaining count
// to refuse against.
func TestReconcileChild_Chat_EveryFurtherQuestionIsHonoured(t *testing.T) {
	r, c := budgetFixture(t, v1.SubagentModeChat, nil)

	for _, q := range []struct {
		n    int64
		text string
	}{{1, "first"}, {2, "second"}, {3, "third"}, {4, "fourth"}} {
		asked := askAndAnswer(t, r, c, q.n, q.text)
		assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, asked.Status.Phase,
			"chat question %d must reach the parent", q.n)
		assert.Equal(t, q.text, asked.Status.Message, "chat question %d must reach the parent verbatim", q.n)
		assert.Nil(t, asked.Status.ExchangesRemaining,
			"chat bounds no exchanges, so recording a remaining count would invent a ceiling it does not have")
	}
}

// Brief test 4: single_turn is untouched by the budget. Its child's
// parentExchange is discarded before the budget is ever consulted, so no count
// is recorded and the request never leaves Running — exactly as it did before
// the budget existed.
func TestReconcileChild_SingleTurn_BudgetNeverEngages(t *testing.T) {
	r, c := budgetFixture(t, "", &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "let me talk to you",
	})

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a single_turn delegation never becomes a conversation, whatever its child's status claims")
	assert.Nil(t, got.Status.ExchangesRemaining,
		"the discarded exchange must not be counted, or a later mode change would start from a spent budget")
	assert.True(t, getChild(t, c, "req1-child").Status.ParentExchange.Pending,
		"the single_turn guard ignores the field; it is not the refusal path and must not rewrite the child")
}

// ExchangeBudget is the whole mode table for this half of spec.mode, so its
// answers are pinned here rather than only observed through the reconciler.
func TestExchangeBudget_PerMode(t *testing.T) {
	cases := []struct {
		mode        string
		wantLimit   int64
		wantBounded bool
		why         string
	}{
		{v1.SubagentModeChat, 0, false, "a full back-and-forth bounds nothing"},
		{v1.SubagentModeTask, 1, true, "one bounded clarifying question"},
		{v1.SubagentModeSingleTurn, 0, true, "headless: there is no question to carry"},
		{"", 0, true, "empty resolves to single_turn"},
		{"nonsense", 0, true, "an unrecognized mode is bounded at zero, never unbounded"},
	}
	for _, tc := range cases {
		t.Run(tc.mode+": "+tc.why, func(t *testing.T) {
			sr := &v1.SubagentRequest{Spec: v1.SubagentRequestSpec{Mode: tc.mode}}
			limit, bounded := sr.ExchangeBudget()
			assert.Equal(t, tc.wantLimit, limit)
			assert.Equal(t, tc.wantBounded, bounded)
		})
	}
}
