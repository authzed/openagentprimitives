package toolcall

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestWorkspaceIsIsolated gates the pre-dispatch snapshot: an isolated bundle
// session has no shared <session>-workspace PVC, so launching a snapshot Job
// that mounts one leaves the Job's pod unschedulable
// ("persistentvolumeclaim ...-workspace not found") forever, blocking every
// stateful tool call for 3m before erroring. When the workspace is isolated the
// controller must skip the snapshot entirely rather than hang.
func TestWorkspaceIsIsolated(t *testing.T) {
	cases := []struct {
		name string
		ws   spiceboxv1alpha1.WorkspaceConfig
		want bool
	}{
		{
			name: "shared with a claim: snapshot the PVC (not isolated)",
			ws:   spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceShared, SharedClaimName: "as-1-workspace"},
			want: false,
		},
		{
			name: "explicit isolated mode: no PVC to snapshot",
			ws:   spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceIsolated},
			want: true,
		},
		{
			name: "unset mode: treated as isolated (no shared claim)",
			ws:   spiceboxv1alpha1.WorkspaceConfig{},
			want: true,
		},
		{
			name: "shared mode but empty claim: nothing to snapshot",
			ws:   spiceboxv1alpha1.WorkspaceConfig{Mode: spiceboxv1alpha1.WorkspaceShared},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.SpiceboxSession{}
			sess.Spec.Workspace = tc.ws
			assert.Equal(t, tc.want, workspaceIsIsolated(sess))
		})
	}
}
