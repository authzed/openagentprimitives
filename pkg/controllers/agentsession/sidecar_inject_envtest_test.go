//go:build integration

// pkg/controllers/agentsession/sidecar_inject_envtest_test.go
//
// End-to-end integration test that proves the AgentSession reconciler,
// wired with a real PodRunnerFactory, actually CREATES a runner Pod that
// contains the sidecar container + the MCP_PORT_* runner env + the
// per-session secret wired via envFrom.
//
// Coverage gap filled: Tasks 7 (podspec) + 8 (factory/StartOpts) + 9
// (reconcile sidecar resolution) are each unit/reconcile-level tested.
// This test is the first to exercise the integrated path:
//
//	reconciler → materializeSidecarSecret → PodRunnerFactory.Start →
//	BuildRunnerPod → apiserver creates the Pod.
//
// It does NOT duplicate sidecar_reconcile_test.go's assertions about
// empty-secret contents, missing-CR boot-fail, static-cred projection,
// or missing-credential boot-fail.  Those are reconcile-level facts that
// don't require a real Pod.  This test's unique assertion is that the
// injected sidecar CONTAINER appears in the created Pod's spec.
package agentsession_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestSidecarInjection_ComposesContainerIntoRunnerPod proves the end-to-end
// path: a real PodRunnerFactory creates a runner Pod that contains the
// sidecar container injected from the AgentClass sidecarToolboxes ref.
//
// Assertions (unique to this test — not in sidecar_reconcile_test.go):
//   - Pod.Spec.Containers contains a container named "sidecar-<ref>"
//   - The runner container (Containers[0]) has env MCP_PORT_<EnvPrefix(name)>
//     set to the allocated port value (non-zero)
//   - The sidecar container has envFrom referencing the per-session sidecar
//     secret name
//   - sess.Status.ResolvedSidecarToolboxes has one entry with Port != 0
func TestSidecarInjection_ComposesContainerIntoRunnerPod(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newReconciler(t, env)

	// SpiceboxClass: cluster-scoped, referenced by the SidecarToolbox's
	// sandbox.class. A minimal spec with real resource values is required
	// for a valid CR; the network field is left zero (mode=none is the
	// default, which means effective mode will also be none/empty when
	// there are no allowedHosts on the CR either).
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sidecar-sandbox-default"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU:              resource.MustParse("100m"),
				Memory:           resource.MustParse("64Mi"),
				EphemeralStorage: resource.MustParse("16Mi"),
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, cls), "create SpiceboxClass")

	// SidecarToolbox: the echo-example path (no upstream credential needed).
	// An empty provider/envVar means materializeSidecarSecret writes an empty
	// Secret — the envFrom resolves, and the pod-build path is fully exercised
	// without needing a real credential. sidecar_reconcile_test.go already
	// covers what is IN that secret; this test only cares that the container
	// appears in the Pod.
	tb := sidecarToolbox(t, "echo-inject", "", "")
	require.NoError(t, env.Client.Create(ctx, tb), "create SidecarToolbox")

	// AgentClass referencing the sidecar.  Name "inject-echo" avoids
	// collisions with the sessions created in sidecar_reconcile_test.go.
	ac := classWithSidecars("ac-inject",
		spiceboxv1alpha1.AgentClassSidecarToolboxRef{Name: "echo", Ref: "echo-inject"},
	)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	// AgentSession for that class.
	sess := validSession("s-inject", "ac-inject")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// Drive reconcile enough times for finalizer + RBAC + sidecar secret +
	// pod creation.  reconcileToWork runs 4 passes; mirrors the pattern used
	// throughout the other reconcile-level tests.
	reconcileToWork(t, ctx, r, "s-inject")

	// ----------------------------------------------------------------
	// Precondition: the per-session sidecar Secret must exist (created
	// by materializeSidecarSecret before PodRunnerFactory.Start).
	sidecarSecretName := "agentsession-s-inject-toolbox-echo-inject"
	var sec corev1.Secret
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sidecarSecretName}, &sec),
		"per-session sidecar Secret must exist before the Pod is fetched")

	// ----------------------------------------------------------------
	// Primary assertion: the runner Pod exists in the apiserver.
	var pod corev1.Pod
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-inject-runner"}, &pod),
		"runner Pod must be created by PodRunnerFactory.Start")

	// Locate the sidecar container — name is "sidecar-<ref>".
	const sidecarContainerName = "sidecar-echo-inject"
	var sidecarContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == sidecarContainerName {
			sidecarContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, sidecarContainer,
		"Pod must contain a container named %q; containers present: %v",
		sidecarContainerName, containerNames(pod.Spec.Containers))

	// ----------------------------------------------------------------
	// Runner container (index 0) must have MCP_PORT_ECHO env set to the
	// allocated port value.  The name "echo" is the AgentClassSidecarToolboxRef.Name
	// (LLM prefix); EnvPrefix("echo") = "ECHO".
	runnerContainer := pod.Spec.Containers[0]
	require.Equal(t, "runner", runnerContainer.Name,
		"Containers[0] should be the runner container")

	mcpPortEnvKey := "MCP_PORT_ECHO"
	mcpPortValue := ""
	for _, e := range runnerContainer.Env {
		if e.Name == mcpPortEnvKey {
			mcpPortValue = e.Value
			break
		}
	}
	assert.NotEmpty(t, mcpPortValue,
		"runner container must have env %s set (got envs: %v)", mcpPortEnvKey, runnerContainer.Env)

	// ----------------------------------------------------------------
	// Sidecar container must have envFrom referencing the per-session secret.
	var envFromSecretRef string
	for _, ef := range sidecarContainer.EnvFrom {
		if ef.SecretRef != nil {
			envFromSecretRef = ef.SecretRef.Name
			break
		}
	}
	assert.Equal(t, sidecarSecretName, envFromSecretRef,
		"sidecar container envFrom must reference the per-session sidecar Secret")

	// ----------------------------------------------------------------
	// Status snapshot: one resolved sidecar with a non-zero port.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-inject"}, &got),
		"Get AgentSession for status assertion")
	require.Len(t, got.Status.ResolvedSidecarToolboxes, 1,
		"one ResolvedSidecarToolbox entry in status")
	rt := got.Status.ResolvedSidecarToolboxes[0]
	assert.Equal(t, "echo", rt.Name, "resolved sidecar Name (LLM prefix)")
	assert.Equal(t, "echo-inject", rt.Ref, "resolved sidecar Ref (CR name)")
	assert.NotZero(t, rt.Port, "resolved sidecar Port must be allocated (non-zero)")

	// Confirm the MCP_PORT_ECHO env value matches the allocated port.
	assert.Equal(t, fmt.Sprintf("%d", rt.Port), mcpPortValue,
		"runner MCP_PORT_ECHO must equal the status-recorded port")
}

// containerNames returns the names of a container list; used in assertion
// messages to aid diagnosis when the sidecar container is absent.
func containerNames(cs []corev1.Container) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names
}
