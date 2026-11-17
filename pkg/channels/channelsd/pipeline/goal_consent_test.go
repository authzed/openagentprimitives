package pipeline

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// goalManagers answers manage on agent_goal:<id> from a fixed manager set;
// every other Authz method is unused here and panics through the nil embed.
type goalManagers struct {
	Authz
	// managers maps a goal ID to who manages it.
	managers map[string]map[identity.CanonicalUserID]bool
}

func (a goalManagers) CheckOnResource(_ context.Context, typ, id, perm string, u identity.CanonicalUserID, _ bool) (bool, error) {
	return typ == "agent_goal" && perm == "manage" && a.managers[id][u], nil
}

func emailCanon(t *testing.T, email string) identity.CanonicalUserID {
	t.Helper()
	c, err := identity.FromExternal(identity.Kind("email"), "", "", identity.Email(email)).Canonical()
	require.NoError(t, err)
	return c
}

type groupConsent struct {
	p         *Pipeline
	committer *planConsentCommitter
	mem       memory.Memory
	card      channelevents.InteractionRequestPayload
	ctx       context.Context
	alice     identity.CanonicalUserID
	carol     identity.CanonicalUserID
}

// newGroupConsent is a signed group-goal consent card for goal-1 in team/chat,
// managed by alice and carol; bob is only a member.
func newGroupConsent(t *testing.T, withAuthz bool) *groupConsent {
	t.Helper()
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	keys := provenance.MapKeyLookup{}
	newSigner := func(publisher string) *provenance.Signer {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer := provenance.NewSigner(priv, publisher)
		keys[provenance.PubKeyRef{Publisher: publisher, KeyID: signer.KeyID()}] = pub
		return signer
	}
	operator, channels := newSigner("system:operator"), newSigner("system:channelsd")
	mem := memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	g := &groupConsent{committer: &planConsentCommitter{keys: keys, decisions: map[string]memory.Entry{}}, mem: mem,
		alice: emailCanon(t, "alice@example.com"), carol: emailCanon(t, "carol@example.com")}
	g.p = &Pipeline{Mem: provenance.NewSigningMemory(mem, channels), GoalConsentCommitter: g.committer}
	goal := goals.Goal{ID: "goal-1", Domain: goals.Domain{Namespace: "team", Owner: "group:g1#member", Class: "assistant", ClassUID: "class"},
		Execution: &goals.ExecutionConsent{Session: "team/chat", SessionUID: "chat-uid", Digest: "d1"}}
	if withAuthz {
		// dave manages goal-2, a sibling in the same group, and not goal-1.
		g.p.Authz = goalManagers{managers: map[string]map[identity.CanonicalUserID]bool{
			goal.ID:  {g.alice: true, g.carol: true},
			"goal-2": {emailCanon(t, "dave@example.com"): true},
		}}
	}
	BindGoalConsentHandler(g.p)
	typ, id, perm := goal.ManageRef()
	expires := time.Now().UTC().Add(time.Hour)
	g.card = channelevents.InteractionRequestPayload{AgentSessionRef: channelevents.SessionRef{Namespace: "team", Name: "chat"}, Category: categories.GoalGroupExecutionConsent,
		RequestRef: "goalconsent-request-d1", Lead: "Approve this group goal's standing plan?", ExpiresAt: &expires,
		Resources: []channelevents.InteractionResourceRef{{Type: typ, ID: id, Permission: perm}},
		Audience:  channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
		Actions:   []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}, {ID: "deny", Label: "Decline", Kind: channelevents.ActionKindDecision}}}
	raw, err := json.Marshal(g.card)
	require.NoError(t, err)
	content, err := json.Marshal(goalconsent.Content{Goal: goal, Request: raw})
	require.NoError(t, err)
	opCtx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	_, err = provenance.NewSigningMemory(mem, operator).Put(opCtx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/chat"}, Kind: goalconsent.KindName, ID: g.card.RequestRef, CreatedAt: time.Now().UTC(), Content: content})
	require.NoError(t, err)
	g.ctx = channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd"))
	return g
}

func (g *groupConsent) click(t *testing.T, email string) (channelinteractions.Outcome, error) {
	t.Helper()
	handler, ok := channelinteractions.HandlerFor(categories.GoalGroupExecutionConsent)
	require.True(t, ok, "the group consent category is bound")
	card := g.card
	return handler(g.ctx, channelinteractions.Decision{Session: card.AgentSessionRef, Request: &card,
		Payload: channelevents.InteractionDecisionPayload{Category: card.Category, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "email", Email: identity.Email(email)}}})
}

