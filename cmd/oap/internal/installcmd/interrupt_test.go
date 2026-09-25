package installcmd

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func TestAppliedSet_RecordsConcurrentAddsAndSortsNames(t *testing.T) {
	var a appliedSet
	require.True(t, a.empty(), "a fresh appliedSet must be empty")

	// applyOne calls OnApplied from the executor's concurrent workers, so the
	// recorder must tolerate parallel adds. This fails under -race if the
	// implementation drops the mutex.
	var wg sync.WaitGroup
	for _, name := range []string{"nats", "spicedb", "postgres", "neo4j"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			a.add(n)
		}(name)
	}
	wg.Wait()

	assert.False(t, a.empty(), "after adds the set must not be empty")
	assert.Equal(t, []string{"nats", "neo4j", "postgres", "spicedb"}, a.names(),
		"names must be sorted so the interrupt message is deterministic")
}

func TestAppliedSet_DeduplicatesRepeatedComponent(t *testing.T) {
	var a appliedSet
	a.add("nats")
	a.add("nats")
	assert.Equal(t, []string{"nats"}, a.names(),
		"a component applied twice must appear once")
}

// baseRegionDocs is a miniature stand-in for the base manifest region: the
// pieces of it whose presence on a fresh cluster is exactly what makes an
// interrupted first install worth rolling back.
func baseRegionDocs(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	doc := func(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetNamespace(namespace)
		u.SetName(name)
		return u
	}
	return []*unstructured.Unstructured{
		doc("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "agentsessions.agentprimitives.authzed.com"),
		doc("v1", "Namespace", "", "agentprimitives-system"),
		doc("apps/v1", "Deployment", "agentprimitives-system", "spicebox-operator"),
	}
}

// TestInterruptAfterBaseRegion_OffersTeardownOnlyOnAFreshCluster pins the whole
// point of the interrupt gate: the base region — the CRDs, the
// agentprimitives-system Namespace and the Deployments — lands BEFORE the
// pipeline executor runs, so a Ctrl-C anywhere after it (the imperative secret
// ensures, the stateful-storage probe) must see a run that mutated the cluster.
//
// It drives the real apply path rather than hand-populating the set, because
// the defect was precisely that the production path recorded nothing: the loop
// narrated "applied …" for every doc while the recorder stayed empty, so a
// first install interrupted at the stateful-storage wait was told "nothing was
// applied before the interrupt" with its CRDs and Deployments live.
func TestInterruptAfterBaseRegion_OffersTeardownOnlyOnAFreshCluster(t *testing.T) {
	cases := []struct {
		name      string
		hadPrior  bool
		wantOffer bool
	}{
		{
			name:      "fresh cluster: the base region counts as applied, so teardown is offered",
			hadPrior:  false,
			wantOffer: true,
		},
		{
			// `oap install` is the documented upgrade path. Re-applying the base
			// region over a live install is ordinary, and `oap clean` is a FULL
			// uninstall — recording the region must not turn that into an offer.
			name:      "pre-existing install: re-applying the base region must NOT offer teardown",
			hadPrior:  true,
			wantOffer: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runtime.NewScheme()
			require.NoError(t, scheme.AddToScheme(s))
			require.NoError(t, apiextv1.AddToScheme(s))
			dyn := dynfake.NewSimpleDynamicClient(s)

			applied := &appliedSet{}
			ctx := withAppliedSet(context.Background(), applied)

			var buf bytes.Buffer
			require.NoError(t,
				applyBaseRegion(ctx, dyn, baseRegionDocs(t), nil /*mirrorMap*/, mutationsFor(ctx, newReporter(t, &buf))),
				"the base region must apply cleanly against a fresh cluster")

			offer, why := interruptTeardownDecision(applied, tc.hadPrior)
			assert.Equal(t, tc.wantOffer, offer, "teardown offer (reason given: %q)", why)
			if tc.wantOffer {
				assert.Empty(t, why, "an offered teardown needs no refusal reason")
			}
		})
	}
}

