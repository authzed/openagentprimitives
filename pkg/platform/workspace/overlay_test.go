package workspace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOverlayCutJob(t *testing.T) {
	job := BuildOverlayCutJob(
		CPByPodConfig{Image: "busybox:1.36", ServiceAccount: "ap-snapshotter"},
		PVCRef{Namespace: "team-a", Name: "ws-base-demo-source"},
		PVCRef{Namespace: "team-a", Name: "sess1-workspace"},
		"tree",
	)
	assert.Equal(t, "team-a", job.Namespace)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	c := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{"sh"}, c.Command)
	require.Len(t, c.Args, 2)
	// reflink the checkout subtree (base/tree) into the session workspace root
	assert.Contains(t, c.Args[1], "cp -a --reflink=auto /base/tree/. /dst/")
	// base mounted read-only, dst read-write
	var baseRO, dstRW bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/base" && m.ReadOnly {
			baseRO = true
		}
		if m.MountPath == "/dst" && !m.ReadOnly {
			dstRW = true
		}
	}
	assert.True(t, baseRO, "base mounted read-only")
	assert.True(t, dstRW, "dst mounted read-write")
	// correct claim names
	names := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		names[v.Name] = v.PersistentVolumeClaim.ClaimName
	}
	assert.Equal(t, "ws-base-demo-source", names["base"])
	assert.Equal(t, "sess1-workspace", names["dst"])
	assert.False(t, strings.Contains(c.Args[1], ".."), "no traversal in the cp command")
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Greater(t, *job.Spec.ActiveDeadlineSeconds, int64(0))
}
