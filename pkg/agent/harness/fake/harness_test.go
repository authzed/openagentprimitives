package fake_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/fake"
)

func TestFakeContributesItsConfiguredContainerSpec(t *testing.T) {
	h := fake.New("demo-harness")
	h.Cmd = []string{"/bin/demo", "--serve"}
	h.ExtraEnv = []corev1.EnvVar{{Name: "DEMO_MODE", Value: "on"}}

	assert.Equal(t, "demo-harness", h.Name())
	assert.Equal(t, harness.Proxied, h.ModelAccess())

	spec, err := h.Container(harness.HarnessOpts{Image: "demo:dev"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/bin/demo", "--serve"}, spec.Command)
	assert.Equal(t, []corev1.EnvVar{{Name: "DEMO_MODE", Value: "on"}}, spec.Env)
}

func TestFakeContainerErrorPropagates(t *testing.T) {
	h := fake.New("boom-harness")
	h.Err = errors.New("synthetic container failure")

	_, err := h.Container(harness.HarnessOpts{})
	require.Error(t, err, "a fake configured to fail must surface that error to the caller")
	assert.Contains(t, err.Error(), "synthetic container failure")
}
