// Package fake is an in-memory Harness for tests. It contributes whatever
// command/env the test configures, so a test can prove the harness seam is
// actually threaded through to the runner Pod rather than asserting against
// ap-native's deliberately-empty contribution.
//
// Not registered by an init(): tests register it explicitly so the global
// registry stays deterministic. Mirrors pkg/channels/channelkinds/fake.
package fake

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
)

// Harness is a configurable test harness. Safe for concurrent Container
// calls: every field is set once at construction and only read afterwards.
type Harness struct {
	// HarnessName is returned by Name(); it is the registry key.
	HarnessName string
	// Cmd becomes ContainerSpec.Command. Nil leaves the image entrypoint.
	Cmd []string
	// ExtraEnv becomes ContainerSpec.Env.
	ExtraEnv []corev1.EnvVar
	// Mode is returned by ModelAccess(). Zero value means harness.Proxied.
	Mode harness.ModelAccessMode
	// Err, when non-nil, is returned by Container instead of a spec.
	Err error
}

// New returns a fake harness registered under name, defaulting to Proxied.
func New(name string) *Harness {
	return &Harness{HarnessName: name, Mode: harness.Proxied}
}

func (h *Harness) Name() string { return h.HarnessName }

func (h *Harness) ModelAccess() harness.ModelAccessMode {
	if h.Mode == "" {
		return harness.Proxied
	}
	return h.Mode
}

func (h *Harness) Container(harness.HarnessOpts) (harness.ContainerSpec, error) {
	if h.Err != nil {
		return harness.ContainerSpec{}, h.Err
	}
	return harness.ContainerSpec{Command: h.Cmd, Env: h.ExtraEnv}, nil
}