// TestInstallMutations_EnsureRecordsOnlyWhatSucceeded covers the imperative
// half: the prerequisite Secret/ConfigMap writes are narrated and recorded in
// one call, and a step that failed must not be claimed as applied — the
// interrupt summary tells the operator what to undo by hand.
func TestInstallMutations_EnsureRecordsOnlyWhatSucceeded(t *testing.T) {
	var buf bytes.Buffer
	applied := &appliedSet{}
	m := mutationsFor(withAppliedSet(context.Background(), applied), newReporter(t, &buf))

	ctx := context.Background()
	require.NoError(t, m.ensure(ctx, "postgres token", func(context.Context) error { return nil }))

	stepErr := errors.New("ensure neo4j token: apiserver said no")
	require.ErrorIs(t, m.ensure(ctx, "neo4j token", func(context.Context) error { return stepErr }), stepErr,
		"ensure must surface the step's own error unwrapped, so each step keeps its message")

	assert.Equal(t, []string{"postgres token"}, applied.names(),
		"a failed step must not be recorded as applied")
	assert.Contains(t, buf.String(), "ensure postgres token", "each step is narrated as it starts")
	assert.Contains(t, buf.String(), "ensure neo4j token",
		"a step is narrated before it runs, so a failure is attributable")
}

// recordingClean captures whether the destructive teardown was invoked, and
// with what scope.
type recordingClean struct {
	calls int
	opts  cleanOptions
}

func (r *recordingClean) fn(_ context.Context, opts cleanOptions) error {
	r.calls++
	r.opts = opts
	return nil
}

func appliedWith(t *testing.T, names ...string) *appliedSet {
	t.Helper()
	a := &appliedSet{}
	for _, n := range names {
		a.add(n)
	}
	return a
}

func TestHandleInstallInterrupt(t *testing.T) {
	cases := []struct {
		name       string
		applied    *appliedSet
		hadPrior   bool
		confirms   bool
		wantCleans int
	}{
		{
			// THE BLOCKER. `oap install` is the documented upgrade path, so this is
			// an ordinary re-run against a live cluster. `oap clean` is a FULL
			// uninstall, so tearing down here destroys work this run did not do.
			name:       "pre-existing install: must never tear down, even if the operator says yes",
			applied:    appliedWith(t, "nats"),
			hadPrior:   true,
			confirms:   true,
			wantCleans: 0,
		},
		{
			name:       "nothing applied: must never tear down, even if the operator says yes",
			applied:    &appliedSet{},
			hadPrior:   false,
			confirms:   true,
			wantCleans: 0,
		},
		{
			name:       "fresh cluster, work applied, operator declines: no teardown",
			applied:    appliedWith(t, "nats", "spicedb"),
			hadPrior:   false,
			confirms:   false,
			wantCleans: 0,
		},
		{
			name:       "fresh cluster, work applied, operator accepts: teardown runs",
			applied:    appliedWith(t, "nats", "spicedb"),
			hadPrior:   false,
			confirms:   true,
			wantCleans: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rc recordingClean
			var out bytes.Buffer

			err := handleInstallInterrupt(context.Background(), interruptDeps{
				Out:      &out,
				Applied:  tc.applied,
				HadPrior: tc.hadPrior,
				Confirm:  func(string) bool { return tc.confirms },
				Clean:    rc.fn,
			})

			require.NoError(t, err)
			assert.Equal(t, tc.wantCleans, rc.calls, "teardown invocations")
		})
	}
}

