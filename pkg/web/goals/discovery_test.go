package goals

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	eventnative "github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsql "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionobservation"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	eventsql "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProductionDiscoveryConsentAndAcceptance(t *testing.T) {
	f := fixture(t)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "discovery.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := goalsql.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	f.s.Service.Store = store
	f.s.Service.ExecutionAuth = f.s
	owner := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test owner")
	var source v1.AgentSession
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "session"}, &source))
	source.Spec.InputChannel = &v1.ChannelBinding{Name: "private", Kind: "browser", Key: "private-human"}
	var class v1.AgentClass
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "assistant"}, &class))
	class.Spec.IdentityMode = "userPassthrough"
	class.Spec.Authz.ToolCalls = &v1.ToolCallsAuthz{Mode: "enforcing"}
	class.Spec.Authz.PlanGate = &v1.PlanGateConfig{Mode: "enforcing", RequirePlan: ptr.To(true), Rendering: &v1.PlanRendering{MaxAutoApproveHandles: 0}}
	channel := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "private", UID: "private-uid"}, Spec: v1.ChannelSpec{Kind: "browser"}}
	user := &v1.UserIdentity{ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(owner.Subject()), UID: "owner-uid"}, Spec: v1.UserIdentitySpec{Subject: owner.Subject().String()}}
	k8s := fake.NewClientBuilder().WithScheme(f.s.Reader.(client.Client).Scheme()).WithObjects(&source, &class, channel, user).Build()
	f.s.Reader = k8s
	f.proof(t, owner.String())
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:operator")
	f.keys[provenance.PubKeyRef{Publisher: "system:operator", KeyID: signer.KeyID()}] = pub
	var cards []channelevents.InteractionRequestPayload
	relayOffline := true
	f.s.Consent = &ConsentPublisher{Service: f.s.Service, Memory: f.mem, Writer: provenance.NewSigningMemory(f.mem, signer), Publish: func(_ string, raw []byte) error {
		var env channelevents.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		var card channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &card); err != nil {
			return err
		}
		if relayOffline {
			return nil
		}
		cards = append(cards, card)
		return parkedprompt.Note(memory.WithCaller(memory.WithSystemApproval(ctx, "system:channelsd"), "system:channelsd"), f.mem, memory.Scope{Kind: "session", ID: env.Session.Namespace + "/" + env.Session.Name}, parkedprompt.Content{RequestRef: card.RequestRef, Category: card.Category, Envelope: raw})
	}}
	eventStore := eventsql.New(db.DB(), false)
	require.NoError(t, eventStore.Migrate(ctx))
	access := &eventweb.NativeSessions{Reader: k8s, Memory: f.mem, Auth: f.auth}
	sources := sessionevents.NewRegistry()
	sources.Register(&eventnative.Adapter{Memory: f.mem, Keys: f.keys, Authority: access, Access: access, Publishers: map[string]bool{"system:channelsd": true}})
	consumers := sessionevents.NewConsumers()
	consumers.Legacy = "goals"
	execution := &domain.EventExecution{Service: f.s.Service, Sources: sources}
	triggers := &sessionevents.Triggers{Store: eventStore, Observations: eventStore, Authority: consumers}
	execution.Triggers = triggers
	consumers.Register("goals", execution)
	f.s.Service.Events = execution
	f.s.EventSources = sources
	discovery := &Discovery{Server: f.s, Store: store, Triggers: triggers, Sources: sources}
	consumers.Register("goal-discovery", discovery)
	f.s.Discovery = discovery
	f.s.Tokens.SetChannelsdToken("channelsd")
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	pipeline.BindGoalConsentHandler(&pipeline.Pipeline{Mem: f.signed, GoalConsentCommitter: consentHTTP{server: f.s, token: "channelsd"}})
	handler, ok := channelinteractions.HandlerFor(categories.GoalExecutionConsent)
	require.True(t, ok)
	click := func(card channelevents.InteractionRequestPayload, email, action string) error {
		_, err := handler(channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")), channelinteractions.Decision{Session: card.AgentSessionRef, Request: &card, Payload: channelevents.InteractionDecisionPayload{Category: card.Category, RequestRef: card.RequestRef, ActionID: action, Decider: channelevents.ExternalIdentity{Kind: "email", Email: identity.Email(email)}}})
		return err
	}
	now := time.Now().UTC()
	f.s.Service.Now = func() time.Time { return now.Add(-time.Minute) }
	request := domain.DiscoveryRequest{RequestID: "enable-trip-suggestions", Title: "Trip monitor", Outcome: "Privately report changes to the proposed trip", Predicate: sessionevents.Predicate{Kind: "trip.upcoming", Subject: "inbox"}, SubjectField: "tripID", MaxProposals: 2, MaxPending: 1, ProposalSeconds: 120, Terms: domain.ExecutionTerms{ActionApproval: "standing_private", DueAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 180, Turns: 10, Tokens: 10000, ApprovalSeconds: 120}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"Private trip report delivered"}, Event: &domain.EventWatch{Source: sessionevents.Source{Kind: "native", Namespace: "team", ID: "team/session"}, Predicate: sessionevents.Predicate{Kind: "trip.changed", Subject: "proposed-trip"}, MaxRuns: 2, RunWindowSeconds: 300, Timezone: "UTC", Burst: "skip_pending"}}}
	status, _ := f.call(t, "token", domain.Request{Operation: "request_discovery", Resource: "foreign", Discovery: request})
	require.Equal(t, http.StatusNotFound, status)
	actor, err := f.s.resolve(ctx, "team", "session")
	require.NoError(t, err)
	status, response := f.call(t, "token", domain.Request{Operation: "request_discovery", Resource: ResourceType + ":" + actor.Domain.ID(), Discovery: request})
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, response.DiscoveryPolicy)
	p := *response.DiscoveryPolicy
	require.Empty(t, cards, "a successful bus publish without a relay is not delivery")
	notified, notifyErr := store.DiscoveryPolicyNotification(ctx, p.ID)
	require.NoError(t, notifyErr)
	require.False(t, notified)
	relayOffline = false
	require.NoError(t, discovery.Tick(context.Background()))
	require.Len(t, cards, 1)
	policyCard := cards[0]
	require.Equal(t, "Suggest goals from these changes?", policyCard.Lead)
	require.NoError(t, policyCard.Validate())
	require.Error(t, click(policyCard, "bob@example.com", "approve"))
	require.NoError(t, click(policyCard, "alice@example.com", "approve"))
	f.s.Service.Now = nil
	p, err = store.DiscoveryPolicy(ctx, actor.Domain, p.ID)
	require.NoError(t, err)
	require.NotNil(t, p.Decision)
	require.NoError(t, discovery.CheckSubscription(ctx, mustDiscoverySubscription(t, p)))
	page, err := store.List(ctx, actor.Domain, domain.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, page.Goals, "policy approval grants no goal execution")
	router := &sessionevents.Router{Triggers: triggers, Store: eventStore}
	dispatcher := &sessionevents.Dispatcher{Triggers: triggers, Consumer: consumers}
	ingest := func(id, kind, subject string, data json.RawMessage) {
		o := sessionevents.Observation{Source: p.Template.Execution.Terms.Event.Source, EventID: id, Kind: kind, Subject: subject, ObservedAt: time.Now().UTC(), Data: data, Dependencies: []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: "team/session", Permission: "read_transcript"}}}
		raw, err := json.Marshal(sessionobservation.Content{Observation: o})
		require.NoError(t, err)
		entry, err := f.signed.Put(memory.WithCaller(memory.WithSystemApproval(ctx, "system:channelsd"), "system:channelsd"), memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, ID: "sessobs-" + id, Kind: sessionobservation.KindName, CreatedAt: o.ObservedAt, Content: raw})
		require.NoError(t, err)
		witness, err := json.Marshal(entry)
		require.NoError(t, err)
		_, err = (&sessionevents.Ingester{Store: eventStore, Adapters: sources}).Ingest(ctx, "native", witness)
		require.NoError(t, err)
	}
	ingest("upcoming", "trip.upcoming", "inbox", json.RawMessage(`{"tripID":"trip-1","note":"observed data, no authority"}`))
	require.NoError(t, router.Tick(context.Background(), time.Now()))
	n, err := dispatcher.Drain(context.Background(), time.Now(), 100)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	relayOffline = true
	require.NoError(t, discovery.Tick(context.Background()))
	require.Len(t, cards, 1, "proposal publish without a relay leaves the notice queued")
	relayOffline = false
	restartedDiscovery := *discovery
	restartedDiscovery.After = ""
	require.NoError(t, restartedDiscovery.Tick(context.Background()))
	require.Len(t, cards, 2)
	proposalCard := cards[1]
	require.Equal(t, "Shall I monitor this for you?", proposalCard.Lead)
	require.Contains(t, proposalCard.Body, "unattended private reports")
	require.NoError(t, proposalCard.Validate())
	var detail struct {
		Goal      domain.Goal              `json:"goal"`
		Authority domain.DiscoveryProposal `json:"authority"`
	}
	require.NoError(t, json.Unmarshal(proposalCard.Details, &detail))
	q := detail.Authority
	require.Equal(t, "trip-1", q.Goal.Execution.Terms.Event.Predicate.Subject)
	require.NotNil(t, q.Goal.Discovery)
	page, err = store.List(ctx, actor.Domain, domain.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, page.Goals, "an unanswered suggestion remains outside goals")
	// Monitor evidence predating acceptance cannot become retroactive execution.
	ingest("changed-before-approval", "trip.changed", "trip-1", json.RawMessage(`{"status":"delayed"}`))
	deceptive := proposalCard
	deceptive.Body = "Authorize unrestricted external actions"
	require.Error(t, click(deceptive, "alice@example.com", "approve"))
	f.auth.denySource = true
	require.Error(t, click(proposalCard, "alice@example.com", "approve"))
	f.auth.denySource = false
	// The decision may be retained before a grant publication fails. Exact retry
	// creates no second goal, consent, notification allowance or watch.
	f.auth.failExecutionGrant = true
	require.Error(t, click(proposalCard, "alice@example.com", "approve"))
	f.auth.failExecutionGrant = false
	page, err = store.List(ctx, actor.Domain, domain.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Goals, 1)
	require.ErrorIs(t, f.s.Service.Dispatchable(ctx, page.Goals[0]), domain.ErrDenied)
	require.NoError(t, click(proposalCard, "alice@example.com", "approve"))
	require.NoError(t, click(proposalCard, "alice@example.com", "approve"))
	page, err = store.List(ctx, actor.Domain, domain.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Goals, 1)
	g := page.Goals[0]
	require.NoError(t, f.s.VerifyDecision(ctx, g, *g.Execution.Decision))
	require.NoError(t, f.s.Consent.Notify(ctx, domain.Event{Action: "execution_decision", Goal: g}))
	require.NoError(t, router.Tick(context.Background(), time.Now()))
	n, err = dispatcher.Drain(ctx, time.Now(), 100)
	require.NoError(t, err)
	require.Zero(t, n, "pre-consent events are skipped")
	ingest("changed-after-approval", "trip.changed", "trip-1", json.RawMessage(`{"status":"delayed"}`))
	// Rehydrate the router after ingestion; durable admissions remain the cursor.
	router = &sessionevents.Router{Triggers: triggers, Store: eventStore}
	require.NoError(t, router.Tick(context.Background(), time.Now()))
	n, err = dispatcher.Drain(ctx, time.Now(), 100)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	runs, err := store.Runs(ctx, g.Domain, g.ID, domain.ListRequest{Limit: 100})
	require.NoError(t, err)
	require.Len(t, runs.Runs, 1)
	require.NoError(t, discovery.Tick(ctx))
	require.Len(t, cards, 2, "notices do not repeat each reconcile")
	require.NoError(t, store.StopDiscoveryPolicy(ctx, p.Template.Domain, p.ID))
	require.NoError(t, execution.CheckSubscription(ctx, runs.Runs[0].Event.Subscription), "stopping suggestions does not revoke a separately accepted goal")
	q, err = store.DiscoveryProposal(ctx, q.Goal.Domain, q.ID)
	require.NoError(t, err)
	require.Equal(t, "accepted", q.State)
	signed, found, err := goalconsent.Find(ctx, f.mem, memory.Scope{Kind: "session", ID: actor.Session}, proposalCard.RequestRef)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, provenance.VerifyEntrySignature(f.keys, signed))
}
func mustDiscoverySubscription(t *testing.T, p domain.DiscoveryPolicy) sessionevents.Subscription {
	t.Helper()
	s, err := domain.DiscoverySubscription(p)
	require.NoError(t, err)
	return s
}
