package installcmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/initpipeline"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestBuildCoreComponentsDependsOn asserts that channelsd, operator, webd, and
// authzd carry the expected DependsOn edges so the executor places their WAIT
// in a later wave than their runtime deps, and that webd/authzd have a non-nil
// Wait (they are awaited-only, with no Manifests, exactly like operator). It
// also asserts extractord is tracked at all — the checklist previously had
// zero mentions of it, so a stalled rollout produced no install-time signal.
// The bundle is zero-value: buildCoreComponents only stores bundle.Typed in
// closures and never invokes it during component construction.
func TestBuildCoreComponentsDependsOn(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault) /*not local*/, ExternalSpiceDB{} /*not external*/)

	byName := make(map[string]int, len(comps))
	for i, c := range comps {
		byName[c.Name] = i
	}

	chIdx, ok := byName["channelsd"]
	require.True(t, ok, "channelsd component must exist in buildCoreComponents output")
	assert.ElementsMatch(t, []string{"NATS", "SpiceDB"}, comps[chIdx].DependsOn,
		"channelsd must DependsOn NATS and SpiceDB so its wait window opens after they are Ready")

	opIdx, ok := byName["operator"]
	require.True(t, ok, "operator component must exist in buildCoreComponents output")
	assert.ElementsMatch(t, []string{"NATS", "SpiceDB", "postgres"}, comps[opIdx].DependsOn,
		"operator must DependsOn NATS, SpiceDB, and postgres so its wait window opens after they are Ready")

	exIdx, ok := byName["extractord"]
	require.True(t, ok, "extractord component must exist in buildCoreComponents output")
	assert.Empty(t, comps[exIdx].DependsOn,
		"extractord has no database, SpiceDB, NATS, or Secret dependency — it is a pure HTTP function service")
	assert.NotNil(t, comps[exIdx].Wait, "extractord must have a non-nil Wait so a stalled rollout is tracked")
	assert.Nil(t, comps[exIdx].Manifests, "extractord must have nil Manifests (it ships in the base bundle, not via the pipeline apply)")
	assert.True(t, comps[exIdx].Optional,
		"extractord must be Optional: a stalled rollout degrades attachment extraction only, not chat/memory/auth, so it warns rather than aborting the whole install")
	assert.Less(t, exIdx, opIdx,
		"extractord is listed ahead of operator, which dials it over EXTRACTORD_ENDPOINT")

	grIdx, ok := byName["graphiti"]
	require.True(t, ok, "graphiti component must exist in buildCoreComponents output")
	assert.ElementsMatch(t, []string{"neo4j"}, comps[grIdx].DependsOn,
		"graphiti must DependsOn neo4j: it dials bolt://spicebox-neo4j…:7687 during startup and EXITS (255, "+
			"\"Cannot resolve address\") when neo4j is not up, so starting it in parallel with a cold neo4j "+
			"guarantees a crash-loop and a skipped component")
	require.NotNil(t, comps[grIdx].Wait, "graphiti must have a non-nil Wait")
	assert.Equal(t, graphitiDeadline, comps[grIdx].Wait.Deadline,
		"graphiti must carry an EXPLICIT hard deadline: being gated, it waits under the run context rather "+
			"than the optional-tail short-circuit, so this is how long a broken graphiti can hold an "+
			"otherwise-finished install")

	wbIdx, ok := byName["webd"]
	require.True(t, ok, "webd component must exist in buildCoreComponents output")
	assert.ElementsMatch(t, []string{"NATS", "SpiceDB", "operator"}, comps[wbIdx].DependsOn,
		"webd must DependsOn NATS, SpiceDB, and operator (it uses NATS_URL + SPICEDB_ENDPOINT + OPERATOR_MEMORY_URL)")
	assert.NotNil(t, comps[wbIdx].Wait, "webd must have a non-nil Wait (it is a Wait-only component)")
	assert.Nil(t, comps[wbIdx].Manifests, "webd must have nil Manifests (it ships in the base bundle, not via the pipeline apply)")

	azIdx, ok := byName["authzd"]
	require.True(t, ok, "authzd component must exist in buildCoreComponents output")
	assert.ElementsMatch(t, []string{"NATS", "SpiceDB", "operator"}, comps[azIdx].DependsOn,
		"authzd must DependsOn NATS, SpiceDB, and operator (it uses NATS_URL + SPICEDB_ENDPOINT + MEMORY_URL→operator)")
	assert.NotNil(t, comps[azIdx].Wait, "authzd must have a non-nil Wait (it is a Wait-only component)")
	assert.Nil(t, comps[azIdx].Manifests, "authzd must have nil Manifests (it ships in the base bundle, not via the pipeline apply)")
}

