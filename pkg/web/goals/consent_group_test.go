package goals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/goals/sqlstore"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// pipelineManagers is the decision pipe's authority, answered by the group
// authority's agent_goal manage model. Every other method is unused here.
type pipelineManagers struct {
	pipeline.Authz
	auth *groupAuthority
}

func (p pipelineManagers) CheckOnResource(ctx context.Context, typ, id, perm string, u identity.CanonicalUserID, full bool) (bool, error) {
	return p.auth.CheckOnResource(ctx, typ, id, perm, u, full)
}

func canonOf(t *testing.T, email string) string {
	t.Helper()
	c, err := identity.FromExternal(identity.Kind("email"), "", "", identity.Email(email)).Canonical()
	require.NoError(t, err)
	return c.String()
}

// groupConsentFlow is a group goal managed by alice and carol (bob is a plain
// member), with a requested standing plan whose card has been published to
// the requesting conversation.
type groupConsentFlow struct {
	f                  *groupFixture
	goal               core.Goal
	card               channelevents.InteractionRequestPayload
	published          channelevents.Envelope
	alice, bob, carol  string
	handler            channelinteractions.DecisionHandler
	publisher          *ConsentPublisher
	committedDecisions int
	store              *sqlstore.Store
	signer             *provenance.Signer
}

func newGroupConsentFlow(t *testing.T) *groupConsentFlow {
	t.Helper()
	return newGroupConsentFlowWith(t, nil)
}

// newGroupConsentFlowWith is newGroupConsentFlow with the class adjusted by
// mutate before the request pins its digest.
func newGroupConsentFlowWith(t *testing.T, mutate func(*v1.AgentClass)) *groupConsentFlow {
	t.Helper()
	f := newGroupFixture(t)
	c := &groupConsentFlow{f: f, alice: canonOf(t, "alice@example.com"), bob: canonOf(t, "bob@example.com"), carol: canonOf(t, "carol@example.com")}
	for _, who := range []string{c.alice, c.bob, c.carol} {
		f.auth.members[who] = true
	}
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	binding := &v1.ChannelBinding{Name: "team-chat", Kind: "grouptest", Key: "thread:1", External: map[string]string{"group": "g1"}}
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "session", UID: "session-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: binding}}
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "team-chat", UID: "channel-uid"}, Spec: v1.ChannelSpec{Kind: "grouptest"}}
	ai := &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "bot", UID: "aid-uid"}}
	// A start gate, so a decider's start permission is checked (the fake
	// answers it from f.auth.startDenied).
	class := groupExecClass()
	class.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{"user:" + c.alice}}
	if mutate != nil {
		mutate(class)
	}
	f.s.Reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess, ch, ai).Build()
	f.s.Service.ExecutionAuth = f.s
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "goals.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	c.store = goalsqlite.New(db.DB())
	require.NoError(t, c.store.Migrate(operatorCtx()))
	f.s.Service.Store = c.store

	f.turnProof(t, c.alice, "r1")
	status, r, body := f.as(t, "r1", groupCreate("create"))
	require.Equal(t, 200, status, body)
	status, r, body = f.as(t, "r1", managerChange("add-carol", *r.Goal, "add_manager", c.carol))
	require.Equal(t, 200, status, body)
	status, r, body = f.as(t, "r1", groupUpdate("activate", *r.Goal, "activate"))
	require.Equal(t, 200, status, body)
	g := *r.Goal

	now := time.Now().UTC()
	exec := core.ExecutionRequest{RequestID: "exec-1", ID: g.ID, Revision: g.Revision, Terms: core.ExecutionTerms{
		DueAt: now.Add(time.Hour), ExpiresAt: now.Add(60 * 24 * time.Hour),
		Bounds:            core.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 90},
		AllowedOperations: []string{"respond_to_user", "report_goal_result"}, Evidence: []string{"weekly summary"},
		ActionApproval: "standing_plan", StandingPlan: groupStandingPlan(t),
	}}
	status, r, body = f.as(t, "r1", core.Request{Operation: "request_execution", Scope: "group", Resource: groupResource(), Execution: exec})
	require.Equal(t, 200, status, body)
	c.goal = *r.Goal
	require.NotNil(t, c.goal.Execution)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:operator")
	c.signer = signer
	f.keys[provenance.PubKeyRef{Publisher: "system:operator", KeyID: signer.KeyID()}] = pub
	c.publisher = &ConsentPublisher{Service: f.s.Service, Memory: f.mem, Writer: provenance.NewSigningMemory(f.mem, signer), Publish: func(_ string, raw []byte) error {
		require.NoError(t, json.Unmarshal(raw, &c.published))
		require.NoError(t, json.Unmarshal(c.published.Payload, &c.card))
		return parkedprompt.Note(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd"), f.mem,
			memory.Scope{Kind: "session", ID: c.published.Session.Namespace + "/" + c.published.Session.Name}, parkedprompt.Content{RequestRef: c.card.RequestRef, Category: c.card.Category, Envelope: raw})
	}}
	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: c.goal, Action: "request_execution"}))

	f.s.Tokens.SetChannelsdToken("channelsd")
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	p := &pipeline.Pipeline{Mem: f.signed, GoalConsentCommitter: countingCommitter{inner: consentHTTP{server: f.s, token: "channelsd"}, n: &c.committedDecisions}, Authz: pipelineManagers{auth: f.auth}}
	pipeline.BindGoalConsentHandler(p)
	handler, ok := channelinteractions.HandlerFor(categories.GoalGroupExecutionConsent)
	require.True(t, ok, "the group consent category has a bound handler")
	c.handler = handler
	return c
}

