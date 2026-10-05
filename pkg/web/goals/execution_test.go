package goals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	eventnative "github.com/authzed/openagentprimitives/pkg/agent/sessionevents/native"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionobservation"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	eventsql "github.com/authzed/openagentprimitives/pkg/memory/sessionevents/sqlstore"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	eventweb "github.com/authzed/openagentprimitives/pkg/web/sessionevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type consentHTTP struct {
	server *Server
	token  string
}

func (c consentHTTP) CommitGoalConsent(ctx context.Context, entry memory.Entry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	request := httptest.NewRequest(http.MethodPost, "/goals/decision", bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+c.token)
	response := httptest.NewRecorder()
	c.server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		return &statusError{code: response.Code}
	}
	return nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

func TestProductionConsentLaunchAndRevocation(t *testing.T) {
	for _, mode := range []string{"standalone", "plan"} {
		for _, recurring := range []bool{false, true} {
			for _, unattended := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/recurring=%t/unattended=%t", mode, recurring, unattended), func(t *testing.T) { productionConsentLaunchAndRevocation(t, mode == "plan", recurring, unattended) })
			}
		}
	}
}

func TestProductionEventConsentLaunchAndRevocation(t *testing.T) {
	for _, inPlan := range []bool{false, true} {
		for _, standing := range []bool{false, true} {
			t.Run(fmt.Sprintf("plan=%t/standing=%t", inPlan, standing), func(t *testing.T) { productionConsentLaunchAndRevocation(t, inPlan, false, standing, true) })
		}
	}
}
func TestProductionDefaultBoundsConsentAndDispatch(t *testing.T) {
	productionConsentLaunchAndRevocation(t, true, false, true, false, true)
}

