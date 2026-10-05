//go:build integration

package install_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// TestUninstall_DeletesInstallLabeledCRs_LeavesUnrelatedInstallUntouched
// installs the demo-agent fixture (AgentClass "demo-class" + Secret "demo-pat",
// both labeled agentprimitives.authzed.com/oap-install=demo-class), plants an
// UNRELATED AgentClass in the SAME namespace carrying a DIFFERENT install label
// (a second, independent `oap agent install` sharing the namespace), then
// uninstalls only "demo-class" and asserts: every demo-class-labeled resource is
// gone, the unrelated AgentClass is untouched, and the reported count matches
// exactly the two resources demo-class's install created.
func TestUninstall_DeletesInstallLabeledCRs_LeavesUnrelatedInstallUntouched(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-uninstall-basic"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	b := packedDemoAgent(t)
	answers, secrets := resolveDemoAgent(t, b, "fake-token-value")
	_, err := install.Install(ctx, env.Client, b, answers, secrets, install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "install demo-agent")

	// An unrelated CR in the SAME namespace, under a DIFFERENT install name —
	// must survive uninstalling "demo-class" untouched.
	other := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-agent",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/instance":              "other-install",
				"agentprimitives.authzed.com/oap-install": "other-install",
			},
		},
		Spec: v1alpha1.AgentClassSpec{
			SystemPrompt: v1alpha1.PromptSource{Inline: "unrelated agent, do not touch"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, other), "create unrelated AgentClass under a different install name")

	deleted, err := install.Uninstall(ctx, env.Client, "demo-class", ns)
	require.NoError(t, err, "Uninstall must succeed")
	assert.Equal(t, 2, deleted, "must delete exactly the AgentClass + Secret demo-class's install created")

	var ac v1alpha1.AgentClass
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-class"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "demo-class AgentClass must be gone")

	var sec corev1.Secret
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-pat"}, &sec)
	assert.True(t, apierrors.IsNotFound(err), "demo-pat Secret must be gone")

	var untouched v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "other-agent"}, &untouched), "unrelated AgentClass under a different install name must survive")
	assert.Equal(t, "other-install", untouched.Labels["agentprimitives.authzed.com/oap-install"])
}

// TestUninstall_NoMatches_ReturnsZeroNoError covers the no-op case: an
// install name with nothing on the cluster bearing its label deletes nothing
// and is not an error (uninstall must be safe to re-run / run against an
// already-clean name).
func TestUninstall_NoMatches_ReturnsZeroNoError(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-uninstall-empty"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	deleted, err := install.Uninstall(ctx, env.Client, "never-installed", ns)
	require.NoError(t, err, "Uninstall of a name with nothing on the cluster must not error")
	assert.Equal(t, 0, deleted)
}

type integrationGraphRunner struct {
	c         client.Client
	namespace string
}

func (r *integrationGraphRunner) PrepareNode(ctx context.Context, in install.NodeInput) (*install.Prepared, error) {
	var secrets []install.SecretSpec
	if in.Path.String() == "child" {
		secrets = []install.SecretSpec{{Name: "credential", Key: "user", Value: "fixture-user"}, {Name: "credential", Key: "token", Value: "fixture-token"}}
	}
	return install.Prepare(ctx, r.c, in.Bundle, nil, secrets, install.InstallOpts{
		Namespace: r.namespace, RootInstallName: "graph-root", DependencyPath: in.Path,
		ResourceNames: in.ResourceNames, DirectNames: in.DirectNames,
		SourceKind: in.SourceKind, SourceRef: in.SourceRef, SourceDigest: in.PackedDigest,
	})
}
func (r *integrationGraphRunner) ApplyNode(ctx context.Context, p *install.Prepared) (*install.Result, error) {
	return install.ApplyPrepared(ctx, r.c, p)
}
func (r *integrationGraphRunner) RollbackNode(ctx context.Context, p *install.Prepared) error {
	return install.RollbackPrepared(ctx, r.c, p)
}