// componentByName indexes a Component slice by Name for lookup in the tests below.
func componentByName(comps []initpipeline.Component) map[string]initpipeline.Component {
	m := make(map[string]initpipeline.Component, len(comps))
	for _, c := range comps {
		m[c.Name] = c
	}
	return m
}

// TestBuildCoreComponents_RemoteAddsOperatorAndDatabase asserts that a remote
// (non-local, non-external) install wires up the SpiceDB operator wait, the
// Postgres-backed SpiceDBCluster CR, and the one-shot SpiceDBDatabase Job that
// creates the 'spicedb' database ahead of the operator's own migration Job.
func TestBuildCoreComponents_RemoteAddsOperatorAndDatabase(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault) /*not local*/, ExternalSpiceDB{} /*not external*/)
	by := componentByName(comps)

	require.Contains(t, by, "SpiceDBOperator")
	require.Contains(t, by, "SpiceDBDatabase")
	sp, ok := by["SpiceDB"]
	require.True(t, ok)
	assert.Contains(t, sp.DependsOn, "SpiceDBOperator")
	assert.Contains(t, sp.DependsOn, "SpiceDBDatabase")

	db := by["SpiceDBDatabase"]
	assert.Contains(t, db.DependsOn, "postgres")

	// Remote SpiceDB CR is postgres-backed.
	docs, err := sp.Manifests()
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Contains(t, string(docs[0]), "datastoreEngine: postgres")
}

// TestBuildCoreComponents_LocalMemoryNoDatabase asserts that --local installs
// use the in-memory SpiceDB datastore and skip the SpiceDBDatabase Job (no
// Postgres database to create).
func TestBuildCoreComponents_LocalMemoryNoDatabase(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyLocal), ExternalSpiceDB{} /*not external*/)
	by := componentByName(comps)

	require.Contains(t, by, "SpiceDBOperator")
	assert.NotContains(t, by, "SpiceDBDatabase", "memory engine needs no Postgres database")
	sp := by["SpiceDB"]
	assert.Contains(t, sp.DependsOn, "SpiceDBOperator")
	assert.NotContains(t, sp.DependsOn, "SpiceDBDatabase")
	docs, err := sp.Manifests()
	require.NoError(t, err)
	assert.Contains(t, string(docs[0]), "datastoreEngine: memory")
}

// TestBuildCoreComponents_ExternalOmitsSpiceDBDeploy asserts that when the user
// brings their own external SpiceDB instance, buildCoreComponents deploys none
// of the SpiceDB/SpiceDBOperator/SpiceDBDatabase components, and no remaining
// component still DependsOn the now-absent "SpiceDB".
func TestBuildCoreComponents_ExternalOmitsSpiceDBDeploy(t *testing.T) {
	comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault), ExternalSpiceDB{Endpoint: "spicedb.example.net:443"})
	by := componentByName(comps)
	assert.NotContains(t, by, "SpiceDB", "external SpiceDB: we deploy nothing")
	assert.NotContains(t, by, "SpiceDBOperator")
	assert.NotContains(t, by, "SpiceDBDatabase")
	// Consumers must not depend on a component that no longer exists.
	for _, c := range comps {
		assert.NotContains(t, c.DependsOn, "SpiceDB", "%s still DependsOn SpiceDB", c.Name)
	}
}