func productionConsentLaunchAndRevocation(t *testing.T, inPlan, recurring, unattended bool, eventMode ...bool) {
	watch := len(eventMode) == 1 && eventMode[0]
	var eventStore *eventsql.Store
	var eventRouter *sessionevents.Router
	var eventDispatcher *sessionevents.Dispatcher
	var eventHTTP *eventweb.Server
	f := fixture(t)
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "goals.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	store := goalsqlite.New(db.DB())
	require.NoError(t, store.Migrate(ctx))
	f.s.Service.Store = store
	f.s.Service.ExecutionAuth = f.s
	owner := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test human")
	source := &v1.AgentSession{}
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "session"}, source))
	source.Spec.InputChannel = &v1.ChannelBinding{Name: "private", Kind: "browser", Key: "private-human"}
	class := &v1.AgentClass{}
	require.NoError(t, f.s.Reader.Get(ctx, client.ObjectKey{Namespace: "team", Name: "assistant"}, class))
	class.Spec.IdentityMode = "userPassthrough"
	class.Spec.UserPreferences = []v1.UserPreferenceSchema{{Name: "goal_execution_enabled", Type: "bool", Default: &apiextv1.JSON{Raw: []byte(`true`)}}}
	class.Spec.Authz.ToolCalls = &v1.ToolCallsAuthz{Mode: "enforcing"}
	class.Spec.Authz.PlanGate = &v1.PlanGateConfig{Mode: "enforcing", RequirePlan: ptr.To(true), Rendering: &v1.PlanRendering{MaxAutoApproveHandles: 0}}
	channel := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "private", UID: "private-uid"}, Spec: v1.ChannelSpec{Kind: "browser"}}
	user := &v1.UserIdentity{ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(owner.Subject()), UID: "owner-catalog-uid"}, Spec: v1.UserIdentitySpec{Subject: owner.Subject().String()}}
	require.NoError(t, corev1.AddToScheme(f.s.Reader.(client.Client).Scheme()))
	k8s := fake.NewClientBuilder().WithScheme(f.s.Reader.(client.Client).Scheme()).WithObjects(source, class, channel, user).WithStatusSubresource(&v1.AgentSession{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if sess, ok := obj.(*v1.AgentSession); ok {
			sess.UID = types.UID("scheduled-session-uid")
			sess.CreationTimestamp = metav1.Now()
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	f.s.Reader = k8s
	f.proof(t, owner.String())
	actor, err := f.s.resolve(ctx, "team", "session")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	f.s.Service.Now = func() time.Time { return now.Add(-time.Minute) }
	g, err := f.s.Service.Create(ctx, actor, domain.CreateRequest{RequestID: "create", Title: "Private reminder", Outcome: "Remind me to stretch"})
	require.NoError(t, err)
	g, err = f.s.Service.Update(ctx, actor, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "activate", Action: "activate"})
	require.NoError(t, err)
	req := domain.ExecutionRequest{RequestID: "request-execution", ID: g.ID, Revision: g.Revision, Terms: domain.ExecutionTerms{DueAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 120}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"A private reminder delivery"}}}
	if unattended {
		req.Terms.ActionApproval = "standing_private"
	}
	if len(eventMode) > 1 && eventMode[1] {
		req.Terms.Bounds = domain.ExecutionBounds{}
	}
	if recurring {
		req.Terms.Schedule = &sessionschedule.Spec{Kind: "interval", Timezone: "UTC", IntervalSeconds: 60, MaxRuns: 2, RunWindowSeconds: 600}
	}
	if watch {
		eventStore = eventsql.New(db.DB(), false)
		require.NoError(t, eventStore.Migrate(ctx))
		access := &eventweb.NativeSessions{Reader: k8s, Memory: f.mem, Auth: f.auth}
		sources := sessionevents.NewRegistry()
		sources.Register(&eventnative.Adapter{Keys: f.keys, Authority: access, Access: access, Memory: f.mem, Publishers: map[string]bool{"system:channelsd": true}})
		execution := &domain.EventExecution{Service: f.s.Service, Sources: sources}
		triggers := &sessionevents.Triggers{Store: eventStore, Observations: eventStore, Authority: execution}
		execution.Triggers = triggers
		f.s.EventSources = sources
		f.s.Service.Events = execution
		eventRouter = &sessionevents.Router{Triggers: triggers, Store: eventStore}
		eventDispatcher = &sessionevents.Dispatcher{Triggers: triggers, Consumer: execution}
		eventHTTP = &eventweb.Server{Ingester: &sessionevents.Ingester{Store: eventStore, Adapters: sources}, Tokens: f.s.Tokens}
		req.Terms.Event = &domain.EventWatch{Source: sessionevents.Source{Kind: "native", Namespace: "team", ID: "team/session"}, Predicate: sessionevents.Predicate{Kind: "trip.changed", Subject: "trip-1"}, MaxRuns: 2, RunWindowSeconds: 600, Timezone: "UTC", Burst: "skip_pending"}
	}
	if inPlan {
		req.ApprovalMode = "plan"
		status, _ := f.call(t, "token", domain.Request{Operation: "request_execution", Resource: ResourceType + ":" + actor.Domain.ID(), Execution: req})
		require.Equal(t, http.StatusNotFound, status, "missing signing collaborators fail closed before goal mutation")
		unchanged, err := f.s.Service.Get(ctx, actor, g.ID)
		require.NoError(t, err)
		assert.Equal(t, g.Revision, unchanged.Revision)
		assert.Nil(t, unchanged.Execution)
	}
	require.NoError(t, f.s.PrepareExecution(ctx, actor, &req))
	validationGoal := g
	validationGoal.Execution = &domain.ExecutionConsent{Session: actor.Session, SessionUID: actor.SessionUID}
	require.NoError(t, f.s.Validate(ctx, validationGoal, req.Terms), "a class with no start gate must not check an empty permission")
	// Omitted limits become exact reviewed defaults; explicit oversized limits
	// remain invalid rather than being silently narrowed.
	defaultRequest := req
	defaultRequest.Terms.Bounds = domain.ExecutionBounds{}
	require.NoError(t, f.s.PrepareExecution(ctx, actor, &defaultRequest))
	assert.Equal(t, executionPolicy(class).DefaultBounds, defaultRequest.Terms.Bounds)
	require.NoError(t, f.s.Validate(ctx, validationGoal, defaultRequest.Terms))
	listStatus, listed := f.call(t, "token", domain.Request{Operation: "list"})
	require.Equal(t, http.StatusOK, listStatus)
	require.NotNil(t, listed.ExecutionPolicy)
	assert.Equal(t, defaultRequest.Terms.Bounds, listed.ExecutionPolicy.DefaultBounds)
	assert.False(t, listed.ExecutionPolicy.ServerTime.IsZero())
	oversizedTerms := req.Terms
	oversizedTerms.Bounds.Tokens = listed.ExecutionPolicy.MaxBounds.Tokens + 1
	err = f.s.Validate(ctx, validationGoal, oversizedTerms)
	require.ErrorIs(t, err, domain.ErrInvalid)
	require.ErrorContains(t, err, "bounds.tokens")
	assert.Equal(t, listed.ExecutionPolicy.MaxBounds.Tokens+1, oversizedTerms.Bounds.Tokens, "explicit limits are never silently clipped")
	for _, explicit := range []bool{false, true} {
		t.Run("restricted start gate explicit="+fmt.Sprint(explicit), func(t *testing.T) {
			restricted := &v1.AgentClass{}
			require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(class), restricted))
			restricted.Spec.Authz.Session = &v1.SessionAuthz{AllowedStarters: []string{owner.Subject().String()}, PlatformAdminsMayStart: ptr.To(!explicit)}
			require.NoError(t, k8s.Update(ctx, restricted))
			terms := req.Terms
			terms.ClassDigest, err = digest(restricted.Spec)
			require.NoError(t, err)
			f.auth.denyStart = true
			require.ErrorIs(t, f.s.Validate(ctx, validationGoal, terms), domain.ErrDenied)
			require.Equal(t, restricted.StartGatePermission(), f.auth.startPermission)
			f.auth.denyStart = false
			require.NoError(t, f.s.Validate(ctx, validationGoal, terms))
			require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(class), restricted))
			restricted.Spec = *class.Spec.DeepCopy()
			require.NoError(t, k8s.Update(ctx, restricted))
		})
	}
	unknownApproval := req
	unknownApproval.RequestID = "unknown-approval"
	unknownApproval.Terms.ActionApproval = "unlimited"
	_, err = f.s.Service.RequestExecution(ctx, actor, unknownApproval)
	require.ErrorIs(t, err, domain.ErrInvalid)

	// Separate create/request payloads can each fit ingress while the combined
	// signed consent is too large. Reject before mutation in either mode.
	large, err := f.s.Service.Create(ctx, actor, domain.CreateRequest{RequestID: "create-large", Title: "Large evidence", Outcome: strings.Repeat("\"\n", 8000)})
	require.NoError(t, err)
	large, err = f.s.Service.Update(ctx, actor, domain.Change{ID: large.ID, Revision: large.Revision, RequestID: "activate-large", Action: "activate"})
	require.NoError(t, err)
	oversized := req
	oversized.ID, oversized.Revision, oversized.RequestID = large.ID, large.Revision, "large-consent"
	oversized.Terms.Evidence = make([]string, 20)
	for n := range oversized.Terms.Evidence {
		oversized.Terms.Evidence[n] = strings.Repeat("e", 1000)
	}
	_, err = f.s.Service.RequestExecution(ctx, actor, oversized)
	require.ErrorIs(t, err, domain.ErrInvalid)
	unchanged, err := f.s.Service.Get(ctx, actor, large.ID)
	require.NoError(t, err)
	require.Equal(t, large.Revision, unchanged.Revision)
	require.Nil(t, unchanged.Execution)
	g, err = f.s.Service.RequestExecution(ctx, actor, req)
	require.NoError(t, err)
	changedApproval := req
	if unattended {
		changedApproval.Terms.ActionApproval = "manual"
	} else {
		changedApproval.Terms.ActionApproval = "standing_private"
	}
	_, err = f.s.Service.RequestExecution(ctx, actor, changedApproval)
	require.ErrorIs(t, err, domain.ErrConflict, "the same request cannot silently change its action approval policy")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer := provenance.NewSigner(priv, "system:operator")
	f.keys[provenance.PubKeyRef{Publisher: "system:operator", KeyID: signer.KeyID()}] = pub
	var card channelevents.InteractionRequestPayload
	relayOffline := true
	publisher := &ConsentPublisher{Service: f.s.Service, Memory: f.mem, Writer: provenance.NewSigningMemory(f.mem, signer), Publish: func(_ string, raw []byte) error {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(raw, &env))
		if relayOffline {
			return nil
		}
		if err := json.Unmarshal(env.Payload, &card); err != nil {
			return err
		}
		return parkedprompt.Note(memory.WithCaller(memory.WithSystemApproval(ctx, "system:channelsd"), "system:channelsd"), f.mem, memory.Scope{Kind: "session", ID: env.Session.Namespace + "/" + env.Session.Name}, parkedprompt.Content{RequestRef: card.RequestRef, Category: card.Category, Envelope: raw})
	}}
	firstPublish := publisher.Notify(ctx, domain.Event{Goal: g, Action: "request_execution"})
	if inPlan {
		require.NoError(t, firstPublish)
	} else {
		require.ErrorIs(t, firstPublish, errConsentDeliveryPending)
	}
	relayOffline = false
	require.NoError(t, publisher.Notify(ctx, domain.Event{Goal: g, Action: "request_execution"}))
	if inPlan {
		assert.Empty(t, card.RequestRef, "prepared requests must not publish a separate consent card")
		f.s.Consent = publisher
		status, response := f.call(t, "token", domain.Request{Operation: "request_execution", Resource: ResourceType + ":" + actor.Domain.ID(), Execution: req})
		require.Equal(t, http.StatusOK, status)
		require.NotNil(t, response.Approval)
		card = *response.Approval
		status, retry := f.call(t, "token", domain.Request{Operation: "request_execution", Resource: ResourceType + ":" + actor.Domain.ID(), Execution: req})
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, response.Approval, retry.Approval, "repeated full plan updates reuse the exact reviewed card")
	}

	if recurring {
		assert.Equal(t, "Allow scheduled private reminders?", card.Lead)
		assert.Contains(t, card.Fields[0].Value, "up to 2 runs")
		assert.Equal(t, "Limits per session", card.Fields[2].Label)
		require.Len(t, g.Execution.Terms.ScheduleWindows, 2)
	}
	if watch {
		assert.Equal(t, "Monitor this goal for changes?", card.Lead)
		assert.Contains(t, card.Fields[0].Value, "up to 2 sessions")
		assert.Equal(t, "session-uid", g.Execution.Terms.Event.Source.UID)
	}
	require.Equal(t, "goalconsent-request-"+g.Execution.Digest, card.RequestRef)
	if unattended {
		require.Contains(t, card.Body, "without another approval")
	} else {
		require.Contains(t, card.Body, "fresh plan")
	}
	require.NoError(t, card.Validate())
	require.NotContains(t, card.Body, g.Title, "untrusted goal content belongs in the inert excerpt")
	require.Contains(t, card.Excerpt.Content, g.Title)
	require.Contains(t, card.Excerpt.Content, req.Terms.Evidence[0])
	require.NotContains(t, card.Body, g.Domain.ClassUID, "internal pins remain in signed details")
	var reviewedGoal domain.Goal
	require.NoError(t, json.Unmarshal(card.Details, &reviewedGoal))
	require.Equal(t, g.Execution.Terms, reviewedGoal.Execution.Terms)
	require.ErrorIs(t, f.s.Service.Dispatchable(ctx, g), domain.ErrDenied, "no human decision")
	f.s.Tokens.SetChannelsdToken("channelsd")
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	p := &pipeline.Pipeline{Mem: f.signed, GoalConsentCommitter: consentHTTP{server: f.s, token: "channelsd"}}
	pipeline.BindGoalConsentHandler(p)
	handler, ok := channelinteractions.HandlerFor(categories.GoalExecutionConsent)
	require.True(t, ok)
	click := channelinteractions.Decision{Session: channelevents.SessionRef{Namespace: "team", Name: "session"}, Request: &card, Payload: channelevents.InteractionDecisionPayload{Category: categories.GoalExecutionConsent, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "email", Email: "alice@example.com"}}}
	for _, tc := range []struct {
		name   string
		mutate func(*channelinteractions.Decision)
	}{
		{"misleading body", func(d *channelinteractions.Decision) { d.Request.Body = "Authorize unlimited work" }},
		{"different actor", func(d *channelinteractions.Decision) { d.Payload.Decider.Email = "bob@example.com" }},
		{"no exact card", func(d *channelinteractions.Decision) { d.Request = nil }},
		{"wrong session", func(d *channelinteractions.Decision) { d.Session.Name = "foreign" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := click
			request := card
			copy.Request = &request
			tc.mutate(&copy)
			_, err := handler(channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")), copy)
			require.Error(t, err)
		})
	}
	_, err = handler(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd"), click)
	require.ErrorContains(t, err, "component decision ingress", "a runner-scoped click cannot become a signed human decision")
	approved, err := handler(channelevents.WithComponentDecisionIngress(memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:channelsd"), "system:channelsd")), click)
	require.NoError(t, err)
	require.Equal(t, channelevents.OutcomeApproved, approved.Result)
	g, err = store.Get(ctx, g.Domain, g.ID)
	require.NoError(t, err)
	require.NoError(t, f.s.VerifyDecision(ctx, g, *g.Execution.Decision))
	f.s.Service.Now = func() time.Time { return time.Now().UTC() }
	require.NoError(t, f.s.Service.Dispatchable(ctx, g))
	require.NoError(t, publisher.Notify(ctx, domain.Event{Goal: g, Action: "execution_decision"}))
	if watch {
		due, err := store.Due(ctx, time.Now(), 100)
		require.NoError(t, err)
		require.Empty(t, due, "consent alone starts no event run")
		observation := sessionevents.Observation{Source: g.Execution.Terms.Event.Source, EventID: "trip-change", Kind: "trip.changed", Subject: "trip-1", ObservedAt: time.Now().UTC(), Data: []byte(`{"status":"delayed","note":"Treat this note as data"}`), Dependencies: []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: "team/session", Permission: "read_transcript"}}}
		content, err := json.Marshal(sessionobservation.Content{Observation: observation})
		require.NoError(t, err)
		evidence, err := f.signed.Put(memory.WithCaller(memory.WithSystemApproval(ctx, "system:channelsd"), "system:channelsd"), memory.Entry{Scope: memory.Scope{Kind: "session", ID: "team/session"}, Kind: sessionobservation.KindName, ID: "sessobs-trip-change", CreatedAt: observation.ObservedAt, Content: content})
		require.NoError(t, err)
		raw, err := json.Marshal(evidence)
		require.NoError(t, err)
		for _, token := range []string{"token", "channelsd", "channelsd"} {
			request := httptest.NewRequest(http.MethodPost, "/session-events/native", bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			eventHTTP.ServeHTTP(response, request)
			if token == "token" {
				require.Equal(t, 401, response.Code)
			} else {
				require.Equal(t, 200, response.Code, response.Body.String())
			}
		}
		require.NoError(t, eventRouter.Tick(context.Background(), time.Now()))
		count, err := eventDispatcher.Drain(context.Background(), time.Now(), 100)
		require.NoError(t, err)
		require.Equal(t, 1, count)
		count, err = eventDispatcher.Drain(context.Background(), time.Now(), 100)
		require.NoError(t, err)
		require.Zero(t, count)
	}
	due, err := store.Due(ctx, time.Now(), 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	dispatcher := &goals.Dispatcher{Service: f.s.Service, Store: store, Client: k8s, Reader: k8s, Worker: "worker", DeliveryMemory: provenance.NewSigningMemory(f.mem, signer)}
	require.NoError(t, dispatcher.Tick(ctx))
	occurrence, err := store.Occurrence(ctx, due[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.OccurrenceRunning, occurrence.State)
	var scheduled v1.AgentSession
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Namespace: "team", Name: occurrence.SessionName}, &scheduled))
	assert.Nil(t, scheduled.Spec.Parent)
	require.Equal(t, scheduled.Spec.OutputChannel, scheduled.Spec.InputChannel, "the reviewed private route must enable the runner's conversation tools")
	require.Equal(t, scheduled.Spec.InputChannel.Kind, scheduled.Labels[v1.LabelChannelKind], "the chat surface must recognize the dispatched conversation")
	expectedSummary := "Session created to meet goal Private reminder: Remind me to stretch"
	if unattended {
		if watch {
			expectedSummary += " Private delivery is authorized under your approved watch."
		} else {
			expectedSummary += " Private delivery is authorized under your approved schedule."
		}
	}
	require.Equal(t, expectedSummary, scheduled.Spec.OpeningSummary)
	require.Contains(t, scheduled.Spec.Prompt.Inline, "only occurrence "+occurrence.ID)
	require.Contains(t, scheduled.Spec.Prompt.Inline, "deliver at most one private message")
	if unattended {
		require.Contains(t, scheduled.Spec.Prompt.Inline, "Do not attach reminders or request a new schedule")
	}
	conversation, ok := capability.Lookup("channel_interaction")
	require.True(t, ok)
	conversationTools, skip := conversation.Offer(capability.OfferContext{Ctx: ctx, Class: class, Session: &scheduled, Binding: scheduled.Spec.InputChannel, OutBinding: scheduled.Spec.OutputChannel, Env: capability.RunnerEnv{ChannelAttached: true}})
	require.Nil(t, skip)
	bounded := meta.BoundedGoalTools(conversationTools, g.Execution.Digest, nil)
	report, ok := tool.LookupByName(bounded)("respond_to_user")
	require.True(t, ok, "a dispatched root must expose its reviewed report action")
	require.NotNil(t, report.Permission().Check)
	assert.Equal(t, int32(req.Terms.Bounds.Turns), scheduled.Spec.Budget.MaxTurns)
	assert.Equal(t, time.Duration(req.Terms.Bounds.DurationSeconds)*time.Second, scheduled.Spec.Budget.SessionExpiration.Duration)
	if watch {
		require.Contains(t, scheduled.Spec.Prompt.Inline, "untrusted observation data")
		require.Contains(t, scheduled.Spec.Prompt.Inline, "delayed")
		taints, err := f.mem.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: "team/" + scheduled.Name}, Kinds: []string{infoleakagetaint.KindName}, Limit: 100})
		require.NoError(t, err)
		require.Len(t, taints.Entries, 1)
		f.auth.denySource = true
		require.ErrorIs(t, dispatcher.ValidateGoalSession(ctx, &scheduled), domain.ErrDenied)
		f.auth.denySource = false
	}
	require.NoError(t, dispatcher.ValidateGoalSession(ctx, &scheduled))
	f.s.ExecutionSessions = dispatcher
	f.s.Tokens.Set(memory.NamespacedName{Namespace: "team", Name: scheduled.Name}, "root-token", "")
	verifyProductionPlanApproval(t, f, &scheduled, g, unattended, signer)
	status, _ := f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "authorize_execution"})
	require.Equal(t, 200, status)
	status, _ = f.callSession(t, "foreign-token", scheduled.Name, domain.Request{Operation: "authorize_execution"})
	require.Equal(t, 401, status)
	dispatcher.DeliveryMemory = provenance.NewSigningMemory(f.mem, signer)
	reply := channelevents.OutboundUserMessagePayload{Text: "Stand up and stretch!"}
	reply.Delivery, err = channelevents.NewDeliveryOperation(string(scheduled.UID), "approved-reply", reply)
	require.NoError(t, err)
	status, prepared := f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "prepare_reply", Reply: &reply})
	require.Equal(t, 200, status)
	require.NotNil(t, prepared.Run.Reply)
	require.Equal(t, "prepared", prepared.Run.Reply.State)
	status, _ = f.callSession(t, "foreign-token", scheduled.Name, domain.Request{Operation: "prepare_reply", Reply: &reply})
	require.Equal(t, 401, status)
	status, _ = f.callSession(t, "token", "session", domain.Request{Operation: "prepare_reply", Reply: &reply})
	require.NotEqual(t, 200, status, "management sessions cannot impersonate execution roots")
	// The dispatch worker polls after its current lease; avoid a wall-clock
	// sleep while exercising that same lease boundary.
	dispatcher.Now = func() time.Time { return time.Now().UTC().Add(6 * time.Second) }
	require.NoError(t, dispatcher.Tick(ctx))
	status, accepted := f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "prepare_reply", Reply: &reply})
	require.Equal(t, 200, status)
	require.Equal(t, "accepted", accepted.Run.Reply.State)
	require.NoError(t, accepted.Run.Reply.Receipt.Validate(accepted.Run.Reply.Intent))
	proposal := domain.RunProposal{RequestID: "result", Status: "reported_success", Summary: "Private reminder published", Evidence: []string{"tool-call:reminder"}}
	status, result := f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "report_execution_result", Proposal: proposal})
	require.Equal(t, 200, status)
	require.NotNil(t, result.Run)
	require.Equal(t, occurrence.ID, result.Run.ID)
	require.Equal(t, proposal.Summary, result.Run.Proposal.Summary)
	// Caller fields never choose a domain or a run, and submittedAt is stamped by the server.
	status, _ = f.callSession(t, "token", "session", domain.Request{Operation: "report_execution_result", Proposal: proposal})
	require.NotEqual(t, 200, status, "a human management session is not an execution root")
	status, _ = f.callSession(t, "foreign-token", scheduled.Name, domain.Request{Operation: "report_execution_result", Proposal: proposal})
	require.Equal(t, 401, status)
	status, _ = f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "list"})
	require.Equal(t, 404, status, "report roots cannot gain management authority from an invented actor")
	// A confirmed user opt-out is checked again before each report.
	userScope, err := memory.UserScope(owner.String())
	require.NoError(t, err)
	preference, err := json.Marshal(userpreference.Preference{ClassNamespace: "team", ClassName: "assistant", Key: "goal_execution_enabled", Value: json.RawMessage(`false`)})
	require.NoError(t, err)
	entry := memory.Entry{Scope: userScope, Kind: userpreference.KindName, ID: userpreference.EntryID("team", "assistant", "goal_execution_enabled"), Content: preference, CreatedAt: time.Now().UTC()}
	_, err = f.mem.Put(ctx, entry)
	require.NoError(t, err)
	require.ErrorIs(t, dispatcher.ValidateGoalSession(ctx, &scheduled), domain.ErrDenied)
	status, _ = f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "authorize_plan", PlanApproval: &domain.PlanApprovalRequest{Digest: "revoked", Phase: 0}})
	require.Equal(t, 404, status, "a retained plan approval cannot revive revoked authority")
	status, _ = f.callSession(t, "root-token", scheduled.Name, domain.Request{Operation: "report_execution_result", Proposal: proposal})
	require.Equal(t, 404, status, "revocation is checked even for identical report retries")
	preference, err = json.Marshal(userpreference.Preference{ClassNamespace: "team", ClassName: "assistant", Key: "goal_execution_enabled", Value: json.RawMessage(`true`)})
	require.NoError(t, err)
	entry.Content = preference
	_, err = f.mem.Put(ctx, entry)
	require.NoError(t, err)
	require.NoError(t, dispatcher.ValidateGoalSession(ctx, &scheduled))
	// Account deactivation is authoritative, and does not depend on session logs.
	require.NoError(t, k8s.Get(ctx, client.ObjectKey{Name: user.Name}, user))
	user.Spec.Suspended = true
	require.NoError(t, k8s.Update(ctx, user))
	require.ErrorIs(t, dispatcher.ValidateGoalSession(ctx, &scheduled), domain.ErrDenied)
	// Cancellation's stale snapshot can never restore the approved revision.
	user.Spec.Suspended = false
	require.NoError(t, k8s.Update(ctx, user))
	_, err = f.s.Service.Update(ctx, actor, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "cancel", Action: "cancel"})
	require.NoError(t, err)
	require.ErrorIs(t, f.s.Service.Dispatchable(ctx, g), domain.ErrDenied)
	_, err = db.DB().ExecContext(ctx, `UPDATE oap_goal_occurrences SET lease_until=0 WHERE id=?`, occurrence.ID)
	require.NoError(t, err)
	dispatcher.Worker = "takeover"
	require.NoError(t, dispatcher.Tick(ctx))
	require.Error(t, k8s.Get(ctx, client.ObjectKey{Namespace: "team", Name: scheduled.Name}, &scheduled))
	_, err = db.DB().ExecContext(ctx, `UPDATE oap_goal_occurrences SET lease_until=0 WHERE id=?`, occurrence.ID)
	require.NoError(t, err)
	require.NoError(t, dispatcher.Tick(ctx))
	occurrence, err = store.Occurrence(ctx, occurrence.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.OccurrenceCancelled, occurrence.State)
	if recurring {
		history, err := store.Runs(ctx, g.Domain, g.ID, domain.ListRequest{Limit: 100})
		require.NoError(t, err)
		require.Len(t, history.Runs, 2)
		for _, run := range history.Runs {
			assert.Equal(t, domain.OccurrenceCancelled, run.State)
			require.NotNil(t, run.Outcome)
			assert.Equal(t, domain.RunCancelled, run.Outcome.Reason)
		}
	}
}

func TestDecisionRequiresComponentSignature(t *testing.T) {
	f := fixture(t)
	f.s.Service.ExecutionAuth = f.s
	g := domain.Goal{Execution: &domain.ExecutionConsent{Digest: "digest"}}
	for _, witness := range []string{`{}`, `{"kind":"goal_consent","provenance":{"publisher":"system:operator"}}`, `{"kind":"goal_consent","provenance":{"publisher":"system:channelsd"}}`} {
		t.Run(witness, func(t *testing.T) {
			require.Error(t, f.s.VerifyDecision(context.Background(), g, domain.ExecutionDecision{Witness: witness}))
		})
	}
}