type graphDeleteClient struct {
	client.Client
	names []string
}

func (c *graphDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.names = append(c.names, obj.GetName())
	return c.Client.Delete(ctx, obj, opts...)
}

type patchHookClient struct {
	client.Client
	beforePatch func(context.Context, client.Object) error
}

func (c *patchHookClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		beforePatch := c.beforePatch
		c.beforePatch = nil
		if err := beforePatch(ctx, obj); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type deleteHookClient struct {
	client.Client
	beforeDelete func(context.Context, client.Object) error
}

func (c *deleteHookClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.beforeDelete != nil {
		beforeDelete := c.beforeDelete
		c.beforeDelete = nil
		if err := beforeDelete(ctx, obj); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestUninstallGraphAfterIdempotentGraphInstall(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-install-private-graph"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	leaf := bundleFromCRs(t, minimalAgentClass("helper"))
	leaf.Manifest.Agent.Name = "helper"
	childCR := minimalAgentClass("child")
	childCR["spec"].(map[string]any)["subagents"] = []any{"helper"}
	child := bundleFromCRs(t, childCR)
	child.Manifest.Agent.Name = "child"
	child.Manifest.Requires.Secrets = []oap.RequiredSecret{{Name: "credential", Keys: []string{"user", "token"}}}
	childDep := oap.RequiredAgent{Path: "dependencies/helper"}
	child.Manifest.Requires.Agents = []oap.RequiredAgent{childDep}
	child.Dependencies = []*oap.Dependency{{Descriptor: childDep, Bundle: leaf}}
	rootCR := minimalAgentClass("graph-root")
	rootCR["spec"].(map[string]any)["subagents"] = []any{"child"}
	root := bundleFromCRs(t, rootCR)
	root.Manifest.Agent.Name = "graph-root"
	rootDep := oap.RequiredAgent{Path: "dependencies/child"}
	root.Manifest.Requires.Agents = []oap.RequiredAgent{rootDep}
	root.Dependencies = []*oap.Dependency{{Descriptor: rootDep, Bundle: child}}
	runner := &integrationGraphRunner{c: env.Client, namespace: ns}
	versions := map[string]string{}
	for i := 0; i < 2; i++ {
		plan, err := install.PlanGraph(ctx, root, "", runner)
		require.NoError(t, err)
		_, err = install.ExecuteGraph(ctx, plan, runner)
		require.NoError(t, err)
		for _, node := range plan.Nodes {
			var ac v1alpha1.AgentClass
			require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: node.Input.PhysicalName}, &ac))
			assert.Equal(t, "graph-root", ac.Labels[instance.LabelInstall])
			assert.Equal(t, node.Input.Path.String(), ac.Annotations[install.AnnotationDependencyPath])
			if i == 0 {
				versions[ac.Name] = ac.ResourceVersion
			} else {
				assert.Equal(t, versions[ac.Name], ac.ResourceVersion, "byte-identical graph reinstall must be SSA-idempotent")
			}
		}
	}
	var secret corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "graph-root-child-credential"}, &secret))
	assert.Equal(t, map[string][]byte{"user": []byte("fixture-user"), "token": []byte("fixture-token")}, secret.Data)
	c := &graphDeleteClient{Client: env.Client}
	deleted, err := install.UninstallGraph(ctx, c, "graph-root", ns)
	require.NoError(t, err)
	assert.Equal(t, 4, deleted)
	assert.Equal(t, []string{"graph-root", "graph-root-child", "graph-root-child-credential", "graph-root-child-helper"}, c.names)
}

