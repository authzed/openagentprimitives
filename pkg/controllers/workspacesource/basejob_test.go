package workspacesource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
)

func demoWS() *spiceboxv1alpha1.WorkspaceSource {
	return &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-source", Namespace: "team-a", UID: "uid-1"},
		Spec: spiceboxv1alpha1.WorkspaceSourceSpec{
			Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "https://example.com/o/r.git", Ref: "main"},
		},
	}
}

func TestBuildBasePVC(t *testing.T) {
	pvc := BuildBasePVC(demoWS(), "ap-workspace-rwx", "2Gi")
	assert.Equal(t, "ws-base-demo-source", pvc.Name)
	assert.Equal(t, "team-a", pvc.Namespace)
	require.Len(t, pvc.OwnerReferences, 1)
	assert.Equal(t, "WorkspaceSource", pvc.OwnerReferences[0].Kind)
	assert.Equal(t, "demo-source", pvc.OwnerReferences[0].Name)
	assert.Equal(t, "ap-workspace-rwx", *pvc.Spec.StorageClassName)
	assert.True(t, *pvc.OwnerReferences[0].Controller)
	assert.True(t, *pvc.OwnerReferences[0].BlockOwnerDeletion)
}

func TestBuildMaterializeJob_RunsDriverCommandsAsInitContainers(t *testing.T) {
	cmds := []workspacekinds.Command{
		{Env: []string{"GIT_TERMINAL_PROMPT=0"}, Argv: []string{"git", "clone", "--", "https://example.com/o/r.git", "/base/tree"}},
	}
	job := BuildMaterializeJob(demoWS(), "ws-base-demo-source", "ap-materializer", "busybox:1.36", cmds)

	require.Len(t, job.Spec.Template.Spec.InitContainers, 1, "one initContainer per driver command")
	ic := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, []string{"git"}, ic.Command)
	assert.Equal(t, []string{"clone", "--", "https://example.com/o/r.git", "/base/tree"}, ic.Args)
	assert.Contains(t, ic.Env, corev1EnvVar("GIT_TERMINAL_PROMPT", "0"))
	require.Len(t, ic.VolumeMounts, 1)
	assert.Equal(t, "/base", ic.VolumeMounts[0].MountPath)
	// main container is a trivial no-op
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, []string{"true"}, job.Spec.Template.Spec.Containers[0].Command)
	// base PVC is mounted
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "ws-base-demo-source", job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

func TestBuildRefreshJob_RunsSyncCommandsAgainstBaseMount(t *testing.T) {
	cmds := []workspacekinds.Command{
		{Env: []string{"GIT_TERMINAL_PROMPT=0"}, Dir: "/base/tree", Argv: []string{"git", "pull", "--ff-only"}},
	}
	job := BuildRefreshJob(demoWS(), "ws-base-demo-source", "ap-materializer", "busybox:1.36", cmds)

	assert.Equal(t, "ws-refresh-demo-source", job.Name, "refresh Job name must not collide with the materialize Job")
	assert.Equal(t, "team-a", job.Namespace)
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, "WorkspaceSource", job.OwnerReferences[0].Kind)
	assert.Equal(t, "demo-source", job.OwnerReferences[0].Name)
	assert.True(t, *job.OwnerReferences[0].Controller)
	assert.NotContains(t, job.Labels, BaseGenerationLabel, "refresh re-pulls regardless of generation, so it carries no generation label")
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.EqualValues(t, jobDeadlineSeconds, *job.Spec.ActiveDeadlineSeconds)

	require.Len(t, job.Spec.Template.Spec.InitContainers, 1, "one initContainer per driver sync command")
	ic := job.Spec.Template.Spec.InitContainers[0]
	assert.Equal(t, []string{"git"}, ic.Command)
	assert.Equal(t, []string{"pull", "--ff-only"}, ic.Args)
	assert.Equal(t, "/base/tree", ic.WorkingDir)
	assert.Contains(t, ic.Env, corev1EnvVar("GIT_TERMINAL_PROMPT", "0"))
	require.Len(t, ic.VolumeMounts, 1)
	assert.Equal(t, "/base", ic.VolumeMounts[0].MountPath, "base PVC is mounted at /base, same as materialize")

	// main container is a trivial no-op
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, []string{"true"}, job.Spec.Template.Spec.Containers[0].Command)
	// base PVC is mounted (same claim materialize used)
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "ws-base-demo-source", job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

func TestSpecToDriver(t *testing.T) {
	d := SpecToDriver(demoWS().Spec)
	assert.Equal(t, "git", d.Kind)
	assert.Equal(t, "https://example.com/o/r.git", d.Locator)
	assert.Equal(t, "main", d.Ref)

	d2 := SpecToDriver(spiceboxv1alpha1.WorkspaceSourceSpec{
		Source: spiceboxv1alpha1.WorkspaceSourceRef{Kind: "git", Locator: "x"},
		Scope:  spiceboxv1alpha1.WorkspaceScope{Paths: []spiceboxv1alpha1.WorkspaceScopePath{{Path: "src", Writable: true}}},
	})
	require.Len(t, d2.Scope.Paths, 1)
	assert.Equal(t, "src", d2.Scope.Paths[0].Path)
	assert.True(t, d2.Scope.Paths[0].Writable)
}
