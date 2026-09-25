// Package workshop hosts the Workshop reconciler: layer 1.2 (the closed
// RBAC kind set — the runner gets nothing, only <session>-workshop-sa gets
// the workshop namespace's build kinds) and layer 1.3 (the SpiceDB tuple
// that binds the workshop to the ONE session allowed to act as it). This
// file (rbac.go) is the pure, side-effect-free half: every Build* function
// takes inputs and returns Kubernetes objects, never touches the API
// server. controller.go applies what these build.
package workshop

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
)

// workshopNamespaceRoleName names the Role + RoleBinding pair, in the
// provisioned workshop namespace, that grants the closed kind set. One
// workshop namespace ever holds one workshop's RBAC, so a fixed name is
// safe — re-provisioning re-applies the identical object (SSA no-op).
const workshopNamespaceRoleName = "workshop-agent"

// spiceboxWorkshopToolwriterClusterRole is the static, cluster-scoped
// ClusterRole (config/manager/workshop-toolwriter.yaml) every workshop's
// per-session toolwriter ClusterRoleBinding binds. RBAC cannot scope
// `create` by name, which is exactly why this grant is narrow (only
// spiceboxtoolspecs/spiceboxtoolkits) and why the webhook (Task 9) — not
// this ClusterRole — is what stops a workshop sidecar from naming another
// workshop's tool CRs.
const spiceboxWorkshopToolwriterClusterRole = "spicebox-workshop-toolwriter"

// spiceboxWorkshopReaderClusterRole is the static, cluster-scoped ClusterRole
// (config/manager/workshop-reader.yaml) every workshop's per-session reader
// ClusterRoleBinding binds — get/list on the three cluster-scoped kinds the
// `inventory` tool reads (ClusterAgentSettings, ClusterSkill, SpiceboxClass).
// Read-only: unlike toolwriter, nothing here needs the admission webhook's
// narrowing, since get/list on a descriptive, non-secret kind is not a
// write another workshop's identity could be spoofed into.
const spiceboxWorkshopReaderClusterRole = "spicebox-workshop-reader"

// workshopKindResources is the CLOSED set of namespaced resources the
// workshop SA may manage inside the workshop namespace. Everything else —
// secrets, pods, configmaps, RBAC — is deliberately absent: the sidecar can
// declare agents and tools, and can never read a credential, run an image, or
// widen its own access. Growing this list is a security review (spec §1.2).
//
// WorkshopProbe is NOT in this set — it gets its own, narrower rule
// (create;get;list;watch, no update/patch/delete) in BuildWorkshopRBAC
// below, because unlike everything here it is fire-once, spec-immutable, and
// controller-owned on status.
var workshopKindResources = []string{
	"agentclasses", "mcpservers", "sidecartoolboxes", "agentidentities",
	"skills", "agentuis", "subagentrequests",
}

// BuildWorkshopNamespace is the workshop's isolated namespace: labeled with
// both session-attribution labels (the collision guard in controller.go
// reads these back to prove an EXISTING namespace of this derived name
// really is this workshop's, never someone else's) and Pod Security
// "restricted" — every workshop pod, present or future, runs under the
// tightest built-in profile.
func BuildWorkshopNamespace(ws *spiceboxv1alpha1.Workshop, sess *spiceboxv1alpha1.AgentSession) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name: spiceboxv1alpha1.WorkshopNamespaceName(sess.UID),
			Labels: map[string]string{
				spiceboxv1alpha1.LabelWorkshopSessionNamespace: sess.Namespace,
				spiceboxv1alpha1.LabelWorkshopSessionName:      sess.Name,
				"pod-security.kubernetes.io/enforce":           "restricted",
			},
		},
	}
}