func TestRollbackPreparedPreservesConcurrentReplacement(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-rollback-replaced"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	b := bundleFromCRs(t, minimalAgentClass("rollback-agent"))
	p, err := install.Prepare(ctx, env.Client, b, nil, []install.SecretSpec{{Name: "credential", Key: "token", Value: "fixture-token"}}, install.InstallOpts{Namespace: ns})
	require.NoError(t, err)
	_, err = install.ApplyPrepared(ctx, env.Client, p)
	require.NoError(t, err)
	var original v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "rollback-agent"}, &original))
	require.NoError(t, env.Client.Delete(ctx, &original))
	replacement := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "rollback-agent", Namespace: ns}, Spec: v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "foreign replacement"}}}
	require.NoError(t, env.Client.Create(ctx, replacement))
	require.NotEqual(t, original.UID, replacement.UID)
	err = install.RollbackPrepared(ctx, env.Client, p)
	require.Error(t, err, "UID precondition must refuse to delete the replacement")
	assert.True(t, apierrors.IsConflict(err))
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(replacement), &got))
	assert.Equal(t, replacement.UID, got.UID)
	var secret corev1.Secret
	assert.True(t, apierrors.IsNotFound(env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "credential"}, &secret)), "cleanup continues after a protected replacement")
}

func TestApplyPreparedRefusesDisappearedApprovedObject(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-apply-disappeared"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	existing := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns, Labels: map[string]string{instance.LabelInstall: "stable-agent"}},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "previous desired state"}},
	}
	require.NoError(t, env.Client.Create(ctx, existing))
	p, err := install.Prepare(ctx, env.Client, bundleFromCRs(t, minimalAgentClass("stable-agent")), nil, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err)
	require.NoError(t, env.Client.Delete(ctx, existing))

	_, err = install.ApplyPrepared(ctx, env.Client, p)
	require.Error(t, err, "SSA must remain conditional on the object instance approved by Prepare")
	var got v1alpha1.AgentClass
	assert.True(t, apierrors.IsNotFound(env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "stable-agent"}, &got)), "apply must not recreate a disappeared approved object")
}

func TestApplyPreparedRefusesReplacementAfterPrepare(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-apply-replaced"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	existing := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns, Labels: map[string]string{instance.LabelInstall: "stable-agent"}},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "previous desired state"}},
	}
	require.NoError(t, env.Client.Create(ctx, existing))
	p, err := install.Prepare(ctx, env.Client, bundleFromCRs(t, minimalAgentClass("stable-agent")), nil, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err)
	require.NoError(t, env.Client.Delete(ctx, existing))
	replacement := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "foreign replacement"}},
	}
	require.NoError(t, env.Client.Create(ctx, replacement))

	_, err = install.ApplyPrepared(ctx, env.Client, p)
	require.Error(t, err, "SSA must refuse a same-name object with a different UID")
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(replacement), &got))
	assert.Equal(t, replacement.UID, got.UID)
	assert.Equal(t, "foreign replacement", got.Spec.SystemPrompt.Inline)
}

// TestApplyPreparedRetriesSameUIDUpdateAfterPrepare is the real-apiserver analog
// of TestInstall_ControllerWriteBetweenCreateAndApply_RetriesOnCurrentVersion: an
// intervening write that keeps the SAME uid — the object's own controller adding
// a status/finalizer, or any external actor touching a field the install does not
// manage — is a benign race, not a seizure. The conditional apply must re-read,
// confirm the uid it approved, and retry on the current resourceVersion so the
// install's desired state still lands, rather than aborting the whole install on
// a bare resourceVersion conflict.
//
// Only a uid CHANGE (a same-name replacement) or the object's disappearance still
// fails closed — those remain TestApplyPreparedRefusesReplacementAfterPrepare and
// TestApplyPreparedRefusesDisappearedApprovedObject.
func TestApplyPreparedRetriesSameUIDUpdateAfterPrepare(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-apply-updated"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	existing := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns, Labels: map[string]string{instance.LabelInstall: "stable-agent"}},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "previous desired state"}},
	}
	require.NoError(t, env.Client.Create(ctx, existing))
	p, err := install.Prepare(ctx, env.Client, bundleFromCRs(t, minimalAgentClass("stable-agent")), nil, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err)

	// A write that lands after Prepare but keeps the same uid: the exact shape of
	// the object's own controller adding a finalizer/status in the apply window.
	var updated v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(existing), &updated))
	updated.Labels["external.example/change"] = "after-prepare"
	require.NoError(t, env.Client.Update(ctx, &updated))

	_, err = install.ApplyPrepared(ctx, env.Client, p)
	require.NoError(t, err, "a same-uid intervening update is a benign race: the apply must re-read and retry, not abort")
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(existing), &got))
	assert.Equal(t, updated.UID, got.UID, "the retry must stay on the object this run approved, not replace it")
	assert.Equal(t, "fake test agent", got.Spec.SystemPrompt.Inline, "the install's desired spec must land on the retry")
	assert.Equal(t, "after-prepare", got.Labels["external.example/change"], "a field owned by another manager must survive the force-ownership apply")
}

