package health

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
)

func tokenSecret(name string, token string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if token != "" {
		s.Data = map[string][]byte{bootstrap.TokenSecretKey: []byte(token)}
	}
	return s
}

func bundleWith(objs ...runtime.Object) *kube.Bundle {
	return &kube.Bundle{Typed: fake.NewSimpleClientset(objs...)}
}

func TestMemoryTokenSecrets_AllPresentOK(t *testing.T) {
	var objs []runtime.Object
	for _, n := range bootstrap.MemoryTokenSecretNames {
		objs = append(objs, tokenSecret(n, "deadbeef"))
	}
	res := memoryTokenSecrets{}.Check(context.Background(), bundleWith(objs...))
	assert.Equal(t, OK, res.Status)
	assert.False(t, res.NotFound)
}

func TestMemoryTokenSecrets_MissingOneFails(t *testing.T) {
	var objs []runtime.Object
	for _, n := range bootstrap.MemoryTokenSecretNames[1:] { // omit the first
		objs = append(objs, tokenSecret(n, "deadbeef"))
	}
	res := memoryTokenSecrets{}.Check(context.Background(), bundleWith(objs...))
	assert.Equal(t, Failed, res.Status)
	assert.Contains(t, res.Detail, bootstrap.MemoryTokenSecretNames[0], "names the missing Secret")
	assert.False(t, res.NotFound, "some present → not a clean NotFound")
}

func TestMemoryTokenSecrets_EmptyTokenFails(t *testing.T) {
	var objs []runtime.Object
	objs = append(objs, tokenSecret(bootstrap.MemoryTokenSecretNames[0], "")) // present but empty
	for _, n := range bootstrap.MemoryTokenSecretNames[1:] {
		objs = append(objs, tokenSecret(n, "deadbeef"))
	}
	res := memoryTokenSecrets{}.Check(context.Background(), bundleWith(objs...))
	assert.Equal(t, Failed, res.Status)
	assert.Contains(t, res.Detail, "empty token")
}

func TestMemoryTokenSecrets_AllAbsentNotFound(t *testing.T) {
	res := memoryTokenSecrets{}.Check(context.Background(), bundleWith())
	assert.Equal(t, Failed, res.Status)
	assert.True(t, res.NotFound, "none present → NotFound so trimmed profiles can skip")
	// The tokens are minted by the pre-start bootstrap Job, NOT the operator
	// runnable (that design was abandoned). A cold-start diagnosis must point at
	// the Job so an operator doesn't hunt for operator mint logs that never exist.
	assert.Contains(t, res.Detail, "bootstrap Job", "attributes a cold-start miss to the Job, not the operator")
	assert.NotContains(t, res.Detail, "operator bootstrap", "must not misdirect to the abandoned operator-mint design")
}

func linkedChannelsd(secret string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-channelsd", Namespace: namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "memory-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}},
		}}},
	}
}

func TestMemoryTokenSecrets_ChecksTheLinkedSecret(t *testing.T) {
	cases := []struct {
		name       string
		linkedMade bool
		want       Status
	}{
		{name: "linked Secret missing (default present): Failed naming the linked Secret", want: Failed},
		{name: "linked Secret present: OK", linkedMade: true, want: OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{linkedChannelsd("my-channelsd-token")}
			for _, n := range bootstrap.MemoryTokenSecretNames {
				objs = append(objs, tokenSecret(n, "deadbeef")) // the Job mints defaults regardless of links
			}
			if tc.linkedMade {
				objs = append(objs, tokenSecret("my-channelsd-token", "deadbeef"))
			}
			res := memoryTokenSecrets{}.Check(context.Background(), bundleWith(objs...))
			assert.Equal(t, tc.want, res.Status, res.Detail)
			if tc.want == Failed {
				assert.Contains(t, res.Detail, "my-channelsd-token")
			}
		})
	}
}