type countingCommitter struct {
	inner consentHTTP
	n     *int
}

func (c countingCommitter) CommitGoalConsent(ctx context.Context, e memory.Entry) error {
	*c.n++
	return c.inner.CommitGoalConsent(ctx, e)
}

func (c countingCommitter) CheckGoalConsent(ctx context.Context, request memory.Entry, decider string, approved bool) error {
	return c.inner.CheckGoalConsent(ctx, request, decider, approved)
}

func (c *groupConsentFlow) click(t *testing.T, email string) (channelinteractions.Outcome, error) {
	t.Helper()
	card := c.card
	ctx := channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd"))
	return c.handler(ctx, channelinteractions.Decision{Session: card.AgentSessionRef, Request: &card,
		Payload: channelevents.InteractionDecisionPayload{Category: card.Category, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "email", Email: identity.Email(email)}}})
}

// executionTuples are the relations written on this goal's execution grant.
func (c *groupConsentFlow) executionTuples() []authz.Relation {
	var out []authz.Relation
	for _, r := range c.f.auth.written {
		if r.ResourceType == ExecutionResourceType && r.ResourceID == c.goal.Execution.Digest {
			out = append(out, r)
		}
	}
	return out
}

func TestGroupConsentCardIsPostedInTheConversationForTheGoalsManagers(t *testing.T) {
	c := newGroupConsentFlow(t)
	assert.Equal(t, channelevents.SessionRef{Namespace: groupNS, Name: "session"}, c.published.Session, "published to the requesting conversation")
	assert.Equal(t, categories.GoalGroupExecutionConsent, c.card.Category)
	assert.Equal(t, "goalconsent-request-"+c.goal.Execution.Digest, c.card.RequestRef)
	assert.Equal(t, []channelevents.InteractionResourceRef{{Type: GoalResourceType, ID: c.goal.ID, Permission: "manage"}}, c.card.Resources,
		"the deciders are whoever manages this goal at click time, not the domain")
	assert.Equal(t, channelevents.AudienceParticipants, c.card.Audience.Scope, "posted for the conversation, not addressed to one owner")
	assert.Nil(t, c.card.Audience.Requester, "no single owner is addressed")
	require.NoError(t, c.card.Validate())

	fields := map[string]string{}
	for _, field := range c.card.Fields {
		fields[field.Label] = field.Value
	}
	assert.Contains(t, fields["Standing plan"], "acme/app", "the standing plan's resources are on the card")
	assert.Contains(t, fields["Posts to"], "team-chat", "the group destination is on the card")
	assert.Contains(t, fields["Who can approve"], "manager")
	assert.Equal(t, "assistant", fields["Agent"], "Agent is read back as a display name, so it carries the class alone")
	assert.Contains(t, fields["Acts as"], `"bot"`, "the pinned AgentIdentity is on the card")
	assert.NotContains(t, c.card.Body, c.goal.Title, "untrusted goal content stays in the excerpt")
	require.NotNil(t, c.card.Excerpt)
	assert.Contains(t, c.card.Excerpt.Content, c.goal.Title)

	cat, ok := channelinteractions.Get(c.card.Category)
	require.True(t, ok)
	assert.Equal(t, channelinteractions.DecideResourceOwners, cat.Deciders)
	resources := []authz.ApproverResourceRef{{Type: c.card.Resources[0].Type, ID: c.card.Resources[0].ID, Permission: c.card.Resources[0].Permission}}
	for _, tc := range []struct {
		who  string
		want bool
	}{{c.alice, true}, {c.carol, true}, {c.bob, false}} {
		ok, err := authz.CheckApproverAuthorized(context.Background(), pipelineManagers{auth: c.f.auth}, groupNS, "session", resources, identity.CanonicalFromTrusted(tc.who, "test"))
		require.NoError(t, err)
		assert.Equal(t, tc.want, ok, "decider policy standing for %s", tc.who)
	}
}

