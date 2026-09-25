package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/fake"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestMergeHarnessContainer(t *testing.T) {
	cases := []struct {
		name     string
		platform []corev1.EnvVar
		spec     harness.ContainerSpec
		wantErr  string
		wantEnv  []corev1.EnvVar
		wantCmd  []string
	}{
		{
			name:     "empty spec leaves the container untouched",
			platform: []corev1.EnvVar{{Name: "OPERATOR_MEMORY_URL", Value: "http://op"}},
			spec:     harness.ContainerSpec{},
			wantEnv:  []corev1.EnvVar{{Name: "OPERATOR_MEMORY_URL", Value: "http://op"}},
		},
		{
			name:     "harness env is appended after platform env",
			platform: []corev1.EnvVar{{Name: "OPERATOR_MEMORY_URL", Value: "http://op"}},
			spec:     harness.ContainerSpec{Env: []corev1.EnvVar{{Name: "DEMO", Value: "1"}}},
			wantEnv: []corev1.EnvVar{
				{Name: "OPERATOR_MEMORY_URL", Value: "http://op"},
				{Name: "DEMO", Value: "1"},
			},
		},
		{
			name:     "harness command overrides the image entrypoint",
			platform: nil,
			spec:     harness.ContainerSpec{Command: []string{"/bin/demo"}},
			wantCmd:  []string{"/bin/demo"},
		},
		{
			name:     "a harness may not shadow platform env",
			platform: []corev1.EnvVar{{Name: "MEMORY_TOKEN_PATH", Value: "/var/run/agent/memory-token"}},
			spec:     harness.ContainerSpec{Env: []corev1.EnvVar{{Name: "MEMORY_TOKEN_PATH", Value: "/tmp/steal"}}},
			wantErr:  `harness env "MEMORY_TOKEN_PATH" collides with platform wiring`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := corev1.Container{Name: "runner", Env: tc.platform}
			err := mergeHarnessContainer(&c, tc.spec)
			if tc.wantErr != "" {
				require.Error(t, err, "a colliding env name must fail closed, not silently win")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantEnv != nil {
				assert.Equal(t, tc.wantEnv, c.Env)
			}
			assert.Equal(t, tc.wantCmd, c.Command)
		})
	}
}

// basePodSpecOpts returns the minimal PodSpecOpts a runner-pod build needs,
// with the harness under test wired in. Uses fixture names, never an
// example's name.
//
// EffectiveSettings is stamped here (beyond the brief's original fixture)
// because BuildRunnerPod unconditionally reads
// Session.Status.EffectiveSettings.Model.APIKey.Name to build the
// llm-api-key volume; a nil EffectiveSettings would nil-panic rather than
// exercise the harness merge under test.
func basePodSpecOpts(t *testing.T, h harness.Harness, harnessImage string) PodSpecOpts {
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
	}
	opts := PodSpecOpts{
		Session:     sess,
		Class:       class,
		Image:       "runner:dev",
		ServiceAcct: "demo-sa",
		MemoryToken: "demo-session-memory-token",
		OperatorURL: "http://operator.test:8082",
		Harness:     h,
	}
	// HarnessImage is the already-resolved single image BuildRunnerPod
	// consumes. The name->image MAP lives on PodRunnerFactory, not here, and
	// is resolved before this call.
	opts.HarnessImage = harnessImage
	return opts
}

func TestBuildRunnerPodAppliesTheHarnessContribution(t *testing.T) {
	h := fake.New("demo-harness")
	h.Cmd = []string{"/bin/demo", "--serve"}
	h.ExtraEnv = []corev1.EnvVar{{Name: "DEMO_MODE", Value: "on"}}

	pod, err := BuildRunnerPod(basePodSpecOpts(t, h, "demo:dev"))
	require.NoError(t, err)

	var runner *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "runner" {
			runner = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner, "the built pod must contain a runner container")
	assert.Equal(t, "demo:dev", runner.Image, "the harness image must win over the operator-wide runner image")
	assert.Equal(t, []string{"/bin/demo", "--serve"}, runner.Command)
	assert.Contains(t, runner.Env, corev1.EnvVar{Name: "DEMO_MODE", Value: "on"})
	assert.Contains(t, runner.Env, corev1.EnvVar{Name: "MEMORY_TOKEN_PATH", Value: "/var/run/agent/memory-token"},
		"platform wiring must survive the harness merge")
}

func TestBuildRunnerPodWithAPNativeIsUnchanged(t *testing.T) {
	withHarness, err := BuildRunnerPod(basePodSpecOpts(t, apnative.Harness{}, ""))
	require.NoError(t, err)

	var runner *corev1.Container
	for i := range withHarness.Spec.Containers {
		if withHarness.Spec.Containers[i].Name == "runner" {
			runner = &withHarness.Spec.Containers[i]
		}
	}
	require.NotNil(t, runner)
	assert.Nil(t, runner.Command, "ap-native must not override the image entrypoint")
	// Args is platform-set (the runner's own --hostname flag), not
	// harness-contributed; ap-native's empty ContainerSpec.Args (nil) must
	// leave that platform value untouched rather than clearing it.
	assert.Equal(t, []string{"--hostname=$(POD_NAME)"}, runner.Args, "ap-native must not alter the platform-set runner args")
}
