package cloud

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// operatorDeployment is the operator Deployment as `oap install` leaves it:
// the stamp on every container, since setDeploymentEnv writes them all.
// env == "" stamps nothing, which is a cluster installed before the stamp
// existed.
func operatorDeployment(env string) *appsv1.Deployment {
	c := corev1.Container{Name: "operator"}
	if env != "" {
		c.Env = []corev1.EnvVar{{Name: ClusterKindEnvVar, Value: env}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: OperatorDeploymentName, Namespace: WebdServiceNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}},
		},
	}
}

// TestStamped_ReadsBackTheKindInstallRecorded covers the answers Detect cannot
// give. The desktop row is the whole reason this function exists: a node scan
// of a desktop cluster returns the default kind, because `desktop` reports no
// providerID prefix on purpose.
func TestStamped_ReadsBackTheKindInstallRecorded(t *testing.T) {
	for _, key := range []string{KeyDesktop, KeyLocal, KeyGKE, KeyDefault} {
		t.Run(key+": the stamped kind is the one returned", func(t *testing.T) {
			kc := fake.NewSimpleClientset(operatorDeployment(key))

			got, err := Stamped(context.Background(), kc)
			require.NoError(t, err)
			assert.Equal(t, key, got.Key())
		})
	}
}

// TestStamped_RefusesRatherThanFallingBack. Each of these is "I could not tell",
// and every one of them must stay distinguishable from "this is the default
// kind" — a fallback here would silently treat a developer box as production
// infrastructure, or the reverse.
func TestStamped_RefusesRatherThanFallingBack(t *testing.T) {
	cases := []struct {
		name    string
		objs    []runtime.Object
		wantErr string
	}{
		{
			name:    "no operator Deployment: nothing was installed here",
			wantErr: "run `oap install` first",
		},
		{
			name:    "a Deployment with no stamp: installed before the kind was recorded",
			objs:    []runtime.Object{operatorDeployment("")},
			wantErr: "carries no " + ClusterKindEnvVar,
		},
		{
			name:    "a stamp naming a kind this build does not register",
			objs:    []runtime.Object{operatorDeployment("nosuchkind")},
			wantErr: "nosuchkind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)

			got, err := Stamped(context.Background(), kc)
			require.Error(t, err)
			assert.Nil(t, got, "a refusal must never hand back a usable Strategy alongside its error")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
