package runner_test

// The child's mid-flight ask for DATA.
//
// request_input existed as a tool and RunnerEnv.RequestInput was declared, but
// nothing wired it at either site — so the tool was offered with a nil Record
// and refused every call. A capability an agent can see and never use is worse
// than one it is not offered: the model spends turns asking.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func inputRequestFixture(t *testing.T) (*runner.StatusPatcher, func() *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "demo"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	key := types.NamespacedName{Namespace: "demo", Name: "child"}
	return runner.NewStatusPatcher(c, key), func() *spiceboxv1alpha1.AgentSession {
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(context.Background(), key, &got))
		return &got
	}
}

// TestARequestIsRecordedForTheParentToSee.
func TestARequestIsRecordedForTheParentToSee(t *testing.T) {
	p, read := inputRequestFixture(t)

	n, err := p.RequestInput(context.Background(), "diff", "the task refers to a diff I was not given")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	ir := read().Status.InputRequest
	require.NotNil(t, ir)
	assert.Equal(t, "diff", ir.Slot)
	assert.Equal(t, "the task refers to a diff I was not given", ir.Why)
	assert.True(t, ir.Pending)
}

// TestTheExchangeNumberIsMonotonic.
//
// It is the one thing a parent has to tell a NEW request from the one it just
// answered. Derived by reading the stored value back rather than from a
// counter in memory, so it survives the child's pod being reaped mid-wait and
// re-hydrated — a number that restarted at 1 would make the second ask look
// like the first.
func TestTheExchangeNumberIsMonotonic(t *testing.T) {
	p, read := inputRequestFixture(t)

	first, err := p.RequestInput(context.Background(), "diff", "need the diff")
	require.NoError(t, err)
	second, err := p.RequestInput(context.Background(), "logs", "and the logs")
	require.NoError(t, err)

	assert.Equal(t, int64(1), first)
	assert.Equal(t, int64(2), second)
	ir := read().Status.InputRequest
	require.NotNil(t, ir)
	assert.Equal(t, int64(2), ir.Exchange)
	assert.Equal(t, "logs", ir.Slot, "the outstanding request is the latest one")
}

// TestALocalSessionRefusesRatherThanReportingSuccess.
//
// A kubectl-driven session has no AgentSession record to ask through and no
// parent to read it. Reporting success would tell the model its request is
// with someone, and it would then wait for data nobody will ever send.
func TestALocalSessionRefusesRatherThanReportingSuccess(t *testing.T) {
	p := runner.LocalStatusPatcher()

	_, err := p.RequestInput(context.Background(), "diff", "need it")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no AgentSession record")
}
