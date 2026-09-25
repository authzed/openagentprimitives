package workspace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

func TestBuildReconcileJob(t *testing.T) {
	cmds := []workspacekinds.Command{{Dir: "/workspace", Env: []string{"GIT_TERMINAL_PROMPT=0"}, Argv: []string{"git", "push", "origin", "HEAD:main"}}}
	job := BuildReconcileJob(CPByPodConfig{Namespace: "team-a", Image: "alpine/git", ServiceAccount: "ap-snapshotter"},
		"ws-apply-sess1", "sess1-workspace", cmds, "WORKSPACE_GIT_TOKEN", "sess1-push-creds", "git-token", 600)

	assert.Equal(t, "team-a", job.Namespace)
	require.Len(t, job.Spec.Template.Spec.InitContainers, 1)
	ic := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, []string{"git"}, ic.Command)
	assert.Equal(t, []string{"push", "origin", "HEAD:main"}, ic.Args)
	assert.Equal(t, "/workspace", ic.VolumeMounts[0].MountPath)
	require.Len(t, ic.Env, 2)
	assert.Equal(t, "GIT_TERMINAL_PROMPT", ic.Env[0].Name)
	credEnv := ic.Env[1]
	assert.Equal(t, "WORKSPACE_GIT_TOKEN", credEnv.Name)
	require.NotNil(t, credEnv.ValueFrom, "cred env must be sourced from the Secret, not a literal value")
	require.NotNil(t, credEnv.ValueFrom.SecretKeyRef)
	assert.Equal(t, "sess1-push-creds", credEnv.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "git-token", credEnv.ValueFrom.SecretKeyRef.Key)
	require.NotNil(t, credEnv.ValueFrom.SecretKeyRef.Optional)
	assert.True(t, *credEnv.ValueFrom.SecretKeyRef.Optional, "missing Secret/key must not block the pod")
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(600), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished, "finished reconcile Jobs must self-clean")
	assert.Equal(t, int32(3600), *job.Spec.TTLSecondsAfterFinished)
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "sess1-workspace", job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)

	// sync variant: no cred env var → no injected credential
	j2 := BuildReconcileJob(CPByPodConfig{Namespace: "team-a", Image: "alpine/git"}, "ws-sync-sess1", "sess1-workspace", cmds, "", "", "", 600)
	require.Len(t, j2.Spec.Template.Spec.InitContainers[0].Env, 1, "no credEnvVar -> no injected credential var")
	assert.Equal(t, "GIT_TERMINAL_PROMPT", j2.Spec.Template.Spec.InitContainers[0].Env[0].Name)
}
