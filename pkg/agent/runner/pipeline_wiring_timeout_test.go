package runner

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// consolidatedTimeout is the per-class authz.approvalTimeout the three approval
// asks must all source from after Phase 4's consolidation.
const consolidatedTimeout = 7 * time.Minute

func loopWithConsolidatedTimeout(t *testing.T) *Loop {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	key := memory.NamespacedName{Namespace: "default", Name: "tw1"}
	return &Loop{
		Mem:               mem,
		ResourceStandings: testResourceStandings(),
		SessionKey:        key,
		AgentClass: &spiceboxv1alpha1.AgentClass{
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{
					ApprovalTimeout: &metav1.Duration{Duration: consolidatedTimeout},
					Scope: &spiceboxv1alpha1.ScopeSpec{
						Enabled:   true,
						ColdStart: "extractAndApprove",
					},
				},
			},
		},
	}
}

// TestColdStartScopeDeps_TimeoutFromConsolidated asserts the cold-start scope
// wait derives from authz.approvalTimeout, not the retired scope sub-field.
func TestColdStartScopeDeps_TimeoutFromConsolidated(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	host := &runnerHost{l: l}
	deps := l.coldStartScopeDeps(host)
	assert.Equal(t, consolidatedTimeout, deps.ApprovalTimeout,
		"cold-start ApprovalTimeout must source from authz.approvalTimeout")
}

// TestBuildToolCallApprovalAsk_TimeoutFromConsolidated asserts the tool_call
// ApprovalAsk carries the consolidated timeout (previously it was orphaned at 0
// → the executor's 10m default).
func TestBuildToolCallApprovalAsk_TimeoutFromConsolidated(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}

	ask, err := l.buildToolCallApprovalAsk(memory.WithSystemApproval(context.Background(), "test"), "do_thing", map[string]any{}, authz.Permission{StateImpact: authz.External}, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
	require.NoError(t, err)
	require.NotNil(t, ask)
	assert.Equal(t, consolidatedTimeout, ask.Timeout,
		"tool_call ApprovalAsk.Timeout must source from authz.approvalTimeout")
}

// TestBuildLeakageApprovalAsk_TimeoutFromConsolidated asserts the leakage_share
// ApprovalAsk carries the consolidated timeout.
func TestBuildLeakageApprovalAsk_TimeoutFromConsolidated(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"}

	taint := []infoleakagetaint.TaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}}
	ask, err := l.buildLeakageApprovalAsk(memory.WithSystemApproval(context.Background(), "test"), []string{"user:bob"}, taint, "proposed text")
	require.NoError(t, err)
	require.NotNil(t, ask)
	assert.Equal(t, consolidatedTimeout, ask.Timeout,
		"leakage_share ApprovalAsk.Timeout must source from authz.approvalTimeout")
}

// testResourceStandings is the standing declaration a Loop needs before it can
// route any approval that names a resource.
//
// In production this arrives on AgentClass.status.resolvedResourceStandings; a
// Loop that has none refuses to build the ask, because there is no safe way to
// guess who may approve a type nobody classified. Tests get the fixture types
// declared here so they exercise routing rather than that refusal — the refusal
// itself has its own coverage, and a type deliberately left OUT of this map
// still hits it.
//
// `required` types name `owner` because their fixtures seed owner tuples;
// git_repo is session-only because nothing writes a tuple per git remote.
func testResourceStandings() map[string]ResourceStanding {
	req := func(perm string) ResourceStanding {
		return ResourceStanding{Standing: spiceboxv1alpha1.StandingRequired, ApproverPermission: perm}
	}
	sessionOnly := ResourceStanding{Standing: spiceboxv1alpha1.StandingSessionOnly}
	return map[string]ResourceStanding{
		"github_repo":   req("owner"),
		"issue":         req("owner"),
		"crm_company":   req("owner"),
		"tracker_issue": req("owner"),
		"repo":          req("owner"),
		"r":             req("owner"),
		"mailbox":       req("owner"),
		"linear_issue":  req("owner"),
		"record":        req("owner"),
		"data":          req("owner"),
		"git_repo":      sessionOnly,
	}
}
