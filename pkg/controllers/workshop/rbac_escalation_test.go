package workshop

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestOperatorCanGrantTheWorkshopRole is the STATIC guard for the live failure
// on oap-desktop 2026-09-11: the Workshop stuck at RBACReady=False because the
// operator tried to create the workshop-agent Role granting
// "workshopprobes: create" while its OWN ClusterRole held only get/list/watch.
// Kubernetes RBAC escalation prevention refuses to let a non-superuser grant a
// verb it does not hold, so provisioning wedged and the sidecar never got its
// identity.
//
// The existing TestWorkshopRBACSufficiency (integration) could not catch this:
// envtest's test client is cluster-admin and BYPASSES the escalation check, so
// the Role created fine there. This is the missing half — that the operator's
// SHIPPED role is a superset of everything BuildWorkshopRBAC hands out, checked
// against the generated role.yaml with no apiserver at all. It fails by naming
// the exact (group, resource, verb) the operator must add.
func TestOperatorCanGrantTheWorkshopRole(t *testing.T) {
	ws := &spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "agentprimitives-system"}}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "agentprimitives-system", UID: "uid"}}
	_, role, _, crRole, _, _, _ := BuildWorkshopRBAC(ws, sess)

	// BuildWorkshopBrowserRBAC's Role is a SEPARATE grant the operator must
	// also be able to hand out without RBAC escalation — the same live-failure
	// shape as workshop-agent above, just for the browser's start Role instead
	// of the sidecar's. browserRole here mirrors config/webd/role-builder.yaml
	// (see TestBrowserRoleMatchesStaticBuilderRole), which this test does not
	// re-check; it only asks whether the operator holds what it grants.
	browserRole, _ := BuildWorkshopBrowserRBAC(ws, sess, "spicebox-webd", "agentprimitives-system")

	held := operatorHeldRules(t)
	grantedRules := append([]rbacv1.PolicyRule{}, role.Rules...)
	grantedRules = append(grantedRules, crRole.Rules...)
	grantedRules = append(grantedRules, browserRole.Rules...)
	for _, granted := range grantedRules {
		for _, g := range apiGroupsOrEmpty(granted.APIGroups) {
			for _, res := range granted.Resources {
				for _, verb := range granted.Verbs {
					assert.Truef(t, held.allows(g, res, verb),
						"operator ClusterRole must hold %q on %q/%q to GRANT it to the workshop SA without RBAC escalation "+
							"(add the +kubebuilder:rbac marker; re-run mage gen:api)", verb, g, res)
				}
			}
		}
	}
}

func apiGroupsOrEmpty(g []string) []string {
	if len(g) == 0 {
		return []string{""}
	}
	return g
}

type ruleSet struct{ rules []rbacv1.PolicyRule }

func (s ruleSet) allows(group, resource, verb string) bool {
	for _, r := range s.rules {
		if !contains(r.APIGroups, group) && !contains(r.APIGroups, "*") {
			continue
		}
		if !contains(r.Resources, resource) && !contains(r.Resources, "*") {
			continue
		}
		if contains(r.Verbs, verb) || contains(r.Verbs, "*") {
			return true
		}
	}
	return false
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// operatorHeldRules reads the GENERATED operator ClusterRole
// (config/manager/role.yaml) — the single source `mage manifests` bundles into
// install.yaml — so this test tracks exactly what the shipped operator holds.
func operatorHeldRules(t *testing.T) ruleSet {
	t.Helper()
	var role rbacv1.ClusterRole
	readRepoYAML(t, filepath.Join("config", "manager", "role.yaml"), &role)
	require.NotEmpty(t, role.Rules, "role.yaml carried no rules — wrong document?")
	return ruleSet{rules: role.Rules}
}
