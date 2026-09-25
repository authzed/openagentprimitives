//go:build integration

package testenv_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

// Reset must delete a finalizer-bearing CR + a Secret, leaving the cluster empty.
func TestResetWipesClusterIncludingFinalizers(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"}}
	require.NoError(t, env.Client.Create(ctx, sec), "create secret")

	// A non-default namespace + a Secret in it: proves the all-namespaces sweep.
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
	}), "create namespace ns")
	nsSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ns-creds", Namespace: "ns"}}
	require.NoError(t, env.Client.Create(ctx, nsSec), "create secret in ns")

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "reset-me",
			Namespace:  "default",
			Finalizers: []string{"spicebox.authzed.com/test"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass with finalizer")

	testenv.Reset(t, env)

	var gotAC spiceboxv1alpha1.AgentClass
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "reset-me"}, &gotAC)
	assert.True(t, apierrors.IsNotFound(err), "AgentClass should be gone after Reset; err=%v", err)

	var gotSec corev1.Secret
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "llm-creds"}, &gotSec)
	assert.True(t, apierrors.IsNotFound(err), "Secret should be gone after Reset; err=%v", err)

	var gotNSSec corev1.Secret
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "ns-creds"}, &gotNSSec)
	assert.True(t, apierrors.IsNotFound(err), "Secret in ns should be gone after Reset; err=%v", err)

	// Prove batchv1.Job is also wiped. The API server defaults Jobs to Orphan
	// propagation (adds a finalizer the GC must clear); since envtest runs no GC,
	// Reset must delete them with explicit Background propagation or they leak.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "reset-job", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "c", Image: "busybox"},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, job), "create Job")

	testenv.Reset(t, env)

	var gotJob batchv1.Job
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "reset-job"}, &gotJob)
	assert.True(t, apierrors.IsNotFound(err), "Job should be gone after Reset; err=%v", err)
}