func (g *groupConsent) decision(t *testing.T) (goalconsent.Content, bool) {
	t.Helper()
	entry, found, err := goalconsent.Find(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator"), g.mem, memory.Scope{Kind: "session", ID: "team/chat"}, "goalconsent-decision-d1")
	require.NoError(t, err)
	if !found {
		return goalconsent.Content{}, false
	}
	var c goalconsent.Content
	require.NoError(t, json.Unmarshal(entry.Content, &c))
	return c, true
}

func TestGroupGoalConsentDecidedByAManager(t *testing.T) {
	g := newGroupConsent(t, true)
	out, err := g.click(t, "alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)
	c, found := g.decision(t)
	require.True(t, found, "the decision is recorded")
	assert.Equal(t, g.alice.String(), c.Owner, "the deciding manager is recorded as the approver")
	require.NotNil(t, c.Approved)
	assert.True(t, *c.Approved)
	assert.Len(t, g.committer.decisions, 1)
	assert.Equal(t, []string{g.alice.String()}, g.committer.checks, "the goal server was asked before anything was recorded")
}

// A committer that cannot pre-check a decision cannot serve group consent.
func TestGroupGoalConsentNeedsAPreWriteCheck(t *testing.T) {
	g := newGroupConsent(t, true)
	g.p.GoalConsentCommitter = commitOnly{g.committer}
	_, err := g.click(t, "alice@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot check a decision")
	_, found := g.decision(t)
	assert.False(t, found)
}

type commitOnly struct{ c *planConsentCommitter }

func (o commitOnly) CommitGoalConsent(ctx context.Context, e memory.Entry) error {
	return o.c.CommitGoalConsent(ctx, e)
}

func TestGroupGoalConsentRefusals(t *testing.T) {
	cases := []struct {
		name      string
		withAuthz bool
		first     string
		clicker   string
		// checkErr is the server's answer to every check; afterFirst its
		// answer once the first click is done.
		checkErr, afterFirst error
		wantMsg              string
		// decided is who the recorded decision names afterwards, "" for none.
		decided string
	}{
		{name: "a member who does not manage the goal: refused, nothing recorded", withAuthz: true, clicker: "bob@example.com", wantMsg: "manage"},
		{name: "no authority to check managers: refused, nothing recorded", clicker: "alice@example.com", wantMsg: "unavailable"},
		{name: "a manager of a sibling goal in the same group: refused, nothing recorded", withAuthz: true, clicker: "dave@example.com", wantMsg: "manage"},
		{name: "the goal server refuses the decider: refused with its reason, nothing recorded", withAuthz: true, checkErr: errors.New("not allowed to start sessions of this agent"), clicker: "alice@example.com", wantMsg: "not allowed to start"},
		{name: "a second manager on a decided digest: the server says already decided, one decision", withAuthz: true, first: "alice@example.com", afterFirst: errors.New("this request was already decided"), clicker: "carol@example.com", wantMsg: "already decided", decided: "alice@example.com"},
		{name: "a recorded decision the server never committed: ask for a fresh request", withAuthz: true, first: "alice@example.com", clicker: "carol@example.com", wantMsg: "request it again", decided: "alice@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupConsent(t, tc.withAuthz)
			g.committer.checkErr = tc.checkErr
			if tc.first != "" {
				_, err := g.click(t, tc.first)
				require.NoError(t, err, "the first manager's click decides")
				g.committer.checkErr = tc.afterFirst
			}
			_, err := g.click(t, tc.clicker)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			c, found := g.decision(t)
			if tc.decided == "" {
				assert.False(t, found, "no decision is recorded")
				assert.Empty(t, g.committer.decisions, "nothing is committed")
				return
			}
			require.True(t, found)
			assert.Equal(t, emailCanon(t, tc.decided).String(), c.Owner, "the first decision stands")
			assert.Len(t, g.committer.decisions, 1, "committed once")
		})
	}
}

// A group goal's card must arrive under the group category, and a private
// goal's under the private one: the category is what picked the decider policy.
func TestGroupGoalConsentUnderThePrivateCategoryIsRefused(t *testing.T) {
	g := newGroupConsent(t, true)
	handler, ok := channelinteractions.HandlerFor(categories.GoalExecutionConsent)
	require.True(t, ok)
	card := g.card
	_, err := handler(g.ctx, channelinteractions.Decision{Session: card.AgentSessionRef, Request: &card,
		Payload: channelevents.InteractionDecisionPayload{Category: categories.GoalExecutionConsent, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "email", Email: "alice@example.com"}}})
	require.Error(t, err)
	_, found := g.decision(t)
	assert.False(t, found)
}
