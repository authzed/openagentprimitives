package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func budgetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// delegatedChildSession is a child the SubagentRequest controller created:
// spec.parent set, and the controller owner reference that names the request
// carrying the delegation's terms.
func delegatedChildSession(owned bool) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req1-child"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-parent"},
		},
	}
	if owned {
		s.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeGroupVersion.String(),
			Kind:       "SubagentRequest",
			Name:       "req1",
			Controller: ptrTrue(),
		}}
	}
	return s
}

func ptrTrue() *bool { b := true; return &b }

func requestWithRemaining(remaining *int64) *spiceboxv1alpha1.SubagentRequest {
	return &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req1"},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent: spiceboxv1alpha1.NamespacedRef{Namespace: "ns", Name: "demo-parent"},
			Class:  "demo-coder",
			Task:   "do the thing",
			Mode:   spiceboxv1alpha1.SubagentModeTask,
		},
		Status: spiceboxv1alpha1.SubagentRequestStatus{ExchangesRemaining: remaining},
	}
}

// The guard reads a COUNT the controller wrote and refuses only when it is
// spent. Each row is a state the child can genuinely be in, and the expected
// outcome is what its ask_parent call gets back.
func TestAskParentBudgetGuard(t *testing.T) {
	spent, left := int64(0), int64(1)
	cases := []struct {
		name       string
		remaining  *int64
		owned      bool
		wantRecord bool
		wantErr    error
	}{
		{
			name:       "budget left: the question is recorded as usual",
			remaining:  &left,
			owned:      true,
			wantRecord: true,
		},
		{
			name:       "no bound in force (an unbounded mode, or no exchange honoured yet): recorded",
			remaining:  nil,
			owned:      true,
			wantRecord: true,
		},
		{
			name:      "budget spent: refused before anything is written",
			remaining: &spent,
			owned:     true,
			wantErr:   meta.ErrExchangeBudgetSpent,
		},
		{
			name:      "the delegation cannot be read: refused, because what would permit it is what is missing",
			remaining: &left,
			owned:     false, // delegated, but the link to its terms is gone
			wantErr:   meta.ErrExchangeBudgetSpent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(budgetScheme(t)).
				WithObjects(requestWithRemaining(tc.remaining)).
				Build()

			var recorded []string
			guard := AskParentBudgetGuard(c, delegatedChildSession(tc.owned),
				func(_ context.Context, q string) (int64, error) {
					recorded = append(recorded, q)
					return int64(len(recorded)), nil
				})

			n, err := guard(context.Background(), "which branch?")

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Zero(t, n)
				assert.Empty(t, recorded, "a refused question must not be written to the child's status")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int64(1), n)
			assert.Equal(t, []string{"which branch?"}, recorded)
		})
	}
}

// A session nobody delegated to has no budget to spend, and the guard must not
// invent one. ask_parent is not offered to such a session, but the wrapper is
// wired unconditionally, so this is the shape that would break every
// non-delegated session if it fell through the wrong way.
func TestAskParentBudgetGuard_UndelegatedSession_PassesStraightThrough(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(budgetScheme(t)).Build()
	var recorded []string
	guard := AskParentBudgetGuard(c, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "plain"},
	}, func(_ context.Context, q string) (int64, error) {
		recorded = append(recorded, q)
		return 1, nil
	})

	_, err := guard(context.Background(), "q")

	require.NoError(t, err)
	assert.Equal(t, []string{"q"}, recorded)
}

// A delegated child with no reader wired fails CLOSED for the same reason a
// failed read does: what could not be established is exactly the thing that
// would have permitted the question. It must not panic on the nil reader on the
// way there.
func TestAskParentBudgetGuard_NoReaderWired_RefusesADelegatedChild(t *testing.T) {
	guard := AskParentBudgetGuard(nil, delegatedChildSession(true),
		func(context.Context, string) (int64, error) {
			t.Fatal("a question must not be recorded when the budget could not be read")
			return 0, nil
		})

	_, err := guard(context.Background(), "q")

	require.ErrorIs(t, err, meta.ErrExchangeBudgetSpent)
}

// A transient read failure fails CLOSED, and says so in words the child can act
// on: refusing costs it one question, while admitting one it should not have
// parks it until the delegation is ended and everything it has done is thrown
// away.
func TestAskParentBudgetGuard_ReadFailure_RefusesAndNamesTheCause(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	c := fake.NewClientBuilder().
		WithScheme(budgetScheme(t)).
		WithObjects(requestWithRemaining(nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).
		Build()

	guard := AskParentBudgetGuard(c, delegatedChildSession(true),
		func(context.Context, string) (int64, error) {
			t.Fatal("a question must not be recorded when the budget could not be read")
			return 0, nil
		})

	_, err := guard(context.Background(), "q")

	require.ErrorIs(t, err, meta.ErrExchangeBudgetSpent,
		"the tool renders this as a budget refusal, which is the only outcome that does not park the child")
	assert.ErrorIs(t, err, boom, "the underlying cause must survive for the log")
}
