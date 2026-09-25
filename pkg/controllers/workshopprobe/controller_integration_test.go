//go:build integration

// Integration tests for the WorkshopProbe controller against a REAL
// apiserver (envtest) and a REAL SpiceDB (testspicedb) — the security
// claims in controller_test.go's fake-client TestReconcile_DeniedBeforeProbe
// are worth little on their own, since a fake Build checker can be made to
// say anything. These tests exercise the actual
// workshop:<id>#build permission ("permission build = session",
// pkg/authz/spicedb/schema) end to end: EnsureWorkshopSubjects really writes
// the tuple, CheckWorkshopBuild really reads it back, and the no-tuple case
// proves the refusal holds against the real authorization backend, not a
// stand-in for it.
//
// Reuses testNamespace/testWorkshop/testProbe/getProbe/probedConditionReason
// and the proberFunc adapter from controller_test.go — same package, no
// build tag on that file, so those helpers compile into this build too.
package workshopprobe

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

func TestMain(m *testing.M) {
	code := testenv.RunPackage(m)
	// These tests boot a shared SpiceDB container; stop it after the
	// apiserver so a package run leaves no container behind — mirrors
	// pkg/controllers/agentidentity/sharedenv_test.go.
	testspicedb.StopShared()
	os.Exit(code)
}

// newSpiceDBClient boots a per-test datastore loaded with the canonical
// schema and returns a client bound to it. Mirrors pkg/controllers/
// agentidentity/platform_link_integration_test.go's helper of the same
// name/shape.
func newSpiceDBClient(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sessionLabels is the pair the Workshop controller stamps on the workshop
// namespace at provisioning (BuildWorkshopNamespace,
// pkg/controllers/workshop/rbac.go) — the ONLY place this controller trusts
// the session attribution from.
func sessionLabels(sessNS, sessName string) map[string]string {
	return map[string]string{
		v1alpha1.LabelWorkshopSessionNamespace: sessNS,
		v1alpha1.LabelWorkshopSessionName:      sessName,
	}
}

// TestWorkshopProbe_TuplePresent_ProbeSucceeds is the positive control: with
// the workshop:<id>#build tuple genuinely written in SpiceDB, the probe runs
// and its result lands on status.
func TestWorkshopProbe_TuplePresent_ProbeSucceeds(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace = "ws-tuple-ok"
		sessNS      = "default"
		sessName    = "builder-ok"
	)
	require.NoError(t, env.Client.Create(ctx, testNamespace(wsNamespace, sessionLabels(sessNS, sessName))), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, testWorkshop(sessNS, sessName, 2)), "create Workshop")
	require.NoError(t, spdb.EnsureWorkshopSubjects(ctx, wsNamespace, sessNS, sessName, identity.CanonicalUserID{}), "seed the workshop#build tuple")

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, env.Client.Create(ctx, wp), "create WorkshopProbe")

	wantTools := []v1alpha1.WorkshopProbedTool{
		{Name: "read_file", Description: "reads a file"},
		{Name: "write_file", Description: "writes a file"},
	}
	r := &Reconciler{
		Client: env.Client,
		Build:  spdb,
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			return ProbeResult{Tools: wantTools, ResolvedDigest: "sha256:realdigest"}, nil
		}),
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err)

	got := getProbe(t, env.Client, wp.Namespace, wp.Name)
	assert.Equal(t, v1alpha1.WorkshopProbePhaseSucceeded, got.Status.Phase)
	assert.Equal(t, wantTools, got.Status.Tools)
	assert.Equal(t, "sha256:realdigest", got.Status.ResolvedDigest)
	assert.Equal(t, v1alpha1.ReasonWorkshopProbeSucceeded, probedConditionReason(got))
}

// TestWorkshopProbe_NoTuple_DeniedBeforeProbe is the load-bearing security
// test: with NO workshop:<id>#build tuple written, the probe must be denied
// — and the Prober must never run at all, proven against a real SpiceDB, not
// a fake that could be made to say anything.
func TestWorkshopProbe_NoTuple_DeniedBeforeProbe(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace = "ws-tuple-denied"
		sessNS      = "default"
		sessName    = "builder-denied"
	)
	require.NoError(t, env.Client.Create(ctx, testNamespace(wsNamespace, sessionLabels(sessNS, sessName))), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, testWorkshop(sessNS, sessName, 2)), "create Workshop")
	// Deliberately NO spdb.EnsureWorkshopSubjects call: the tuple is absent.

	wp := testProbe(wsNamespace, "probe-1")
	require.NoError(t, env.Client.Create(ctx, wp), "create WorkshopProbe")

	called := false
	r := &Reconciler{
		Client: env.Client,
		Build:  spdb,
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			called = true
			return ProbeResult{}, nil
		}),
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err, "a denied probe is a recorded result, not a reconcile error")

	got := getProbe(t, env.Client, wp.Namespace, wp.Name)
	assert.Equal(t, v1alpha1.WorkshopProbePhaseFailed, got.Status.Phase)
	assert.Equal(t, v1alpha1.ReasonWorkshopProbeDenied, probedConditionReason(got))
	assert.False(t, called, "the probe must never run without the workshop#build tuple")
}

// TestWorkshopProbe_ConcurrencyCap_RequeuesWithoutProbing proves the cap
// blocks BEFORE the (real, tuple-satisfied) authorization gate would ever
// let the probe run — one Running probe already occupies the namespace's
// only slot (maxConcurrentProbes: 1), so a second reconcile must requeue
// without ever calling the Prober.
func TestWorkshopProbe_ConcurrencyCap_RequeuesWithoutProbing(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		wsNamespace = "ws-tuple-cap"
		sessNS      = "default"
		sessName    = "builder-cap"
	)
	require.NoError(t, env.Client.Create(ctx, testNamespace(wsNamespace, sessionLabels(sessNS, sessName))), "create workshop namespace")
	require.NoError(t, env.Client.Create(ctx, testWorkshop(sessNS, sessName, 1)), "create Workshop with maxConcurrentProbes: 1")
	require.NoError(t, spdb.EnsureWorkshopSubjects(ctx, wsNamespace, sessNS, sessName, identity.CanonicalUserID{}), "seed the workshop#build tuple")

	already := testProbe(wsNamespace, "probe-already-running")
	require.NoError(t, env.Client.Create(ctx, already), "create the already-running probe")
	already.Status.Phase = v1alpha1.WorkshopProbePhaseRunning
	require.NoError(t, env.Client.Status().Update(ctx, already), "mark it Running")

	wp := testProbe(wsNamespace, "probe-blocked")
	require.NoError(t, env.Client.Create(ctx, wp), "create the probe that should be blocked")

	called := false
	r := &Reconciler{
		Client: env.Client,
		Build:  spdb,
		Prober: proberFunc(func(context.Context, *v1alpha1.WorkshopProbe) (ProbeResult, error) {
			called = true
			return ProbeResult{}, nil
		}),
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wp.Namespace, Name: wp.Name}})
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter.Nanoseconds(), int64(0), "an at-cap probe must requeue rather than run immediately")
	assert.False(t, called, "the Prober must never run while every slot is taken")

	got := getProbe(t, env.Client, wp.Namespace, wp.Name)
	assert.NotEqual(t, v1alpha1.WorkshopProbePhaseFailed, got.Status.Phase, "a cap-blocked probe is not refused")
	assert.NotEqual(t, v1alpha1.WorkshopProbePhaseSucceeded, got.Status.Phase)
}
