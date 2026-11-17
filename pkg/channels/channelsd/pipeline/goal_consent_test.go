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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

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
	nats      *fakeNATS
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
	goal := goals.Goal{ID: "goal-1", Domain: goals.Domain{Namespace: "team", Owner: "group:g1#member", Class: "assistant", ClassUID: "class"},
		Execution: &goals.ExecutionConsent{Session: "team/chat", SessionUID: "chat-uid", Digest: "d1"}}
	// The pipeline is a real one, so a click can also travel the whole
	// decision pipe (HandleInteractionDecision), not just the bound handler.
	var az Authz
	if withAuthz {
		// dave manages goal-2, a sibling in the same group, and not goal-1.
		az = goalManagers{managers: map[string]map[identity.CanonicalUserID]bool{
			goal.ID:  {g.alice: true, g.carol: true},
			"goal-2": {emailCanon(t, "dave@example.com"): true},
		}}
	}
	g.nats = &fakeNATS{}
	g.p = NewPipeline(nil, az, &fakeMemory{seeded: map[string][]MemTurn{}}, g.nats, func(string) []string { return nil })
	g.p.Mem = provenance.NewSigningMemory(mem, channels)
	g.p.GoalConsentCommitter = g.committer
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

// conversationClient holds the Slack conversation session the card was posted
// in (not a goal run), with its Channel, so the pipe can resolve where a
// decision's outcome is shown.
func conversationClient(t *testing.T, key client.ObjectKey, goalRun bool) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	ch := &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: key.Namespace},
		Spec: spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: "ac1", CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"}}}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "chat-uid"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "ac1", InputChannel: &spiceboxv1alpha1.ChannelBinding{
			Name: "c1", Kind: "slack", Key: "thread:C1:1", NATSSubjectPrefix: "ap.session." + key.Namespace + "." + key.Name}},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning}}
	if goalRun {
		sess.Spec.GoalExecution = &spiceboxv1alpha1.GoalExecutionReference{GoalID: "goal-1", OccurrenceID: "occ-1", GoalRevision: 1, ConsentDigest: "d0"}
	}
	class := agentClass(t, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve, "")
	class.Namespace = key.Namespace
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(ch, sess, class).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
}

// pipeClick sends a click on the group consent card through the whole
// decision pipe, as a transport delivers it: on the session's ordinary inbound
// subject (Slack's listener and the fake harness PublishIn), or on the
// component tree when component is set.
//
// goalRun makes the card's session a group goal run, where a system renewal
// card is posted; the goal server vouches for it as one.
func (g *groupConsent) pipeClick(t *testing.T, decider channelevents.ExternalIdentity, component, goalRun bool) error {
	t.Helper()
	sessKey := client.ObjectKey{Namespace: g.card.AgentSessionRef.Namespace, Name: g.card.AgentSessionRef.Name}
	g.p.K8s = conversationClient(t, sessKey, goalRun)
	if goalRun {
		g.p.GroupGoalRuns = &fakeGroupGoalRuns{group: true}
	}
	// The outbound relay notes the card before it is shown, as for any
	// cached-resurface category; this is the request the pipe hands on.
	notePendingInteractionRequest(t, sysApproved{inner: g.p.Mem}, sessKey, g.card.Category, g.card.RequestRef, g.card)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	if component {
		ctx = channelevents.WithComponentDecisionIngress(ctx)
	}
	return g.p.HandleInteractionDecision(ctx, mustBuildInteractionDecision(t, sessKey, g.card.Category, g.card.RequestRef, "approve", decider))
}

// A group goal's consent card — the standing-plan card in the conversation
// that asked for it, and the renewal card a run posts, which is the same
// category — is clicked in a Slack or fake conversation, so the click arrives
// by channel ingress. A manager's click there decides it; the category's
// resource-owner check (agent_goal:<id>#manage), the goal server's pre-write
// check and the append-only decision record are exactly as for component
// ingress, so a member's click is refused and records nothing.
func TestGroupGoalConsentByChannelIngress(t *testing.T) {
	slackUser := func(id, email string) channelevents.ExternalIdentity {
		return channelevents.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID(id), Email: identity.Email(email)}
	}
	cases := []struct {
		name      string
		decider   channelevents.ExternalIdentity
		component bool
		goalRun   bool
		// decided is who the recorded decision names, "" for none.
		decided  string
		rejected string
	}{
		{name: "a manager's channel click: decided, recorded, server asked first", decider: slackUser("U_ALICE", "alice@example.com"), decided: "alice@example.com"},
		{name: "a manager's component click: decided as before", decider: slackUser("U_ALICE", "alice@example.com"), component: true, decided: "alice@example.com"},
		{name: "a renewal card in a group run's thread, a manager's channel click: decided", decider: slackUser("U_CAROL", "carol@example.com"), goalRun: true, decided: "carol@example.com"},
		{name: "a renewal card in a group run's thread, a member's channel click: refused not_authorized", decider: slackUser("U_BOB", "bob@example.com"), goalRun: true, rejected: "not_authorized"},
		{name: "a member's channel click: refused not_authorized, nothing recorded", decider: slackUser("U_BOB", "bob@example.com"), rejected: "not_authorized"},
		{name: "a sibling goal's manager's channel click: refused not_authorized, nothing recorded", decider: slackUser("U_DAVE", "dave@example.com"), rejected: "not_authorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupConsent(t, true)
			require.NoError(t, g.pipeClick(t, tc.decider, tc.component, tc.goalRun))
			c, found := g.decision(t)
			if tc.decided == "" {
				assert.False(t, found, "no decision is recorded")
				assert.Empty(t, g.committer.decisions, "nothing is committed")
				assert.Empty(t, g.committer.checks, "the goal server is not asked for a clicker who fails standing")
				assert.Equal(t, tc.rejected, findInteractionDecisionRejected(t, g.nats).Class)
				return
			}
			require.True(t, found, "the decision is recorded")
			assert.Equal(t, emailCanon(t, tc.decided).String(), c.Owner, "the deciding manager is the approver")
			assert.Len(t, g.committer.decisions, 1, "committed once")
			assert.Equal(t, []string{emailCanon(t, tc.decided).String()}, g.committer.checks, "the goal server was asked before the write")
			assert.Equal(t, channelevents.OutcomeApproved, findInteractionAppliedPayload(t, g.nats, ".out").Outcome)
		})
	}
}

// A private goal's consent card is decided only by component ingress
// (browser or DM surfaces); a channel click on it is refused before anything
// is read, as it was before group cards accepted channel ingress.
func TestPrivateGoalConsentKeepsComponentIngress(t *testing.T) {
	g := newGroupConsent(t, true)
	handler, ok := channelinteractions.HandlerFor(categories.GoalExecutionConsent)
	require.True(t, ok)
	card := g.card
	card.Category = categories.GoalExecutionConsent
	channelCtx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")
	_, err := handler(channelCtx, channelinteractions.Decision{Session: card.AgentSessionRef, Request: &card,
		Payload: channelevents.InteractionDecisionPayload{Category: categories.GoalExecutionConsent, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "component decision ingress")
	assert.Empty(t, g.committer.checks)
}