func TestHandleInstallInterrupt_TeardownGetsCleansOwnBudgetNotInstalls(t *testing.T) {
	var rc recordingClean
	var out bytes.Buffer

	require.NoError(t, handleInstallInterrupt(context.Background(), interruptDeps{
		Out: &out, Applied: appliedWith(t, "nats"), HadPrior: false,
		Confirm: func(string) bool { return true },
		Clean:   rc.fn,
	}))

	require.Equal(t, 1, rc.calls)
	// A full teardown routinely outlasts oap install's --timeout (120s by
	// default), and being cut off mid-teardown is worse than taking longer, so
	// the interrupt path must not hand the install's budget down.
	assert.Equal(t, cleanDefaultTimeout, rc.opts.Timeout,
		"teardown must get oap clean's own budget")
	assert.Greater(t, rc.opts.Timeout, installDefaultTimeout,
		"oap install's own budget is too short for a teardown")
	assert.True(t, rc.opts.Yes, "the teardown must not re-prompt")
}

func TestHandleInstallInterrupt_PrintsFullScopeBeforeAsking(t *testing.T) {
	var rc recordingClean
	var out bytes.Buffer

	var promptedAfter string
	require.NoError(t, handleInstallInterrupt(context.Background(), interruptDeps{
		Out: &out, Applied: appliedWith(t, "nats"), HadPrior: false,
		Confirm: func(string) bool { promptedAfter = out.String(); return false },
		Clean:   rc.fn,
	}))

	// Yes:true suppresses oap clean's own scope banner, so this is the only place
	// the operator is told what is about to be deleted.
	require.NotEmpty(t, promptedAfter, "the scope must be printed BEFORE the prompt")
	assert.Contains(t, promptedAfter, "ENTIRE installation")
	assert.Contains(t, promptedAfter, "spicebox-postgres-data")
	assert.Contains(t, promptedAfter, "CRD")
	assert.Equal(t, 0, rc.calls, "declining must not tear down")
}

func TestHandleInstallInterrupt_DeclinedTeardownExplainsAndNamesWhatLanded(t *testing.T) {
	var rc recordingClean
	var out bytes.Buffer

	require.NoError(t, handleInstallInterrupt(context.Background(), interruptDeps{
		Out: &out, Applied: appliedWith(t, "nats", "spicedb"), HadPrior: true,
		Confirm: func(string) bool { return true },
		Clean:   rc.fn,
	}))

	got := out.String()
	assert.Contains(t, got, "existing installation",
		"the operator must be told why no teardown was offered")
	assert.Contains(t, got, "nats, spicedb",
		"the operator must be told what this run applied, so they can undo it themselves")
	assert.Contains(t, got, "oap clean",
		"the operator must be pointed at the command that removes everything")
}

func TestPreexistingInstall(t *testing.T) {
	const crdKey = "agentsessions.agentprimitives.authzed.com"
	errNoAPIServer := errors.New("apiserver unreachable")

	cases := []struct {
		name      string
		bundle    func(t *testing.T) *kube.Bundle
		wantPrior bool
		wantErr   bool
	}{
		{
			name: "CRD present: reports a prior install, no error",
			bundle: func(t *testing.T) *kube.Bundle {
				t.Helper()
				c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
					&apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crdKey}},
				).Build()
				return &kube.Bundle{Controller: c}
			},
			wantPrior: true,
		},
		{
			name: "CRD absent: reports a fresh cluster, no error",
			bundle: func(t *testing.T) *kube.Bundle {
				t.Helper()
				return &kube.Bundle{Controller: fake.NewClientBuilder().WithScheme(kube.Scheme).Build()}
			},
			wantPrior: false,
		},
		{
			name: "probe errors: fail safe to a prior install, and return the error",
			bundle: func(t *testing.T) *kube.Bundle {
				t.Helper()
				c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return apierrors.NewInternalError(errNoAPIServer)
					},
				}).Build()
				return &kube.Bundle{Controller: c}
			},
			wantPrior: true,
			wantErr:   true,
		},
		{
			name:      "no client at all: fail safe to a prior install, and return the error",
			bundle:    func(t *testing.T) *kube.Bundle { t.Helper(); return nil },
			wantPrior: true,
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prior, err := preexistingInstall(context.Background(), tc.bundle(t))
			assert.Equal(t, tc.wantPrior, prior)
			if tc.wantErr {
				assert.Error(t, err, "the caller must be able to surface why the probe failed")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
