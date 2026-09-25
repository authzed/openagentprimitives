//go:build integration

// Envtest coverage for status.permissionSurface: the field the runner writes at
// session start so the CLI and admin panel can answer "what can this agent do?".
//
// A real API server is the point. The field carries a kubebuilder enum on
// stateImpact, and an enum is only enforced by the apiserver — a unit test
// marshals whatever it is given. If permsurface ever emits a severity outside
// the enum, the patch is REJECTED at runtime, and because the runner treats the
// publish as best-effort that rejection would be a log line nobody reads and a
// permanently empty capabilities view.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestPermissionSurface_everySeverityPermsurfaceEmitsIsAcceptedByTheCRD(t *testing.T) {
	ctx := context.Background()
	env := testenv.Shared(t)

	// Exactly the severities permsurface can produce. stateless and passthrough
	// are excluded from the surface entirely, so they must NOT be in the enum —
	// and this list is derived from the authz constants rather than retyped, so
	// a new StateImpact that reaches the surface breaks this test rather than
	// silently failing every runtime patch.
	severities := []authz.StateImpact{authz.Readonly, authz.Readwrite, authz.External}

	entries := make([]spiceboxv1alpha1.PermissionSurfaceEntry, 0, len(severities))
	for _, s := range severities {
		entries = append(entries, spiceboxv1alpha1.PermissionSurfaceEntry{
			Handle:      "perm:read:" + string(s) + "_type",
			StateImpact: string(s),
			Tools:       []string{"some_tool"},
		})
	}

	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "as-permsurface", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "c"},
	}
	require.NoError(t, env.Client.Create(ctx, as))

	as.Status.PermissionSurface = entries
	require.NoError(t, env.Client.Status().Update(ctx, as),
		"the CRD must accept every severity permsurface can emit; a rejection here "+
			"is a runtime patch failure the runner only logs")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(as), &got))
	require.Len(t, got.Status.PermissionSurface, len(severities))
	assert.Equal(t, entries, got.Status.PermissionSurface,
		"the surface must round-trip unchanged — it is what an operator reads")
}

// The enum must also REFUSE a severity the surface never produces. Without this,
// the enum could silently be widened to a bare string and nothing would notice
// until a malformed value reached an operator's screen.
func TestPermissionSurface_rejectsASeverityOutsideTheEnum(t *testing.T) {
	ctx := context.Background()
	env := testenv.Shared(t)

	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "as-permsurface-bad", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "c"},
	}
	require.NoError(t, env.Client.Create(ctx, as))

	as.Status.PermissionSurface = []spiceboxv1alpha1.PermissionSurfaceEntry{
		{Handle: "perm:read:thing", StateImpact: "passthrough"},
	}
	assert.Error(t, env.Client.Status().Update(ctx, as),
		"passthrough is excluded from the surface by construction; the CRD must not accept it")
}