// buildCoreComponentsChannelsdArgs returns the channelsd component's
// generated Deployment container args, for asserting the --spicedb-insecure
// flip in TestBuildCoreComponents_ExternalTLSFlipsChannelsdInsecureFlag.
func buildCoreComponentsChannelsdArgs(t *testing.T, comps []initpipeline.Component) []string {
	t.Helper()
	by := componentByName(comps)
	ch, ok := by["channelsd"]
	require.True(t, ok, "channelsd component must exist")
	require.NotNil(t, ch.Manifests, "channelsd must carry Manifests")
	ms, err := ch.Manifests()
	require.NoError(t, err)
	for _, m := range ms {
		docs, err := manifests.Split(m)
		require.NoError(t, err)
		for _, d := range docs {
			if d.GetKind() != "Deployment" {
				continue
			}
			containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
			require.NoError(t, err)
			require.True(t, found)
			args, err := stringSlice(containers[0].(map[string]any)["args"])
			require.NoError(t, err)
			return args
		}
	}
	t.Fatal("no channelsd Deployment doc found")
	return nil
}

// TestBuildCoreComponents_ExternalTLSFlipsChannelsdInsecureFlag asserts that
// buildCoreComponents' channelsd component appends --spicedb-insecure=false
// to the generated Deployment's container args only when the caller brought
// an external SpiceDB reached over TLS (ext.enabled() && !ext.Insecure); it
// is absent for --external-spicedb-insecure and for a non-external (bundled)
// install, both of which keep channelsd's plaintext cobra-flag default.
func TestBuildCoreComponents_ExternalTLSFlipsChannelsdInsecureFlag(t *testing.T) {
	t.Run("external + TLS: flag present", func(t *testing.T) {
		comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault), ExternalSpiceDB{Endpoint: "spicedb.example.net:443"})
		assert.Contains(t, buildCoreComponentsChannelsdArgs(t, comps), channelsdInsecureFlag)
	})

	t.Run("external + --external-spicedb-insecure: flag absent", func(t *testing.T) {
		comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault), ExternalSpiceDB{Endpoint: "spicedb.example.net:50051", Insecure: true})
		assert.NotContains(t, buildCoreComponentsChannelsdArgs(t, comps), channelsdInsecureFlag)
	})

	t.Run("not external: flag absent", func(t *testing.T) {
		comps := buildCoreComponents(&kube.Bundle{}, manifests.Tags{}, "", cloud.MustFor(cloud.KeyDefault), ExternalSpiceDB{})
		assert.NotContains(t, buildCoreComponentsChannelsdArgs(t, comps), channelsdInsecureFlag)
	})
}

// TestRunInstallNilRailDryRunSucceeds pins 11b: RunInstall accepts the new
// trailing rail parameter, and a non-wizard caller passing nil rail runs the
// existing (no-rail) path unchanged. Driven through --dry-run=client so it needs
// no cluster and never reaches an apply or the reporter's TTY branch — it
// exercises the nil-rail call path directly and asserts the plan still renders.
// (progress.NewWithRail's own byte-identity of nil-rail vs no-rail is proven in
// pkg progress' TestChecklistRail_NilRailIsUnchanged; here we prove RunInstall's
// non-wizard callers are unaffected by the new parameter.)
func TestRunInstallNilRailDryRunSucceeds(t *testing.T) {
	var out bytes.Buffer
	err := RunInstall(
		context.Background(), context.Background(), InstallConfig{
			Out:                 &out,
			G:                   &apcmd.Globals{},
			Tags:                manifests.Tags{},
			DryRun:              apcmd.DryRunClient,
			Routing:             WebdRoutingOpts{},
			PinningMode:         "",
			Strat:               cloud.MustFor(cloud.KeyDefault),
			Ext:                 ExternalSpiceDB{},
			Workspace:           WorkspaceResolveOptions{},
			Stateful:            StatefulResolveOptions{},
			Artifact:            ArtifactStoreOptions{},
			ImagePullSecret:     "",
			MirrorDeps:          false,
			ImagesPrebaked:      false,
			ClampUndersizedPVCs: false,
			WithoutBuilder:      true,
			BuilderStarters:     nil,
			Rail:                nil, // a non-wizard caller gets the no-rail reporter
		},
	)
	require.NoError(t, err, "a nil-rail --dry-run=client install must succeed")
	assert.Contains(t, out.String(), "spicebox-operator", "the plan still renders for a non-wizard (nil-rail) caller")
}

