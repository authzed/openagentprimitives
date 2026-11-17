package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestResolveOwnerSubject(t *testing.T) {
	const starter = "user:YWJjQGV4YW1wbGUuY29t"
	cases := []struct {
		name       string
		identMode  string
		owner      *spiceboxv1alpha1.ChannelOwnerPolicy
		starterAnn string
		outputRef  string
		want       string
		wantErr    bool
	}{
		{name: "agent + starter present ⇒ starter", identMode: "agent", starterAnn: starter, want: starter},
		{name: "agent + explicit ⇒ explicit overrides", identMode: "agent", starterAnn: starter,
			owner: &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:sec#member"}, want: "group:sec#member"},
		{name: "agent + no starter + ownerless.permission ⇒ permission", identMode: "agent",
			owner: &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:x#member"}},
			want:  "team:x#member"},
		{name: "agent + no starter + fromOutputChannel ⇒ output group", identMode: "agent",
			owner:     &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true}},
			outputRef: "slack_channel:C9#member", want: "slack_channel:C9#member"},
		{name: "agent + no starter + no declared owner + output group ⇒ output group", identMode: "agent",
			outputRef: "slack_channel:C0DEMO123#member", want: "slack_channel:C0DEMO123#member"},
		{name: "agent + no starter + declared permission ⇒ permission, not the offered output group", identMode: "agent",
			owner:     &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:x#member"}},
			outputRef: "slack_channel:C0DEMO123#member", want: "team:x#member"},
		{name: "passthrough ⇒ starter forced", identMode: "userPassthrough", starterAnn: starter, want: starter},
		{name: "agent + nothing resolvable ⇒ error", identMode: "agent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOwnerSubject(tc.identMode, tc.owner, tc.starterAnn, tc.outputRef, nil)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// goalOwnerGranter records the owner and participant subjects written.
type goalOwnerGranter struct{ owners, participants []string }

func (g *goalOwnerGranter) TouchStartedBy(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *goalOwnerGranter) TouchOwner(_ context.Context, _, _, subject string) error {
	g.owners = append(g.owners, subject)
	return nil
}
func (g *goalOwnerGranter) TouchInteractParticipant(_ context.Context, _, _, subject string) error {
	g.participants = append(g.participants, subject)
	return nil
}
func (g *goalOwnerGranter) TouchInteractParticipantUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *goalOwnerGranter) TouchDeniedUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *goalOwnerGranter) TouchInteractor(context.Context, string, string, string) error {
	return nil
}

// goalOwnersValidator answers GoalSessionOwners with a fixed result.
type goalOwnersValidator struct {
	settleGoalValidator
	owners GoalRunOwners
	err    error
}

func (v goalOwnersValidator) GoalSessionOwners(context.Context, *spiceboxv1alpha1.AgentSession) (GoalRunOwners, error) {
	return v.owners, v.err
}

func TestResolveAndWriteOwnersGoalRun(t *testing.T) {
	const manage = "agent_goal:goal-1#manage"
	const members = "group:g1#member"
	// The channel's own policy would name someone else entirely: the goal outranks it.
	ch := &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{Owner: &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:sec#member"}}}
	cases := []struct {
		name             string
		validator        goalOwnersValidator
		ceiling          *spiceboxv1alpha1.OwnerCeiling
		wantOwners       []string
		wantParticipants []string
		wantErr          bool
	}{
		{name: "group run: the goal's managers own it, its members participate, the channel policy is not consulted",
			validator: goalOwnersValidator{owners: GoalRunOwners{Owner: manage, Participant: members}}, wantOwners: []string{manage}, wantParticipants: []string{members}},
		{name: "private run (zero owners): channel policy as before",
			validator: goalOwnersValidator{}, wantOwners: []string{"group:sec#member"}},
		{name: "owners cannot be established: nothing written, error",
			validator: goalOwnersValidator{err: errors.New("goal store unavailable")}, wantErr: true},
		{name: "fixed ceiling: anyone but the goal's managers would own the run, nothing written, error",
			validator: goalOwnersValidator{owners: GoalRunOwners{Owner: manage, Participant: members}}, ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:admins#member"}, wantErr: true},
		{name: "starterOnly ceiling: a run with no starter has no owner it admits, nothing written",
			validator: goalOwnersValidator{owners: GoalRunOwners{Owner: manage, Participant: members}}, ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &goalOwnerGranter{}
			r := &Reconciler{AuthzGranter: g, GoalValidator: tc.validator}
			sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "goal-run"}, Spec: spiceboxv1alpha1.AgentSessionSpec{GoalExecution: &spiceboxv1alpha1.GoalExecutionReference{OccurrenceID: "occ"}}}
			ac := &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{IdentityMode: "agent"}}
			if tc.ceiling != nil {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{OwnerCeiling: tc.ceiling}
			}
			err := r.ResolveAndWriteOwners(context.Background(), sess, ch, ac)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, g.owners, "fail closed: no owner written")
				assert.Empty(t, g.participants)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOwners, g.owners)
			assert.Equal(t, tc.wantParticipants, g.participants)
		})
	}
}

func TestResolveGoalRunOwner(t *testing.T) {
	const manage = "agent_goal:goal-1#manage"
	cases := []struct {
		name       string
		mode       string
		ref        string
		ceiling    *spiceboxv1alpha1.OwnerCeiling
		want       string
		wantSource spiceboxv1alpha1.OwnerSource
		wantErr    bool
	}{
		{name: "agent mode: the goal's managers", mode: "agent", ref: manage, want: manage, wantSource: spiceboxv1alpha1.OwnerSourceGoal},
		{name: "passthrough: a goal run has no starter to act as", mode: "userPassthrough", ref: manage, wantErr: true},
		{name: "fixed ceiling: resolves to the admin's pin (the writer refuses it)", mode: "agent", ref: manage, ceiling: &spiceboxv1alpha1.OwnerCeiling{Fixed: "user:admin"}, want: "user:admin", wantSource: spiceboxv1alpha1.OwnerSourceCeiling},
		{name: "starterOnly ceiling: no owner admitted", mode: "agent", ref: manage, ceiling: &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true}, wantErr: true},
		{name: "no goal owner: error", mode: "agent", ref: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, src, err := spiceboxv1alpha1.ResolveGoalRunOwner(tc.mode, tc.ref, tc.ceiling)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantSource, src)
		})
	}
}
