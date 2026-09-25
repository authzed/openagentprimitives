//go:build integration

package install_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv/idempotency"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// TestInstall_AgentClassApply_IsIdempotentViaHarness is the shared
// idempotency-harness counterpart of TestInstall_OapSourceAnnotationIsSSAIdempotent
// (apply_integration_test.go): both pin the SAME regression — the
// ap-agent-install oap-source annotation (stampOapSource) must never carry a
// wall-clock value, or a byte-identical re-install stops being a true SSA no-op
// and field ownership churns on every apply.
//
// Where that test hand-rolls the resourceVersion comparison across two Install()
// runs, this one runs Install ONCE to get a real stampOapSource-produced
// AgentClass, then feeds the clean applied-fields shape (TypeMeta + metadata +
// spec — what install.ssaApply itself sends) through
// idempotency.RequireApplyIdempotent under the install's real field manager,
// proving the general-purpose CI-gate harness covers this specific bug.
func TestInstall_AgentClassApply_IsIdempotentViaHarness(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-harness-idempotent"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	b := packedDemoAgent(t)
	answers, secrets := resolveDemoAgent(t, b, "fake-token-value")

	opts := install.InstallOpts{
		Namespace:    ns,
		SourceKind:   "registry",
		SourceRef:    "ghcr.io/example/demo-agent:1",
		SourceDigest: "sha256:deadbeef1234",
	}
	result, err := install.Install(ctx, env.Client, b, answers, secrets, opts)
	require.NoError(t, err, "Install must succeed so there is a real stampOapSource-produced AgentClass to re-apply")

	var got v1alpha1.AgentClass
	require.NoError(t,
		env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: result.Name}, &got),
		"Get installed AgentClass")
	require.NotEmpty(t, got.Annotations[instance.AnnotationOapSource], "installed AgentClass must carry the oap-source annotation for this test to be meaningful")

	// Clean desired-state shape: TypeMeta + identity + the applied
	// metadata/spec fields only — no resourceVersion/uid/managedFields/status
	// (the live-object bookkeeping a caller constructing a fresh "want" for
	// apply would never have in the first place).
	want := &v1alpha1.AgentClass{
		TypeMeta: metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "AgentClass"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        got.Name,
			Namespace:   got.Namespace,
			Labels:      got.Labels,
			Annotations: got.Annotations,
		},
		Spec: got.Spec,
	}

	idempotency.RequireApplyIdempotent(t, ctx, env.Client, want, install.FieldManager)
}