func TestGroupConsentMemberClickIsRefused(t *testing.T) {
	c := newGroupConsentFlow(t)
	_, err := c.click(t, "bob@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manage")
	g, err := c.f.s.Service.Store.Get(context.Background(), c.goal.Domain, c.goal.ID)
	require.NoError(t, err)
	assert.Nil(t, g.Execution.Decision, "no decision recorded")
	assert.Empty(t, c.executionTuples(), "no grant written")
}

func TestGroupConsentManagerApprovesWithADomainAnchoredGrant(t *testing.T) {
	c := newGroupConsentFlow(t)
	ctx := context.Background()
	out, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	g, err := c.f.s.Service.Store.Get(ctx, c.goal.Domain, c.goal.ID)
	require.NoError(t, err)
	require.NotNil(t, g.Execution.Decision)
	assert.True(t, g.Execution.Decision.Approved)
	assert.Equal(t, c.alice, g.Execution.Decision.Owner, "the deciding manager is recorded as the approver")
	require.NoError(t, c.f.s.VerifyDecision(ctx, g, *g.Execution.Decision))

	domainID := g.Domain.ID()
	expires := g.Execution.Terms.ExpiresAt
	assert.ElementsMatch(t, []authz.Relation{
		{ResourceType: ExecutionResourceType, ResourceID: g.Execution.Digest, Relation: "domain", SubjectType: ResourceType, SubjectID: domainID},
		{ResourceType: ExecutionResourceType, ResourceID: g.Execution.Digest, Relation: "domain_approved", SubjectType: ResourceType, SubjectID: domainID, ExpiresAt: expires},
	}, c.executionTuples(), "the grant is anchored to the domain with the terms' expiry, never to the approving user")

	// A second manager on the same digest is told it is
	// already decided; there is one decision and one grant.
	_, err = c.click(t, "carol@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already decided")
	after, err := c.f.s.Service.Store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	assert.Equal(t, *g.Execution.Decision, *after.Execution.Decision, "the first decision stands")
	assert.Len(t, c.executionTuples(), 2, "no second grant")
	assert.Equal(t, 1, c.committedDecisions, "committed once")

	require.NoError(t, c.f.s.AuthorizeDispatch(ctx, g))
	assert.Contains(t, c.f.auth.subjectChecks, ExecutionResourceType+":"+g.Execution.Digest+"#execute_group@"+ResourceType+":"+domainID,
		"dispatch checks execute_group with the domain itself as subject")

	c.f.auth.members[c.alice] = false
	require.NoError(t, c.f.s.AuthorizeDispatch(ctx, g), "the approver leaving the group does not revoke the domain's grant")
}

