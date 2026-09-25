//go:build integration

package workshop_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv/idempotency"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }

// reconcileUntilReady drives Reconcile against a real apiserver until the
// Workshop reaches Ready or an error surfaces. The first call always adds
// the finalizer and requeues (Reconcile's step 0) before any provisioning
// logic runs; a bounded loop rather than exactly two calls tolerates that
// without hard-coding the finalizer prologue's call count here too.
func reconcileUntilReady(t *testing.T, ctx context.Context, r *workshop.Reconciler, key types.NamespacedName) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			require.NoError(t, err, "reconcile step %d", i)
		}
		var got spiceboxv1alpha1.Workshop
		require.NoError(t, r.Client.Get(ctx, key, &got), "get Workshop after reconcile step %d", i)
		if got.Status.Phase == spiceboxv1alpha1.WorkshopPhaseReady {
			return
		}
	}
	t.Fatalf("Workshop %s did not reach Ready within 5 reconciles", key)
}

// restrictedPodNoResources builds a pod that satisfies the workshop
// namespace's enforced "restricted" Pod Security profile and declares NO
// resources at all — the shape of every pod a person's own test run creates,
// and the shape the workshop ResourceQuota refuses unless a LimitRange has
// defaulted the four values first.
func restrictedPodNoResources(ns, name string) *corev1.Pod {
	yes, no := true, false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "runner",
				Image: "demo/runner:dev",
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func TestProvision_RealAPIServerStateAndIdempotency(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	// builderSession's uid arg is a placeholder: a real apiserver assigns its
	// own UID on Create and overwrites whatever a client sends, so sess.UID
	// (and everything WorkshopNamespaceName derives from it) is only valid
	// AFTER Create returns. This reconciler never reads Spec.Class/Prompt, but
	// the CRD's structural schema requires them.
	sess := builderSession("wsbuild-1", "placeholder-overwritten-by-apiserver")
	sess.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "workshop-test-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	reg := tokens.NewRegistry()
	r := &workshop.Reconciler{
		Client:                         env.Client,
		APIReader:                      env.Client,
		Tuples:                         ft,
		Tokens:                         reg,
		BrowserServiceAccount:          "spicebox-webd",
		BrowserServiceAccountNamespace: "agentprimitives-system",
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	nsName := got.Status.Namespace
	require.NotEmpty(t, nsName, "status.namespace must be set once Ready")
	saName := spiceboxv1alpha1.WorkshopServiceAccountName(sess.Name)

	// Namespace: real apiserver, both labels + restricted Pod Security.
	var ns corev1.Namespace
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: nsName}, &ns))
	assert.Equal(t, sess.Namespace, ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace])
	assert.Equal(t, sess.Name, ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName])
	assert.Equal(t, "restricted", ns.Labels["pod-security.kubernetes.io/enforce"])

	// Deny-all NetworkPolicy.
	var np networkingv1.NetworkPolicy
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-default-deny"}, &np))
	assert.Empty(t, np.Spec.PodSelector.MatchLabels)
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	assert.Empty(t, np.Spec.Ingress)
	assert.Empty(t, np.Spec.Egress)

	// ResourceQuota.
	var rq corev1.ResourceQuota
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-quota"}, &rq))

	// LimitRange, with the container defaults that make the quota satisfiable.
	var lr corev1.LimitRange
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-limits"}, &lr))
	require.Len(t, lr.Spec.Limits, 1)
	item := lr.Spec.Limits[0]
	assert.Equal(t, corev1.LimitTypeContainer, item.Type)
	assert.True(t, resource.MustParse("250m").Equal(item.DefaultRequest[corev1.ResourceCPU]))
	assert.True(t, resource.MustParse("512Mi").Equal(item.DefaultRequest[corev1.ResourceMemory]))
	assert.True(t, resource.MustParse("1").Equal(item.Default[corev1.ResourceCPU]))
	assert.True(t, resource.MustParse("2Gi").Equal(item.Default[corev1.ResourceMemory]))

	// The whole point, proved rather than assumed: a pod that names NO
	// resources is admitted into the provisioned namespace WITH all four values
	// filled in by admission. That is what makes it acceptable to the quota,
	// which demands requests and limits on every container and is why a person's
	// own test run was refused with "must specify limits.cpu ...". The security
	// fields are set because the namespace enforces the restricted Pod Security
	// profile; resources are the one thing deliberately left out.
	//
	// The assertions below are on the DEFAULTED values, not on Create
	// succeeding: this apiserver runs no controller manager, so no quota status
	// is ever computed and the quota admission plugin admits everything here
	// regardless. Only the defaulting half is observable in envtest, and it is
	// the half this test owns — dropping the LimitRange apply makes exactly
	// these two assertions fail.
	require.NoError(t, env.Client.Create(ctx, restrictedPodNoResources(nsName, "bare-runner")),
		"a pod naming no resources must be admitted: the LimitRange defaults what the quota requires")
	var admitted corev1.Pod
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "bare-runner"}, &admitted))
	defaulted := admitted.Spec.Containers[0].Resources
	assert.True(t, resource.MustParse("250m").Equal(defaulted.Requests[corev1.ResourceCPU]),
		"the LimitRange must have defaulted requests.cpu, got %v", defaulted.Requests[corev1.ResourceCPU])
	assert.True(t, resource.MustParse("2Gi").Equal(defaulted.Limits[corev1.ResourceMemory]),
		"the LimitRange must have defaulted limits.memory, got %v", defaulted.Limits[corev1.ResourceMemory])

	// ServiceAccount.
	var sa corev1.ServiceAccount
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: saName}, &sa))

	// Role: exactly the eight CRUD kinds, the agentsessions rule (get/list/
	// watch/delete, for test_sessions and stop_test), and the narrower
	// workshopprobes rule, across three rules.
	var role rbacv1.Role
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-agent"}, &role))
	wantRules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"agentclasses", "mcpservers", "sidecartoolboxes", "agentidentities", "skills", "agentuis", "subagentrequests"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"agentsessions"},
			Verbs:     []string{"get", "list", "watch", "delete"},
		},
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"workshopprobes"},
			Verbs:     []string{"create", "get", "list", "watch"},
		},
	}
	assert.ElementsMatch(t, wantRules, role.Rules)

	// Bindings + toolwriter/reader CRBs.
	var wsBinding rbacv1.RoleBinding
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-agent"}, &wsBinding))
	var crBinding rbacv1.RoleBinding
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: saName + "-cr"}, &crBinding))
	var toolCRB rbacv1.ClusterRoleBinding
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopToolwriterCRBName("default", sess.Name)}, &toolCRB))
	var readerCRB rbacv1.ClusterRoleBinding
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopReaderCRBName("default", sess.Name)}, &readerCRB))

	// Browser start Role + RoleBinding: BrowserServiceAccount(Namespace) is
	// wired above, so BuildWorkshopBrowserRBAC's objects must exist in the
	// workshop namespace against a real apiserver, same as every other RBAC
	// object provisioning stands up. Mirrors the sidecar Role/RoleBinding
	// assertions above: a bare Get proves the name only, not the grant.
	var browserRole rbacv1.Role
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workshop.BrowserRoleName}, &browserRole))
	var browserBinding rbacv1.RoleBinding
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workshop.BrowserRoleName}, &browserBinding))
	wantBrowserRole, _ := workshop.BuildWorkshopBrowserRBAC(&got, sess, r.BrowserServiceAccount, r.BrowserServiceAccountNamespace)
	assert.ElementsMatch(t, wantBrowserRole.Rules, browserRole.Rules)
	assert.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Name: r.BrowserServiceAccount, Namespace: r.BrowserServiceAccountNamespace}}, browserBinding.Subjects)
	assert.Equal(t, rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: workshop.BrowserRoleName}, browserBinding.RoleRef)

	// SSA idempotency: a byte-identical re-apply of every object this
	// controller SSA's must be a true no-op against a REAL apiserver — the
	// fake client cannot prove this (see CLAUDE.md "Server-side apply").
	idempotency.RequireApplyIdempotent(t, ctx, env.Client, workshop.BuildWorkshopDenyAll(nsName), workshop.FieldOwner)
	idempotency.RequireApplyIdempotent(t, ctx, env.Client, workshop.BuildWorkshopQuota(nsName), workshop.FieldOwner)
	idempotency.RequireApplyIdempotent(t, ctx, env.Client, workshop.BuildWorkshopLimitRange(nsName), workshop.FieldOwner)
	saObj, roleObj, wsBindingObj, crRoleObj, crBindingObj, _, _ := workshop.BuildWorkshopRBAC(&got, sess)
	for _, obj := range []client.Object{saObj, roleObj, wsBindingObj, crRoleObj, crBindingObj} {
		idempotency.RequireApplyIdempotent(t, ctx, env.Client, obj, workshop.FieldOwner)
	}

	// Reconcile convergence: once Ready, further reconciles must stop
	// rewriting the Workshop object.
	idempotency.RequireReconcileConverges(t, ctx, env.Client, r, ctrl.Request{NamespacedName: key}, &spiceboxv1alpha1.Workshop{}, 5)
}

