package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

func workspaceSession(t *testing.T) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.AgentClass) {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-ws", Namespace: "default", UID: "uid-ws"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac-ws"},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-ws", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SystemPrompt:    spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			WorkspaceSource: &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "ws"},
		},
	}
	return sess, ac
}

// The Job grant is SPLIT, because Kubernetes RBAC cannot pin every verb and a
// single pinned rule silently refuses the ones it cannot.
//
// resourceNames matches the REQUEST's name. A create has none at admission and
// list/watch address a collection, so folding those into the pinned rule did not
// tighten the grant — it removed it. The runner could no longer make its
// workspace Jobs, and nothing said so until an authorization review asked; the
// version of this test that asserted a single {create,get,delete} pinned rule
// was pinning a functional break as intent.
//
// So: get and delete are pinned to this session's own two job names, and
// create/list/watch are a separate unpinned rule.
//
// WHAT REMAINS OPEN, and this test does not pretend otherwise: create is
// unpinned, and RBAC cannot constrain a pod template. A Job this runner creates
// can name any ServiceAccount and mount any Secret in the namespace, which makes
// the by-name Secret pins in the same Role defence in depth rather than a
// boundary. Sessions share a namespace (webchat uses `default`) and the
// per-session Secret name is deterministic, so the target needs no discovery.
// Closing it needs an admitting webhook on batch/jobs, not a different RBAC
// spelling.
func TestBuildRunnerRBAC_JobsRuleIsPinnedWhereRBACCanPin(t *testing.T) {
	sess, ac := workspaceSession(t)

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	var pinned, unpinned *rbacv1.PolicyRule
	for i := range role.Rules {
		r := &role.Rules[i]
		if !containsStr(r.APIGroups, "batch") || !containsStr(r.Resources, "jobs") {
			continue
		}
		if len(r.ResourceNames) > 0 {
			pinned = r
		} else {
			unpinned = r
		}
	}

	require.NotNil(t, pinned, "the verbs RBAC can pin must be pinned")
	assert.ElementsMatch(t, []string{"ws-sync-sess-ws", "ws-apply-sess-ws"}, pinned.ResourceNames,
		"the two job names the runner actually computes are deterministic, so pinning costs nothing")
	assert.ElementsMatch(t, []string{"get", "delete"}, pinned.Verbs,
		"get and delete address one object by name, so resourceNames authorizes them")

	require.NotNil(t, unpinned, "a workspace-bound class still needs to CREATE its reconcile Job")
	assert.ElementsMatch(t, []string{"create", "list", "watch"}, unpinned.Verbs,
		"exactly the verbs resourceNames cannot express, and nothing else")
	assert.Empty(t, unpinned.ResourceNames)
}

// The grant stays scoped: a class binding no workspace source gets no
// batch/jobs rule at all.
func TestBuildRunnerRBAC_NoJobsRuleWithoutAWorkspaceSource(t *testing.T) {
	sess, ac := workspaceSession(t)
	ac.Spec.WorkspaceSource = nil

	_, role, _, _, _ := agentsession.BuildRunnerRBAC(sess, ac, "tok", nil, nil, "", "", "", "", "", "", nil)

	assert.Nil(t, findRuleForResource(role.Rules, "batch", "jobs"),
		"a non-workspace agent must not receive batch/jobs at all")
}