// BuildWorkshopDenyAll is the workshop namespace's baseline: every pod
// created in it (today none; a later plan may run build-time probes) starts
// with zero ingress and zero egress until something explicitly widens it.
// Empty pod selector = every pod in the namespace; both PolicyTypes with no
// rules = deny-all, the same shape as every other deny-all NetworkPolicy in
// this repo (see pkg/controllers/agentsession/netpol.go).
func BuildWorkshopDenyAll(nsName string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workshop-default-deny",
			Namespace: nsName,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// BuildWorkshopQuota bounds the workshop namespace's compute footprint — the
// COMPUTE story only. Per-kind object counts (spec.limits.maxObjects /
// maxObjectsPerKind) are enforced by the admission webhook (Task 9), because
// a ResourceQuota cannot count "AgentClasses of this workshop" — only
// built-in resource types.
func BuildWorkshopQuota(nsName string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workshop-quota",
			Namespace: nsName,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourcePods:           resource.MustParse("10"),
				corev1.ResourceRequestsCPU:    resource.MustParse("4"),
				corev1.ResourceRequestsMemory: resource.MustParse("8Gi"),
				corev1.ResourceLimitsCPU:      resource.MustParse("8"),
				corev1.ResourceLimitsMemory:   resource.MustParse("16Gi"),
			},
		},
	}
}

// BuildWorkshopLimitRange is the other half of BuildWorkshopQuota, and the two
// are only correct together. A ResourceQuota that bounds requests AND limits
// makes both MANDATORY on every container in the namespace: a pod that names
// none is refused outright ("must specify limits.cpu ... for: runner"), and the
// refusal reaches whoever created the pod, not the person waiting on it. Every
// pod that lands here is created by something other than the person — the
// session reconciler's runner, a probe — so nothing in that path has a place to
// put the four values. This LimitRange supplies them, which is what makes an
// unannotated pod admissible at all.
//
// The defaults are sized so several containers fit inside the quota's own
// ceilings (asserted in rbac_limitrange_test.go) — one test run must not
// exhaust the namespace.
//
// A pure function of nsName, like every other object provisioning SSA-applies,
// so a byte-identical re-apply is a no-op.
func BuildWorkshopLimitRange(nsName string) *corev1.LimitRange {
	return &corev1.LimitRange{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workshop-limits",
			Namespace: nsName,
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				// Container, not Pod: the quota's requests./limits.* ceilings are
				// summed per container, and only a Container item defaults them.
				Type: corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			}},
		},
	}
}

// WorkshopToolwriterCRBName is the deterministic name of the per-workshop
// ClusterRoleBinding to spicebox-workshop-toolwriter. Cluster-scoped objects
// cannot carry a namespaced owner ref, so teardown (teardown.go) deletes it
// by this exact name — the same shape as the runner's toolspec-reader CRB
// (pkg/controllers/agentsession/rbac.go).
func WorkshopToolwriterCRBName(ns, session string) string {
	return "workshop-toolwriter-" + ns + "-" + session
}

// WorkshopReaderCRBName is the deterministic name of the per-workshop
// ClusterRoleBinding to spicebox-workshop-reader — same shape as
// WorkshopToolwriterCRBName, and reaped by teardown.go the same way.
func WorkshopReaderCRBName(ns, session string) string {
	return "workshop-reader-" + ns + "-" + session
}

// BrowserRoleName names the Role and RoleBinding in the workshop namespace
// that let the browser (webd) create a person's own session there — the
// same three writes config/webd/role-builder.yaml grants in a static start
// namespace: Channel, creds Secret, AgentSession. Deterministic so the
// RoleBinding can be reasoned about by name; both live in the workshop
// namespace and go with it at teardown.
const BrowserRoleName = "workshop-browser-start"

