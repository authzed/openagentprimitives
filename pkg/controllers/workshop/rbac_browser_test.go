package workshop

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestBrowserRoleMatchesStaticBuilderRole guards the invariant
// BuildWorkshopBrowserRBAC's own doc comment asserts but nothing previously
// checked: the per-workshop browser Role it returns must mirror
// config/webd/role-builder.yaml — the STATIC Role webd holds in the fixed
// builder-class namespace — exactly. Both grant the identical three writes
// (Channel, AgentSession, creds Secret) for the identical reason (webd
// creating a person's own browser session), just in two different namespace
// shapes: one fixed system namespace, one per-workshop namespace. Widen one
// without the other and either a workshop grants more than the static start
// path ever did (an unreviewed escalation, silent because nothing compares
// the two) or less (a workshop-started session breaks while the static path
// keeps working). This test is the one place that comparison happens.
func TestBrowserRoleMatchesStaticBuilderRole(t *testing.T) {
	var staticRole rbacv1.Role
	readRepoYAML(t, filepath.Join("config", "webd", "role-builder.yaml"), &staticRole)
	require.NotEmpty(t, staticRole.Rules, "role-builder.yaml carried no rules — wrong document?")

	dynRole, _ := BuildWorkshopBrowserRBAC(
		&spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "agentprimitives-system"}},
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "agentprimitives-system", UID: "uid"}},
		"spicebox-webd", "agentprimitives-system",
	)

	assert.ElementsMatch(t, normalizeRules(staticRole.Rules), normalizeRules(dynRole.Rules),
		"the workshop browser Role must grant EXACTLY what config/webd/role-builder.yaml grants — widen one and widen the other")
}

// normalizeRules sorts each rule's APIGroups/Resources/Verbs so two rule sets
// that list the same (group, resource, verb) triples in a different order —
// rule order, or slice order within a rule — compare equal.
func normalizeRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	out := make([]rbacv1.PolicyRule, len(rules))
	for i, r := range rules {
		out[i] = rbacv1.PolicyRule{
			APIGroups: sortedCopy(r.APIGroups),
			Resources: sortedCopy(r.Resources),
			Verbs:     sortedCopy(r.Verbs),
		}
	}
	return out
}

func sortedCopy(ss []string) []string {
	out := append([]string{}, ss...)
	sort.Strings(out)
	return out
}
