package install

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

func graphFixture(t *testing.T) *oap.Bundle {
	t.Helper()
	root := validBundle()
	child := graphChild("child")
	addGraphChild(child, graphChild("helper"))
	addGraphChild(root, child)
	addGraphChild(root, graphChild("sibling"))
	require.NoError(t, oap.ValidateDependencyGraph(root))
	return root
}

func graphChild(name string) *oap.Bundle {
	b := validBundle()
	b.Manifest.Agent.Name = name
	b.Manifests = []byte(strings.ReplaceAll(string(b.Manifests), "fixture-agent", name))
	return b
}

func addGraphChild(parent, child *oap.Bundle) {
	descriptor := oap.RequiredAgent{Path: "dependencies/" + child.Manifest.Agent.Name}
	parent.Manifest.Requires.Agents = append(parent.Manifest.Requires.Agents, descriptor)
	parent.Dependencies = append(parent.Dependencies, &oap.Dependency{Descriptor: descriptor, Bundle: child})
	if len(parent.Dependencies) == 1 {
		parent.Manifests = append(parent.Manifests, []byte("  subagents:\n")...)
	}
	parent.Manifests = append(parent.Manifests, []byte("  - "+child.Manifest.Agent.Name+"\n")...)
}

type fakeNodeRunner struct {
	prepare    func(oap.DependencyPath) error
	apply      func(oap.DependencyPath) error
	rollback   func(oap.DependencyPath) error
	operations []string
	inputs     []NodeInput
}

func (r *fakeNodeRunner) PrepareNode(_ context.Context, in NodeInput) (*Prepared, error) {
	r.operations = append(r.operations, "prepare:"+in.Path.String())
	r.inputs = append(r.inputs, in)
	if r.prepare != nil {
		if err := r.prepare(in.Path); err != nil {
			return nil, err
		}
	}
	return &Prepared{InstallName: in.PhysicalName, Options: InstallOpts{DependencyPath: in.Path}}, nil
}
func (r *fakeNodeRunner) ApplyNode(_ context.Context, p *Prepared) (*Result, error) {
	r.operations = append(r.operations, "apply:"+p.Options.DependencyPath.String())
	if r.apply != nil {
		if err := r.apply(p.Options.DependencyPath); err != nil {
			return nil, err
		}
	}
	return &Result{Name: p.InstallName}, nil
}
func (r *fakeNodeRunner) RollbackNode(_ context.Context, p *Prepared) error {
	r.operations = append(r.operations, "rollback:"+p.Options.DependencyPath.String())
	if r.rollback != nil {
		return r.rollback(p.Options.DependencyPath)
	}
	return nil
}
func failOn(path oap.DependencyPath, name string) error {
	if len(path) > 0 && path[len(path)-1] == name {
		return errors.New("planned failure")
	}
	return nil
}

// clusterNodeRunner keeps graph orchestration real and substitutes only the
// external API server with controller-runtime's client in these unit tests.
type clusterNodeRunner struct {
	c        client.Client
	rootName string
	opts     InstallOpts
	secrets  map[string][]SecretSpec
}

func (r *clusterNodeRunner) PrepareNode(ctx context.Context, in NodeInput) (*Prepared, error) {
	opts := r.opts
	opts.Namespace = "agents"
	opts.RootInstallName = r.rootName
	opts.DependencyPath = in.Path
	opts.ResourceNames = in.ResourceNames
	opts.DirectNames = in.DirectNames
	if len(in.Path) > 0 {
		opts.SourceKind, opts.SourceRef, opts.SourceDigest = in.SourceKind, in.SourceRef, in.PackedDigest
	}
	return Prepare(ctx, r.c, in.Bundle, nil, r.secrets[in.Path.String()], opts)
}
func (r *clusterNodeRunner) ApplyNode(ctx context.Context, p *Prepared) (*Result, error) {
	return ApplyPrepared(ctx, r.c, p)
}
func (r *clusterNodeRunner) RollbackNode(ctx context.Context, p *Prepared) error {
	return RollbackPrepared(ctx, r.c, p)
}

func TestExecuteGraphPreparesAllNodesBeforeApplyingLeavesFirst(t *testing.T) {
	r := &fakeNodeRunner{}
	p, err := PlanGraph(context.Background(), graphFixture(t), "", r)
	require.NoError(t, err)
	assert.Equal(t, []string{"prepare:", "prepare:child", "prepare:child > helper", "prepare:sibling"}, r.operations)
	result, err := ExecuteGraph(context.Background(), p, r)
	require.NoError(t, err)
	assert.Equal(t, []string{"apply:child > helper", "apply:child", "apply:sibling", "apply:"}, r.operations[4:])
	assert.Equal(t, "fixture-agent", result.RootName)
	require.Len(t, result.Nodes, 4)
	assert.Equal(t, "fixture-agent-child-helper", result.Nodes[0].Name)
	assert.Equal(t, oap.DependencyPath{"child", "helper"}, result.Nodes[0].Path)
	assert.Equal(t, "agents/child/helper", r.inputs[2].SourceRef)
	assert.Equal(t, "embedded", r.inputs[2].SourceKind)
	assert.NotEmpty(t, r.inputs[2].PackedDigest)
	assert.Equal(t, "fixture-agent-child-helper", r.inputs[1].DirectNames["helper"])
}

