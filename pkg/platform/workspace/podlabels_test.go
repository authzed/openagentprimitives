package workspace

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A NetworkPolicy selects PODS. These Jobs carried their component label on the
// Job's own ObjectMeta, which selects nothing, and their pod templates carried
// no labels at all — so no policy, this project's or a cluster operator's,
// could reach them. NetworkPolicy is an allow-union with no implicit default,
// which makes an unselectable pod unrestricted in BOTH directions on a fully
// enforcing CNI.
//
// The component is kept as the value rather than flattened to a boolean so a
// policy can tell the git-running reconcile Job apart from the local copy Jobs,
// which need no egress at all.
func TestJobPodMeta_CarriesASelectableComponentLabel(t *testing.T) {
	meta := jobPodMeta("workspace-reconcile", nil)

	assert.Equal(t, "workspace-reconcile", meta.Labels[LabelWorkspaceJob],
		"the marker carries the component, so a policy can name one job kind")
	assert.Equal(t, "workspace-reconcile", meta.Labels["app.kubernetes.io/component"],
		"the standard component label rides along on the pod, not only on the Job")
}

func TestJobPodMeta_MergesExtraLabelsWithoutLosingTheMarker(t *testing.T) {
	meta := jobPodMeta("workspace-snapshot", map[string]string{
		"agentprimitives.authzed.com/sessionUID": "uid-1",
	})

	assert.Equal(t, "uid-1", meta.Labels["agentprimitives.authzed.com/sessionUID"])
	assert.Equal(t, "workspace-snapshot", meta.Labels[LabelWorkspaceJob])
}

// Every workspace Job's pod must be selectable. A new Job added later that
// builds its template by hand is the failure this pins: it runs fine, it is
// simply outside every policy, and nothing else says so.
func TestEveryWorkspaceJobPodTemplateIsLabelled(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
	}{
		{"snapshot", jobPodMeta("workspace-snapshot", nil).Labels},
		{"restore", jobPodMeta("workspace-restore", nil).Labels},
		{"snapshot-gc", jobPodMeta("workspace-snapshot-gc", nil).Labels},
		{"overlay-cut", jobPodMeta("workspace-overlay-cut", nil).Labels},
		{"reconcile", jobPodMeta("workspace-reconcile", nil).Labels},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, tc.meta[LabelWorkspaceJob], "pod must be selectable by a NetworkPolicy")
		})
	}
}
