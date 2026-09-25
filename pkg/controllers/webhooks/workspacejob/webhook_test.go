package workspacejob

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	_ "github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/git"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

func TestRunnerWorkspaceJobTemplate(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	require.NoError(t, batchv1.AddToScheme(s))
	session := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: "team", UID: types.UID("uid1")}}
	session.Status.ResolvedWorkspaceSource = &v1.ResolvedWorkspaceSource{
		Kind: "git", Locator: "https://example.com/repo.git", Revision: "main", OverlayCut: true,
		ReconcileImage: "alpine/git:2.47", ReconcileServiceAccount: "ap-snapshotter",
	}
	w := New(fake.NewClientBuilder().WithScheme(s).WithObjects(session).Build(), admission.NewDecoder(s))
	drv, ok := registry.Get("git")
	require.True(t, ok)
	cmds, err := drv.SyncCommands(workspacekinds.Spec{Kind: "git", Locator: session.Status.ResolvedWorkspaceSource.Locator, Ref: "main"}, "/workspace")
	require.NoError(t, err)
	valid := workspace.BuildReconcileJob(workspace.CPByPodConfig{Namespace: "team", Image: "alpine/git:2.47", ServiceAccount: "ap-snapshotter"},
		"ws-sync-run1", "run1-workspace", cmds, "", "", "", reconcileDeadline)
	yes := true
	valid.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession", Name: "run1", UID: session.UID, Controller: &yes, BlockOwnerDeletion: &yes}}

	request := func(job *batchv1.Job, username string) admission.Request {
		raw, e := json.Marshal(job)
		require.NoError(t, e)
		return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create, Namespace: "team", UserInfo: authenticationv1.UserInfo{Username: username}, Object: runtime.RawExtension{Raw: raw}}}
	}
	runner := "system:serviceaccount:team:run1-runner-sa"
	require.True(t, w.Handle(context.Background(), request(valid, runner)).Allowed, "legitimate sync Job must pass")
	applier := drv.(workspacekinds.Applier)
	applyCommands, err := applier.ApplyCommands(workspacekinds.Spec{Kind: "git", Locator: session.Status.ResolvedWorkspaceSource.Locator, Ref: "main"}, "/workspace")
	require.NoError(t, err)
	credEnv, credKey := drv.(workspacekinds.CredentialedApplier).ApplyCredential()
	applyJob := workspace.BuildReconcileJob(workspace.CPByPodConfig{Namespace: "team", Image: "alpine/git:2.47", ServiceAccount: "ap-snapshotter"},
		"ws-apply-run1", "run1-workspace", applyCommands, credEnv, v1.PassthroughCredentialSecretName("run1"), credKey, reconcileDeadline)
	applyJob.OwnerReferences = valid.OwnerReferences
	require.True(t, w.Handle(context.Background(), request(applyJob, runner)).Allowed, "legitimate apply Job must pass")

	cases := map[string]func(*batchv1.Job){
		"other service account": func(j *batchv1.Job) { j.Spec.Template.Spec.ServiceAccountName = "admin" },
		"foreign secret": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[0].Secret = &corev1.SecretVolumeSource{SecretName: "other"}
			j.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim = nil
		},
		"foreign pvc": func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other-workspace"
		},
		"different image":   func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "attacker/image" },
		"different command": func(j *batchv1.Job) { j.Spec.Template.Spec.InitContainers[0].Command = []string{"sh"} },
		"different owner":   func(j *batchv1.Job) { j.OwnerReferences[0].UID = "other" },
		"other job name":    func(j *batchv1.Job) { j.Name = "arbitrary" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			job := valid.DeepCopy()
			mutate(job)
			require.False(t, w.Handle(context.Background(), request(job, runner)).Allowed)
		})
	}
	require.False(t, w.Handle(context.Background(), request(valid, "system:serviceaccount:other:run1-runner-sa")).Allowed)
	require.True(t, w.Handle(context.Background(), request(valid, "cluster-admin")).Allowed)
}