func TestExecuteGraphPrepareFailureHasPathAndNoMutation(t *testing.T) {
	r := &fakeNodeRunner{prepare: func(path oap.DependencyPath) error { return failOn(path, "helper") }}
	_, err := PlanGraph(context.Background(), graphFixture(t), "", r)
	require.ErrorContains(t, err, "fixture-agent > child > helper")
	assert.NotContains(t, strings.Join(r.operations, ","), "apply:")
}

func TestPlanGraphPreservesRootNaming(t *testing.T) {
	for _, tc := range []struct{ name, owner, physical, child string }{
		{"", "fixture-agent", "fixture-agent", "fixture-agent-child"},
		{"custom", "custom", "custom-fixture-agent", "custom-child"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			r := &fakeNodeRunner{}
			p, err := PlanGraph(context.Background(), graphFixture(t), tc.name, r)
			require.NoError(t, err)
			assert.Equal(t, tc.owner, p.RootName)
			assert.Equal(t, tc.physical, r.inputs[0].PhysicalName)
			assert.Equal(t, tc.physical, r.inputs[0].ResourceNames["AgentClass/fixture-agent"])
			assert.Equal(t, tc.child, r.inputs[0].DirectNames["child"])
		})
	}
}

func TestPlanGraphNamesDependencyChannelsAndWizardCredentialSecretsPrivately(t *testing.T) {
	root := graphFixture(t)
	child := root.Dependencies[0].Bundle
	child.Manifest.Requires.Channels = []oap.RequiredChannel{{
		Kind: "github", Role: v1alpha1.ChannelRoleBoth, Name: "child-github",
	}}

	r := &fakeNodeRunner{}
	_, err := PlanGraph(context.Background(), root, "first-root", r)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(r.inputs), 2)
	names := r.inputs[1].ResourceNames
	assert.Equal(t, "first-root-child-child-github", names["Channel/child-github"])
	assert.Equal(t, "first-root-child-child-github-creds", names["Secret/child-github-creds"])
}

func TestPlanGraphRejectsUnexpectedSecretBeforeMutation(t *testing.T) {
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent", secrets: map[string][]SecretSpec{"child": {{Name: "undeclared", Key: "token", Value: "private"}}}}
	_, err := PlanGraph(context.Background(), graphFixture(t), "", r)
	require.ErrorContains(t, err, "fixture-agent > child")
	assert.Contains(t, err.Error(), "Secret/undeclared")
	assert.Empty(t, c.Mutations())
}

func TestPlanGraphChecksEveryNodesClusterConflictsBeforeMutation(t *testing.T) {
	foreign := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent-child-helper", Namespace: "agents"}}
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(foreign).Build()}
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent"}
	_, err := PlanGraph(context.Background(), graphFixture(t), "", r)
	require.ErrorContains(t, err, "fixture-agent > child > helper")
	var conflict *ConflictError
	assert.ErrorAs(t, err, &conflict)
	assert.Empty(t, c.Mutations())
}