func TestInstallDryRunPrintsObjects(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"install", "--dry-run=client"})
	require.NoError(t, root.Execute(), "install --dry-run=client")

	out := stdout.String()
	for _, want := range []string{
		"Namespace",
		"agentprimitives-system",
		"CustomResourceDefinition",
		"agentclasses.agentprimitives.authzed.com",
		"Deployment",
		"spicebox-operator",
	} {
		assert.Containsf(t, out, want, "install --dry-run output missing %q", want)
	}
}

// TestInstallDryRun_IncludesSpiceDBOperatorAndCRD asserts that install --dry-run
// output includes the SpiceDB operator bundle: the CustomResourceDefinition for
// spicedbclusters.authzed.com and the spicedb-operator Deployment.
func TestInstallDryRun_IncludesSpiceDBOperatorAndCRD(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"install", "--dry-run=client"})
	require.NoError(t, root.Execute(), "install --dry-run=client")

	out := stdout.String()
	for _, want := range []string{
		"CustomResourceDefinition", "spicedbclusters.authzed.com",
		"Deployment", "spicedb-operator",
	} {
		assert.Containsf(t, out, want, "install --dry-run output missing %q", want)
	}
}

// TestSpiceDBClusterServing covers spicedbClusterServing's readiness wait for
// the operator-created SpiceDB Deployment. The operator creates the Deployment
// asynchronously after reconciling the SpiceDBCluster, so a NotFound must be
// tolerated as "not ready yet" (not surfaced as an error) — unlike the generic
// deploymentRolledOut helper, which treats NotFound as fatal.
func TestSpiceDBClusterServing(t *testing.T) {
	const ns, name = "agentprimitives-system", "spicebox-spicedb-spicedb"

	cases := []struct {
		name       string
		deployment *appsv1.Deployment // nil → Deployment absent (NotFound)
		reactErr   error              // non-nil → Get returns this error instead
		wantReady  bool
		wantErr    bool
	}{
		{
			name:       "Deployment absent (NotFound) → not ready, no error, keep polling",
			deployment: nil,
			wantReady:  false,
			wantErr:    false,
		},
		{
			name: "Deployment present, Generation != ObservedGeneration → not ready, no error",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					UpdatedReplicas:    2,
					Replicas:           2,
					AvailableReplicas:  2,
				},
			},
			wantReady: false,
			wantErr:   false,
		},
		{
			name: "Deployment present, observed but partial rollout → not ready, no error",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					UpdatedReplicas:    1,
					Replicas:           2,
					AvailableReplicas:  1,
				},
			},
			wantReady: false,
			wantErr:   false,
		},
		{
			name: "Deployment present, fully rolled out → ready, no error",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					UpdatedReplicas:    2,
					Replicas:           2,
					AvailableReplicas:  2,
				},
			},
			wantReady: true,
			wantErr:   false,
		},
		{
			name:       "Get returns a non-NotFound error (RBAC Forbidden) → surfaced, not swallowed",
			deployment: nil,
			reactErr:   apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, name, errors.New("forbidden")),
			wantReady:  false,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.deployment != nil {
				objs = append(objs, tc.deployment)
			}
			cs := fake.NewSimpleClientset(objs...)
			if tc.reactErr != nil {
				cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.reactErr
				})
			}

			ready, err := spicedbClusterServing(cs, ns, name)(context.Background())

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantReady, ready)
		})
	}
}