// BuildWorkshopBrowserRBAC returns the Role and RoleBinding that let
// browserSA (in browserSANamespace) start sessions in the workshop namespace.
// The rules mirror config/webd/role-builder.yaml exactly — widen one and the
// other, never just this one. Pure function of its inputs (SSA-applied).
func BuildWorkshopBrowserRBAC(ws *spiceboxv1alpha1.Workshop, sess *spiceboxv1alpha1.AgentSession, browserSA, browserSANamespace string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: BrowserRoleName, Namespace: nsName},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"agentprimitives.authzed.com"}, Resources: []string{"channels"}, Verbs: []string{"get", "list", "create", "delete"}},
			{APIGroups: []string{"agentprimitives.authzed.com"}, Resources: []string{"agentsessions"}, Verbs: []string{"get", "list", "create", "delete", "patch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "create", "delete"}},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: BrowserRoleName, Namespace: nsName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: BrowserRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: browserSA, Namespace: browserSANamespace}},
	}
	return role, binding
}

// BuildWorkshopRBAC produces every RBAC object the workshop sidecar identity
// needs, and nothing else — the concrete shape of lockdown layer 1.2.
//
// Two Role+RoleBinding pairs, in two different namespaces, because RBAC
// rules only ever apply within the Role's own namespace:
//
//   - role/wsBinding live in the WORKSHOP namespace (nsName) and grant the
//     closed kind set (workshopKindResources) plus a namespace-scoped read
//     of this workshop's own child AgentSessions. wsBinding's subject is the
//     SA in the SESSION's namespace — a RoleBinding's subjects may name a
//     ServiceAccount in a different namespace than the RoleBinding itself.
//   - crRole/crBinding live in the SESSION's namespace (ws.Namespace, same
//     namespace as the Workshop CR and the SA) and grant get/update/patch on
//     exactly this one Workshop object (resourceNames: [ws.Name]) — the
//     later-plan request-field channel (export/install/capability requests),
//     and nothing else the sidecar could touch on any other Workshop.
//
// Neither Role/RoleBinding pair carries an owner reference: role/wsBinding
// live in nsName, a namespace neither the Workshop nor the AgentSession (both
// in ws.Namespace) can own across namespaces — Kubernetes GC ignores a
// cross-namespace owner ref — so they die when the workshop namespace itself
// is deleted (teardown.go). crRole/crBinding and the ServiceAccount carry the
// SESSION's owner ref (cosidecar.OwnerRef), the same convention
// BuildRunnerRBAC uses for its own namespaced RBAC objects and the bearer
// Secret: they persist until the session itself is deleted, but teardown
// neuters everything they could still reach — the workshop namespace, the
// tuple and the toolwriter CRB are gone, and the Workshop object the
// crRole/crBinding pin by resourceName no longer exists — so a leftover
// ServiceAccount/Role/RoleBinding is dormant, not standing.
//
// toolCRB (cluster-scoped, no owner ref possible) binds the static
// spicebox-workshop-toolwriter ClusterRole so the sidecar can author
// SpiceboxToolspec/SpiceboxToolkit CRs; RBAC cannot scope `create` by name,
// which is why the webhook (Task 9), not this grant, is what stops a
// workshop from naming another workshop's tool CR.
//
// readerCRB (also cluster-scoped, also no owner ref possible) binds the
// static spicebox-workshop-reader ClusterRole so the sidecar's `inventory`
// tool can read the three cluster-scoped, non-secret kinds it reports from
// (ClusterAgentSettings, ClusterSkill, SpiceboxClass) — get/list only, no
// webhook narrowing needed since none of it is a write.
//
// NOTE ON SHAPE: the task brief's code sketch names five return values
// (sa, role, wsBinding, crBinding, toolCRB) but its own prose calls
// crBinding "a Role+Binding PAIR". A Role's rules apply only within its own
// namespace, and the workshops-CR grant must live in ws.Namespace while the
// kind-CRUD grant must live in nsName — two different namespaces, so the
// pair cannot be one object. This function returns the Role that pairs with
// crBinding as its own value (crRole) rather than silently dropping it.
func BuildWorkshopRBAC(ws *spiceboxv1alpha1.Workshop, sess *spiceboxv1alpha1.AgentSession) (
	sa *corev1.ServiceAccount,
	role *rbacv1.Role,
	wsBinding *rbacv1.RoleBinding,
	crRole *rbacv1.Role,
	crBinding *rbacv1.RoleBinding,
	toolCRB *rbacv1.ClusterRoleBinding,
	readerCRB *rbacv1.ClusterRoleBinding,
) {
	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	saName := spiceboxv1alpha1.WorkshopServiceAccountName(sess.Name)
	owner := cosidecar.OwnerRef(sess)

	// ServiceAccount lives in the SESSION's namespace (same as the Workshop
	// CR), not the workshop namespace — RoleBinding subjects may cross
	// namespaces, so wsBinding below still reaches it from nsName.
	// AutomountServiceAccountToken stays unset: the pod builder (Task 5)
	// controls token exposure via a projected volume, not the SA's default
	// automount.
	sa = &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            saName,
			Namespace:       ws.Namespace,
			OwnerReferences: owner,
		},
	}

	role = &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workshopNamespaceRoleName,
			Namespace: nsName,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: workshopKindResources,
				Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
			},
			{
				// Its own children only by RBAC scope — this Role lives in the
				// workshop namespace, so this rule can never reach an AgentSession
				// outside it. No resourceNames: sub-agent sessions the builder
				// creates, and the person's own root test sessions (test_link/
				// watch_test, pkg/tools/workshopmcp/tools_trytest.go), are not
				// known at RBAC-build time. list is for test_sessions (finding the
				// person's test session by name); delete is for stop_test (ending
				// it) — both still bounded to this workshop's own namespace by the
				// Role's scope, never another workshop's sessions.
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"agentsessions"},
				Verbs:     []string{"get", "list", "watch", "delete"},
			},
			{
				// WorkshopProbe (plan-3a: pkg/controllers/workshopprobe) is
				// deliberately its OWN rule, not folded into workshopKindResources
				// above: that set is CRUD (the builder authors and edits those
				// kinds directly), but a probe is fire-once and its spec is
				// immutable after creation (the CRD's own CEL rule enforces that),
				// its status is controller-owned (the operator's WorkshopProbe
				// reconciler writes phase/tools/helpText, never the sidecar), and
				// it is reaped with the workshop namespace rather than deleted
				// individually. So the sidecar gets create (to start a probe) plus
				// get/list/watch (to read one back and to wait on its status via
				// awaitProbe, internal/cmd/workshop/tools_probe.go) and NOTHING
				// else — no update/patch/delete, which would let it forge a probe
				// result or destroy the controller's own record of one.
				APIGroups: []string{"agentprimitives.authzed.com"},
				Resources: []string{"workshopprobes"},
				Verbs:     []string{"create", "get", "list", "watch"},
			},
		},
	}

	wsBinding = &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      workshopNamespaceRoleName,
			Namespace: nsName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     role.Name,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: ws.Namespace,
		}},
	}

	crRoleName := saName + "-cr"
	crRole = &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            crRoleName,
			Namespace:       ws.Namespace,
			OwnerReferences: owner,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{"agentprimitives.authzed.com"},
			Resources:     []string{"workshops"},
			ResourceNames: []string{ws.Name},
			Verbs:         []string{"get", "update", "patch"},
		}},
	}

	crBinding = &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            crRoleName,
			Namespace:       ws.Namespace,
			OwnerReferences: owner,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     crRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: ws.Namespace,
		}},
	}

	toolCRB = &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: WorkshopToolwriterCRBName(ws.Namespace, sess.Name),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     spiceboxWorkshopToolwriterClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: ws.Namespace,
		}},
	}

	readerCRB = &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name: WorkshopReaderCRBName(ws.Namespace, sess.Name),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     spiceboxWorkshopReaderClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      saName,
			Namespace: ws.Namespace,
		}},
	}

	return sa, role, wsBinding, crRole, crBinding, toolCRB, readerCRB
}