func TestPlanGraphRejectsResourceCollisionsAcrossNodes(t *testing.T) {
	b := graphFixture(t)
	b.Manifests = append(b.Manifests, []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture-agent-child-prompt\n")...)
	b.Dependencies[0].Bundle.Manifests = append(b.Dependencies[0].Bundle.Manifests, []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: prompt\n")...)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent"}
	_, err := PlanGraph(context.Background(), b, "", r)
	require.ErrorContains(t, err, "fixture-agent > child")
	assert.Contains(t, err.Error(), "also belongs to node fixture-agent")
	assert.Contains(t, err.Error(), "fixture-agent-child-prompt")
	assert.Empty(t, c.Mutations())
}

func TestExecuteGraphRollsBackFailingNodeThenSuccessfulNodesAndJoinsErrors(t *testing.T) {
	primary, cleanup := errors.New("apply failed"), errors.New("cleanup failed")
	r := &fakeNodeRunner{
		apply: func(path oap.DependencyPath) error {
			if path.String() == "sibling" {
				return primary
			}
			return nil
		},
		rollback: func(path oap.DependencyPath) error {
			if path.String() == "child" {
				return cleanup
			}
			return nil
		},
	}
	p, err := PlanGraph(context.Background(), graphFixture(t), "", r)
	require.NoError(t, err)
	_, err = ExecuteGraph(context.Background(), p, r)
	require.Error(t, err)
	assert.ErrorIs(t, err, primary)
	assert.ErrorIs(t, err, cleanup)
	assert.Contains(t, err.Error(), "fixture-agent > sibling")
	assert.Contains(t, err.Error(), "fixture-agent > child")
	assert.Equal(t, []string{"rollback:sibling", "rollback:child", "rollback:child > helper"}, r.operations[len(r.operations)-3:])
}

func TestExecuteGraphOwnershipProvenanceReinstallAndNoPruning(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent", opts: InstallOpts{SourceKind: "registry", SourceRef: "registry.test/agent:1", SourceDigest: "sha256:root"}}
	b := graphFixture(t)
	var first []*unstructured.Unstructured
	for i := 0; i < 2; i++ {
		p, err := PlanGraph(ctx, b, "", r)
		require.NoError(t, err)
		for _, node := range p.Nodes {
			for _, cr := range node.Prepared.CRs {
				assert.Equal(t, "fixture-agent", cr.GetLabels()[instance.LabelInstall])
				assert.Equal(t, node.Input.Path.String(), cr.GetAnnotations()[AnnotationDependencyPath])
			}
			ac := node.Prepared.CRs[0]
			source, err := instance.ParseOapSource(ac.GetAnnotations()[instance.AnnotationOapSource])
			require.NoError(t, err)
			if len(node.Input.Path) == 0 {
				assert.Equal(t, "registry.test/agent:1", source.Ref)
				assert.Equal(t, "sha256:root", source.Digest)
				roster, _, err := unstructured.NestedStringSlice(ac.Object, "spec", "subagents")
				require.NoError(t, err)
				assert.Equal(t, []string{"fixture-agent-child", "fixture-agent-sibling"}, roster)
			} else {
				assert.Equal(t, "embedded", source.SourceKind)
				assert.Equal(t, node.Input.PackedDigest, source.Digest)
			}
			if i == 0 {
				first = append(first, ac.DeepCopy())
			} else {
				assert.Equal(t, first[0], ac)
				first = first[1:]
			}
		}
		_, err = ExecuteGraph(ctx, p, r)
		require.NoError(t, err)
	}
	// A new root with no declared children converges without silently pruning
	// resources from the previous private graph.
	p, err := PlanGraph(ctx, validBundle(), "", r)
	require.NoError(t, err)
	_, err = ExecuteGraph(ctx, p, r)
	require.NoError(t, err)
	var child v1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "fixture-agent-child-helper"}, &child))
}

func TestExecuteGraphRollbackPreservesExistingAndAdoptedAndCleansPartialFailure(t *testing.T) {
	ctx := context.Background()
	foreign := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent-child", Namespace: "agents"}}
	existing := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent-child-helper", Namespace: "agents", Labels: map[string]string{instance.LabelInstall: "fixture-agent"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(foreign, existing).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == "fixture-agent" {
				return errors.New("root SSA failed")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent", opts: InstallOpts{AdoptAll: true}, secrets: map[string][]SecretSpec{"": {{Name: "widget-token", Key: "token", Value: "sensitive"}}}}
	p, err := PlanGraph(ctx, graphFixture(t), "", r)
	require.NoError(t, err)
	_, err = ExecuteGraph(ctx, p, r)
	require.ErrorContains(t, err, "root SSA failed")
	assert.Contains(t, err.Error(), "fixture-agent > child")
	assert.Contains(t, err.Error(), "retained adopted objects: [AgentClass/fixture-agent-child]")
	for _, name := range []string{"fixture-agent-child", "fixture-agent-child-helper"} {
		var ac v1alpha1.AgentClass
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: name}, &ac))
	}
	for _, name := range []string{"fixture-agent", "fixture-agent-sibling"} {
		var ac v1alpha1.AgentClass
		assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: name}, &ac)))
	}
	var secret corev1.Secret
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "widget-token"}, &secret)))
}

func TestUninstallGraphDeletesRootThenParentsThenLeaves(t *testing.T) {
	ctx := context.Background()
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	r := &clusterNodeRunner{c: c, rootName: "fixture-agent"}
	p, err := PlanGraph(ctx, graphFixture(t), "", r)
	require.NoError(t, err)
	_, err = ExecuteGraph(ctx, p, r)
	require.NoError(t, err)
	rootConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "root-config", Namespace: "agents", Labels: map[string]string{instance.LabelInstall: r.rootName}}}
	require.NoError(t, c.Create(ctx, rootConfig))
	other := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "agents", Labels: map[string]string{instance.LabelInstall: "other"}}}
	require.NoError(t, c.Create(ctx, other))
	c.mutations = nil
	n, err := UninstallGraph(ctx, c, r.rootName, "agents")
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	names := []string{}
	for _, key := range c.Mutations() {
		names = append(names, key.Name)
	}
	assert.Equal(t, []string{"fixture-agent", "root-config", "fixture-agent-child", "fixture-agent-sibling", "fixture-agent-child-helper"}, names)
	var ac v1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(other), &ac))
}