// TestProvision_NamespaceCollisionRefusedBeforeAnchor is the C1 refusing-
// direction test against a REAL apiserver: when the derived ws-<uid12>
// namespace already exists LABELED FOR A DIFFERENT SESSION, provisioning must
// (a) fail the NamespaceReady condition and (b) NEVER anchor status.Namespace.
// The empty-anchor assertion is load-bearing: teardown deletes status.Namespace
// by name, so a collision that anchored the foreign name would let teardown
// destroy a namespace this workshop does not own.
func TestProvision_NamespaceCollisionRefusedBeforeAnchor(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := builderSession("wsbuild-collision", "placeholder-overwritten-by-apiserver")
	sess.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "workshop-test-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	// The namespace the derived name will land on, but labeled for a DIFFERENT
	// session — a name collision, not this workshop's own namespace.
	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	collide := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelWorkshopSessionNamespace: "some-other-namespace",
				spiceboxv1alpha1.LabelWorkshopSessionName:      "some-other-session",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, collide), "create the colliding foreign-labeled namespace")

	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    &fakeTuples{},
		Tokens:    tokens.NewRegistry(),
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}

	// The finalizer prologue requeues; the next reconcile reaches provisioning
	// and must refuse the collision.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "the finalizer-adding reconcile must not error")
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.Error(t, err, "provisioning must refuse a namespace collision")
	assert.Contains(t, err.Error(), nsName, "the error should name the colliding namespace")

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	assert.True(t, hasConditionFalse(got.Status.Conditions, spiceboxv1alpha1.WorkshopConditionNamespaceReady),
		"NamespaceReady must be False on a collision")
	assert.Empty(t, got.Status.Namespace,
		"a collision must NEVER anchor status.Namespace — otherwise teardown would delete a namespace this workshop does not own")

	// The foreign namespace must be untouched: never adopted, never relabeled.
	var stillThere corev1.Namespace
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: nsName}, &stillThere))
	assert.Equal(t, "some-other-namespace", stillThere.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace])
	assert.Equal(t, "some-other-session", stillThere.Labels[spiceboxv1alpha1.LabelWorkshopSessionName])
}

// hasConditionFalse reports whether conds carries condType with status False.
func hasConditionFalse(conds []metav1.Condition, condType string) bool {
	for _, c := range conds {
		if c.Type == condType {
			return c.Status == metav1.ConditionFalse
		}
	}
	return false
}
