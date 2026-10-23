package subagentrequest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

type recordingSweeper struct {
	calls   []string
	slotErr error
}

func (s *recordingSweeper) DeleteSlotGrants(_ context.Context, ns, name string) error {
	s.calls = append(s.calls, "slot:"+ns+"/"+name)
	return s.slotErr
}

func (s *recordingSweeper) DeleteDataSlotGrants(_ context.Context, ns, name string) error {
	s.calls = append(s.calls, "data:"+ns+"/"+name)
	return nil
}

// The subagent child's name is swept of a dead predecessor's slot and
// data-slot tuples before it is created, and only then — an existing child is
// a retry and already holds the grants this controller wrote, so sweeping it
// would strip them. A failed sweep fails closed.
func TestSweepAbsentChild(t *testing.T) {
	child := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req-child"}}
	cases := []struct {
		name      string
		existing  []client.Object
		slotErr   error
		wantSwept bool
		wantErr   bool
		wantCalls []string
	}{
		{name: "absent child: both sweeps run, swept", wantSwept: true,
			wantCalls: []string{"slot:ns/req-child", "data:ns/req-child"}},
		{name: "existing child: never swept", existing: []client.Object{child.DeepCopy()}},
		{name: "sweep error: fails closed", slotErr: errors.New("spicedb down"), wantErr: true,
			wantCalls: []string{"slot:ns/req-child"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.existing...).Build()
			sw := &recordingSweeper{slotErr: tc.slotErr}
			r := &Reconciler{Client: c, SlotSweeper: sw}

			swept, err := r.sweepAbsentChild(context.Background(), child.DeepCopy())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantSwept, swept)
			assert.Equal(t, tc.wantCalls, sw.calls)
		})
	}
}
