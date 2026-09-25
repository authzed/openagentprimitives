package install_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// bundleWithSoleAgentClass builds a minimal single-CR bundle (no questions, no
// requires) whose lone AgentClass is named to collide with a pre-existing
// object a test seeds directly into the fake client. Built inline rather than
// via the shared oaptest fixture: this test needs to control the CR's name to
// match a specific pre-existing object, which the shared fixture's fixed
// contents don't offer a hook for.
func bundleWithSoleAgentClass(name string) *oap.Bundle {
	return &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: name, Version: "1.0.0"},
		},
		Manifests: []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: ` + name + `
spec:
  systemPrompt:
    inline: "bundle prompt"
`),
	}
}

// TestInstall_WriteFailureAfterAdopt_ReportsAdoptedKeysInError pins that a
// write-loop failure still names what this run adopted. resolveConflicts's
// decision is recorded ONLY in Result.Adopted (adoption is stamped on no
// object), and a write-loop failure returns (nil, err), discarding Result — so
// without withAdoptedHint a mid-loop apply failure after an ownership decision
// to adopt a pre-existing object leaves that security-relevant decision
// unrecoverable from the returned error.
//
// This is a fake-client unit test, not an envtest one: a real apiserver's SSA
// Patch does not fail on demand for a well-formed object, so there is no clean
// way to force a mid-loop write failure through envtest. The fake client's
// interceptor lets the Patch call return an arbitrary error, which is the
// failure mode this test needs.
func TestInstall_WriteFailureAfterAdopt_ReportsAdoptedKeysInError(t *testing.T) {
	const ns = "test-ns"

	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	foreign.SetKind("AgentClass")
	foreign.SetName("adopt-agent")
	foreign.SetNamespace(ns)
	// Deliberately NO oap-install label: this is what makes it a conflict for
	// checkResourceOwnership to report, and thus a candidate for opts.Adopt.

	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithObjects(foreign).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				// Every SSA-apply Patch fails, simulating an apiserver error on the
				// write that follows a successful, already-decided adopt.
				return errors.New("simulated apiserver failure")
			},
		}).
		Build()

	_, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("adopt-agent"), oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
		Adopt:     []string{"AgentClass/adopt-agent"},
	})

	require.Error(t, err, "the forced Patch failure must abort Install")
	assert.Contains(t, err.Error(), "AgentClass/adopt-agent", "the failing apply's own error must still name the object")
	assert.Contains(t, err.Error(), "already adopted", "the adopt decision made before any write must survive a later write failure, not be discarded along with Result")
}

// TestInstall_WriteFailureWithNoAdoption_OmitsAdoptedHint is the converse: an
// ordinary write-loop failure with nothing adopted must NOT grow a
// misleading "(already adopted: [])" suffix — the hint is opt-in on
// len(adopted) > 0, per withAdoptedHint's contract.
func TestInstall_WriteFailureWithNoAdoption_OmitsAdoptedHint(t *testing.T) {
	const ns = "test-ns"

	c := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return errors.New("simulated apiserver failure")
			},
		}).
		Build()

	_, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("fresh-agent"), oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
	})

	require.Error(t, err, "the forced Patch failure must abort Install")
	assert.NotContains(t, err.Error(), "already adopted", "nothing was adopted on a fresh install; the hint must not appear")
}