func TestApplyPreparedRefusesReplacementBetweenCreateAndSSA(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-apply-create-replaced"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	replacement := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "new-agent", Namespace: ns},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "foreign replacement"}},
	}
	c := &patchHookClient{Client: env.Client}
	c.beforePatch = func(ctx context.Context, obj client.Object) error {
		var created v1alpha1.AgentClass
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(obj), &created); err != nil {
			return err
		}
		if err := env.Client.Delete(ctx, &created); err != nil {
			return err
		}
		return env.Client.Create(ctx, replacement)
	}
	p, err := install.Prepare(ctx, c, bundleFromCRs(t, minimalAgentClass("new-agent")), nil, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err)

	_, err = install.ApplyPrepared(ctx, c, p)
	require.Error(t, err, "SSA must bind to the UID and resourceVersion returned by Create")
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(replacement), &got))
	assert.Equal(t, replacement.UID, got.UID)
	assert.Equal(t, "foreign replacement", got.Spec.SystemPrompt.Inline)
}

func TestUninstallGraphPreservesReplacementAfterList(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-uninstall-replaced"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	original := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns, Labels: map[string]string{instance.LabelInstall: "stable-agent"}}}
	require.NoError(t, env.Client.Create(ctx, original))
	replacement := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns},
		Spec:       v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "foreign replacement"}},
	}
	c := &deleteHookClient{Client: env.Client}
	c.beforeDelete = func(ctx context.Context, _ client.Object) error {
		var live v1alpha1.AgentClass
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(original), &live); err != nil {
			return err
		}
		if err := env.Client.Delete(ctx, &live); err != nil {
			return err
		}
		return env.Client.Create(ctx, replacement)
	}

	deleted, err := install.UninstallGraph(ctx, c, "stable-agent", ns)
	assert.Zero(t, deleted)
	require.Error(t, err, "delete must remain conditional on the object instance returned by List")
	assert.True(t, apierrors.IsConflict(err))
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(replacement), &got))
	assert.Equal(t, replacement.UID, got.UID)
	assert.Equal(t, "foreign replacement", got.Spec.SystemPrompt.Inline)
}

func TestUninstallGraphPreservesObjectChangedAfterList(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const ns = "oap-uninstall-updated"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	original := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "stable-agent", Namespace: ns, Labels: map[string]string{instance.LabelInstall: "stable-agent"}}}
	require.NoError(t, env.Client.Create(ctx, original))
	c := &deleteHookClient{Client: env.Client}
	c.beforeDelete = func(ctx context.Context, _ client.Object) error {
		var live v1alpha1.AgentClass
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(original), &live); err != nil {
			return err
		}
		live.Labels[instance.LabelInstall] = "different-install"
		return env.Client.Update(ctx, &live)
	}

	deleted, err := install.UninstallGraph(ctx, c, "stable-agent", ns)
	assert.Zero(t, deleted)
	require.Error(t, err, "delete must remain conditional on the resourceVersion returned by List")
	assert.True(t, apierrors.IsConflict(err))
	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(original), &got))
	assert.Equal(t, "different-install", got.Labels[instance.LabelInstall])
}
