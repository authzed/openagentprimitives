// pkg/controllers/agentsession/pod_runner_factory_test.go
//
// Unit tests for PodRunnerFactory.Start's harness-image guard: a non-default
// harness with no entry in HarnessImages must be refused rather than
// silently falling back to RunnerImage (the oap-native runner image) — see
// pod_runner_factory.go's HarnessImages field.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// registerDemoHarnessForStartTest registers a "demo-harness" fake, guarded
// like harness_validation_test.go's init() so re-registration across test
// files sharing this package's process-global harness registry never
// panics.
func registerDemoHarnessForStartTest(t *testing.T) {
	t.Helper()
	if _, ok := registry.Get("demo-harness"); !ok {
		registry.Register(fake.New("demo-harness"))
	}
}

// newStartFixture builds a fake client pre-loaded with the per-session
// memory-token Secret that Start reads before resolving the harness, plus an
// AgentSession with EffectiveSettings stamped — Start refuses to build a pod
// without it (see the "no effectiveSettings stamped" guard).
func newStartFixture(t *testing.T, harnessName string) (*PodRunnerFactory, *spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "default"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			EffectiveSettings: &spiceboxv1alpha1.EffectiveSettings{
				Model: spiceboxv1alpha1.ModelConfig{
					APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "demo-llm-creds", Key: "api-key"},
				},
			},
		},
	}
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentClassSpec{Harness: harnessName},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: MemoryTokenSecretName(sess), Namespace: "default"},
	}
	c := buildFakeClient(t, secret)
	f := &PodRunnerFactory{
		Client:      c,
		RunnerImage: "runner:dev",
		OperatorURL: "http://op:8082",
		NATSURL:     "nats://nats:4222",
		// HarnessImages is deliberately left nil/empty in both cases below —
		// that absence is the condition under test.
	}
	return f, sess, class
}

func TestPodRunnerFactoryStartHarnessImageGuard(t *testing.T) {
	registerDemoHarnessForStartTest(t)

	t.Run("default harness (ap-native) with empty HarnessImages: no error from the image guard", func(t *testing.T) {
		f, sess, class := newStartFixture(t, "")
		err := f.Start(context.Background(), sess, class, StartOpts{})
		assert.NoError(t, err, "ap-native must fall back to RunnerImage, not be refused by the harness-image guard")
	})

	t.Run("named harness with empty HarnessImages: fails closed instead of launching RunnerImage", func(t *testing.T) {
		f, sess, class := newStartFixture(t, "demo-harness")
		err := f.Start(context.Background(), sess, class, StartOpts{})
		require.Error(t, err, "a harness with no configured image must be refused, not silently fall back to the oap-native runner image")
		assert.Contains(t, err.Error(), `harness "demo-harness" has no configured image`)
	})
}
