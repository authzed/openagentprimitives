package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParentRef(t *testing.T) {
	cases := []struct {
		name    string
		sess    AgentSession
		wantOK  bool
		wantRef NamespacedRef
	}{
		{
			name:   "no parent: root session",
			sess:   AgentSession{},
			wantOK: false,
		},
		{
			name: "parent set: delegated child",
			sess: AgentSession{Spec: AgentSessionSpec{
				Parent: &NamespacedRef{Namespace: "ns", Name: "demo-parent"},
			}},
			wantOK:  true,
			wantRef: NamespacedRef{Namespace: "ns", Name: "demo-parent"},
		},
		{
			name:   "forkedFrom is NOT a parent",
			sess:   AgentSession{Spec: AgentSessionSpec{ForkedFrom: "demo-earlier"}},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.sess.ParentRef()
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantRef, got)
			}
		})
	}

	// The table above only ever constructs an AgentSession value, so the
	// s == nil branch in ParentRef is never exercised by it. A nil *AgentSession
	// is a real caller shape (a failed Get, an unset lookup result), so it needs
	// its own case rather than being an untested guard.
	t.Run("nil receiver: false, zero value", func(t *testing.T) {
		var sess *AgentSession
		got, ok := sess.ParentRef()
		assert.False(t, ok)
		assert.Equal(t, NamespacedRef{}, got)
	})
}