// DecideExecution checks manage live on its own: a channelsd-signed decision
// naming someone who does not manage the goal is refused there too.
func TestGroupDecisionFromANonManagerIsRefusedByTheGoalServer(t *testing.T) {
	c := newGroupConsentFlow(t)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	scope := memory.Scope{Kind: "session", ID: groupNS + "/session"}
	request, found, err := goalconsent.Find(ctx, c.f.mem, scope, c.card.RequestRef)
	require.NoError(t, err)
	require.True(t, found)
	var content goalconsent.Content
	require.NoError(t, json.Unmarshal(request.Content, &content))
	witness, err := json.Marshal(request)
	require.NoError(t, err)
	approved := true
	content.Owner, content.Approved, content.RequestWitness = c.bob, &approved, witness
	raw, err := json.Marshal(content)
	require.NoError(t, err)
	decision, err := c.f.signed.Put(ctx, memory.Entry{Scope: scope, Kind: goalconsent.KindName, ID: "goalconsent-decision-" + c.goal.Execution.Digest, Content: raw, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)})
	require.NoError(t, err)

	err = consentHTTP{server: c.f.s, token: "channelsd"}.CommitGoalConsent(context.Background(), decision)
	var status *statusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusNotFound, status.code, "refused as denied")
	g, err := c.f.s.Service.Store.Get(context.Background(), c.goal.Domain, c.goal.ID)
	require.NoError(t, err)
	assert.Nil(t, g.Execution.Decision)
	assert.Empty(t, c.executionTuples())
}

// recordedDecision reports whether a decision entry exists for the card's digest.
func (c *groupConsentFlow) recordedDecision(t *testing.T) bool {
	t.Helper()
	_, found, err := goalconsent.Find(operatorCtx(), c.f.mem, memory.Scope{Kind: "session", ID: groupNS + "/session"}, "goalconsent-decision-"+c.goal.Execution.Digest)
	require.NoError(t, err)
	return found
}

func (c *groupConsentFlow) stored(t *testing.T) core.Goal {
	t.Helper()
	g, err := c.f.s.Service.Store.Get(context.Background(), c.goal.Domain, c.goal.ID)
	require.NoError(t, err)
	return g
}

// A click the goal server would refuse is refused BEFORE the append-only
// decision record is written, so it cannot block another manager.
func TestGroupConsentRefusedClickWritesNothing(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, c *groupConsentFlow) string
		wantMsg string
	}{
		{
			name: "a manager refused by the class start gate at decide time: nothing written",
			setup: func(_ *testing.T, c *groupConsentFlow) string {
				c.f.auth.startDenied = map[string]bool{c.alice: true}
				return "alice@example.com"
			},
			wantMsg: "not allowed to start",
		},
		{
			name: "a member with a stale manager tuple but not listed on the goal: refused before writing",
			setup: func(t *testing.T, c *groupConsentFlow) string {
				dave := canonOf(t, "dave@example.com")
				c.f.auth.members[dave] = true
				c.f.auth.tuples[authz.Relation{ResourceType: GoalResourceType, ResourceID: c.goal.ID, Relation: "manager", SubjectType: "user", SubjectID: dave}] = true
				return "dave@example.com"
			},
			wantMsg: "only a manager",
		},
		{
			name: "a manager of a sibling goal in the same group: refused",
			setup: func(t *testing.T, c *groupConsentFlow) string {
				eve := canonOf(t, "eve@example.com")
				c.f.auth.members[eve] = true
				c.f.turnProof(t, eve, "r2")
				status, _, body := c.f.as(t, "r2", groupCreate("sibling"))
				require.Equal(t, 200, status, body)
				return "eve@example.com"
			},
			wantMsg: "manage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGroupConsentFlow(t)
			who := tc.setup(t, c)
			_, err := c.click(t, who)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.False(t, c.recordedDecision(t), "no decision record written")
			assert.Nil(t, c.stored(t).Execution.Decision)
			assert.Empty(t, c.executionTuples())
			assert.Zero(t, c.committedDecisions)
		})
	}
}

