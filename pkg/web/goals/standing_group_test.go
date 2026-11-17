package goals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// standingRepoClass declares the standing plan's repository slot as one whose
// approver must already hold every permission it confers (read AND push).
func standingRepoClass(c *v1.AgentClass) {
	c.Spec.Authz.Slots = []v1.AuthzSlot{{ResourceType: "github_repo", Description: "a repository", Permission: "read", Permissions: []string{"push"}}}
	c.Status.ResolvedSlots = []v1.ResolvedSlot{{ResourceType: "github_repo", Permission: "read", Standing: v1.StandingRequired}}
}

// groupRun is an approved group goal with one due occurrence and a run
// session whose frozen plan is recorded in signed memory.
type groupRun struct {
	c    *groupConsentFlow
	goal core.Goal
	sess *v1.AgentSession
	plan plangate.Plan
	log  memory.Memory
}

func newGroupRun(t *testing.T) *groupRun {
	t.Helper()
	c := newGroupConsentFlowWith(t, standingRepoClass)
	_, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	g := c.stored(t)
	require.NotNil(t, g.Execution.Decision)
	due := g.Execution.Terms.DueAt
	c.f.s.Service.Now = func() time.Time { return due.Add(time.Minute) }
	o, err := c.store.Schedule(operatorCtx(), g)
	require.NoError(t, err)

	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "run", UID: "run-uid"},
		Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: &v1.GoalExecutionReference{OccurrenceID: o.ID}}}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "session:"+groupNS+"/run")
	c.f.keys[provenance.PubKeyRef{Publisher: signer.Publisher(), KeyID: signer.KeyID()}] = pub
	r := &groupRun{c: c, goal: g, sess: sess, plan: *g.Execution.Terms.StandingPlan, log: provenance.NewSigningMemory(c.f.mem, signer)}
	r.declare(t, r.plan)
	return r
}

// declare records plan as the run's frozen plan, the way the runner does.
func (r *groupRun) declare(t *testing.T, plan plangate.Plan) {
	t.Helper()
	r.plan = plan
	for i := range plan.Phases {
		rec := plangate.PhaseAuthorityRecord(plan, i, nil)
		rec.Event, rec.PlanDigest, rec.Mode = plangateaudit.EventPlanApproved, plan.Digest(), "enforcing"
		r.record(t, rec)
	}
}

func (r *groupRun) record(t *testing.T, rec plangateaudit.Content) {
	t.Helper()
	require.NoError(t, plangateaudit.Record(memory.WithSystemApproval(context.Background(), "plan_gate"), r.log, memory.Scope{Kind: "session", ID: groupNS + "/run"}, rec))
}

func (r *groupRun) derive() (*plangateaudit.ApprovalAuthority, error) {
	return r.c.f.s.derivePlanApproval(operatorCtx(), r.sess, &core.PlanApprovalRequest{Digest: r.plan.Digest(), Phase: 0})
}

func TestDeriveGroupStandingApproval(t *testing.T) {
	repo := "github_repo:acme/app"
	cases := []struct {
		name    string
		arrange func(t *testing.T, r *groupRun)
		// covered: an authority naming the approver; otherwise nil (a card)
		// unless wantErr.
		covered bool
		wantErr bool
	}{
		{name: "covered, approver still a manager holding every slot permission: authority naming the approver", covered: true},
		{name: "approver lost the class start permission after consent: still covered (start gate is consent-time only)", covered: true, arrange: func(t *testing.T, r *groupRun) {
			r.c.f.auth.startDenied = map[string]bool{r.c.alice: true}
		}},
		{name: "a manager added after approval: still covered (consent and the scheduled run untouched)", covered: true, arrange: func(t *testing.T, r *groupRun) {
			status, _, body := r.c.f.as(t, "r1", managerChange("add-bob", r.c.stored(t), "add_manager", r.c.bob))
			require.Equal(t, 200, status, body)
		}},
		{name: "the approver removed as a manager: the run still dispatches, nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			r.c.f.turnProof(t, r.c.carol, "r2")
			status, _, body := r.c.f.as(t, "r2", managerChange("remove-alice", r.c.stored(t), "remove_manager", r.c.alice))
			require.Equal(t, 200, status, body)
		}},
		{name: "run plan adds a phase: not covered, nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			wider := r.plan
			wider.Phases = append(append([]plangate.Phase{}, r.plan.Phases...), r.plan.Phases[1])
			r.declare(t, wider)
		}},
		{name: "run plan names another repository: not covered, nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			other := plangate.Plan{Phases: append([]plangate.Phase{}, r.plan.Phases...)}
			other.Phases[0].Slots = []plangate.Slot{{Type: "github_repo", ID: "acme/other"}}
			r.declare(t, other)
		}},
		{name: "approver left the group: no longer a manager, nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			r.c.f.auth.members[r.c.alice] = false
		}},
		{name: "approver's manager tuple removed: no longer a manager, nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			for rel := range r.c.f.auth.tuples {
				if rel.ResourceType == GoalResourceType && rel.Relation == "manager" && rel.SubjectID == r.c.alice {
					delete(r.c.f.auth.tuples, rel)
				}
			}
		}},
		{name: "approver lost push on the repo slot: nil (a card)", arrange: func(t *testing.T, r *groupRun) {
			r.c.f.auth.refusedPerms[repo+":push:"+r.c.alice] = true
		}},
		{name: "recorded human denial: error (deny)", wantErr: true, arrange: func(t *testing.T, r *groupRun) {
			denial := plangate.PhaseAuthorityRecord(r.plan, 0, nil)
			denial.Event, denial.PlanDigest = plangateaudit.EventDenied, r.plan.Digest()
			r.record(t, denial)
		}},
		{name: "SpiceDB error on the slot check: error (deny)", wantErr: true, arrange: func(t *testing.T, r *groupRun) {
			r.c.f.auth.checkErrs["github_repo"] = errors.New("spicedb unavailable")
		}},
		{name: "SpiceDB error on the manager check: error (deny)", wantErr: true, arrange: func(t *testing.T, r *groupRun) {
			r.c.f.auth.checkErrs[GoalResourceType] = errors.New("spicedb unavailable")
		}},
		{name: "goal paused (Execution cleared): error (deny)", wantErr: true, arrange: func(t *testing.T, r *groupRun) {
			g := r.c.stored(t)
			status, _, body := r.c.f.as(t, "r1", groupUpdate("pause", g, "pause"))
			require.Equal(t, 200, status, body)
			require.Nil(t, r.c.stored(t).Execution)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newGroupRun(t)
			if tc.arrange != nil {
				tc.arrange(t, r)
			}
			authority, err := r.derive()
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, authority)
				return
			}
			require.NoError(t, err)
			if !tc.covered {
				assert.Nil(t, authority, "not covered must fall through to a human card, never deny")
				return
			}
			require.NotNil(t, authority)
			assert.Equal(t, plangateaudit.ApprovalAuthority{
				Kind: "goal_group_standing", Approver: r.c.alice,
				Reference: r.goal.Execution.Digest, DecisionRef: r.goal.Execution.Decision.RequestID,
				OccurrenceID: r.sess.Spec.GoalExecution.OccurrenceID, SessionUID: "run-uid",
			}, *authority)
		})
	}
}