func TestGroupConsentAnotherManagerDecidesAfterARefusedClick(t *testing.T) {
	c := newGroupConsentFlow(t)
	c.f.auth.startDenied = map[string]bool{c.alice: true}
	_, err := c.click(t, "alice@example.com")
	require.Error(t, err, "alice may not start this agent")

	out, err := c.click(t, "carol@example.com")
	require.NoError(t, err, "carol is not blocked by alice's refused click")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)
	g := c.stored(t)
	require.NotNil(t, g.Execution.Decision)
	assert.Equal(t, c.carol, g.Execution.Decision.Owner)
	assert.Len(t, c.executionTuples(), 2, "one grant")
	assert.Equal(t, 1, c.committedDecisions, "one decision")
}

// A decision whose grant write failed is completed by the same manager
// clicking again: the recorded decision is re-committed and the grant written.
func TestGroupConsentGrantWriteFailureIsRetriedByTheSameManager(t *testing.T) {
	c := newGroupConsentFlow(t)
	c.f.auth.failExecutionWrites = true
	_, err := c.click(t, "alice@example.com")
	require.Error(t, err)
	require.NotNil(t, c.stored(t).Execution.Decision, "the decision committed before the grant write failed")
	assert.Empty(t, c.executionTuples())

	c.f.auth.failExecutionWrites = false
	_, err = c.click(t, "alice@example.com")
	require.NoError(t, err)
	assert.Len(t, c.executionTuples(), 2, "the retry writes the grant")
	require.NoError(t, c.f.s.AuthorizeDispatch(context.Background(), c.stored(t)))
}

// The approved-decision event schedules the group goal rather than being
// consumed as "not eligible"; a refusal that may clear is kept for retry.
func TestGroupConsentDecisionEventSchedules(t *testing.T) {
	c := newGroupConsentFlow(t)
	_, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	g := c.stored(t)

	groupTestMembershipErr = errors.New("directory sync is not Ready")
	err = c.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "execution_decision"})
	require.Error(t, err, "membership not current may clear: the event is kept, not consumed")
	assert.Contains(t, err.Error(), "membership is not kept current")
	due, err := c.store.Due(operatorCtx(), g.Execution.Terms.DueAt.Add(time.Minute), 100)
	require.NoError(t, err)
	assert.Empty(t, due)

	groupTestMembershipErr = nil
	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "execution_decision"}))
	due, err = c.store.Due(operatorCtx(), g.Execution.Terms.DueAt.Add(time.Minute), 100)
	require.NoError(t, err)
	require.Len(t, due, 1, "the approved group goal is scheduled")
	assert.Equal(t, g.ID, due[0].GoalID)
}

// A definitive refusal (the pinned AgentIdentity is gone) consumes the event:
// retrying cannot help, and a fresh approval is a new event.
func TestGroupConsentDecisionEventForAnIneligibleGoalIsConsumed(t *testing.T) {
	c := newGroupConsentFlow(t)
	_, err := c.click(t, "alice@example.com")
	require.NoError(t, err)
	g := c.stored(t)
	require.NoError(t, c.f.s.Reader.(client.Client).Delete(context.Background(), &v1.AgentIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: groupNS, Name: "bot"}}))
	require.NoError(t, c.publisher.Notify(context.Background(), core.Event{Goal: g, Action: "execution_decision"}))
	due, err := c.store.Due(operatorCtx(), g.Execution.Terms.DueAt.Add(time.Minute), 100)
	require.NoError(t, err)
	assert.Empty(t, due, "nothing scheduled")
}
