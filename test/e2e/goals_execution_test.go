//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionobservation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Real envtest controllers + SpiceDB + signed consent + SQLite dispatch + real
// runner gates + browser receipts. The scripted model has no external access.
func TestGoalExecutionThroughRunnerAndPrivateReceipt(t *testing.T) {
	for _, eventDriven := range []bool{false, true} {
		name := "scheduled"
		if eventDriven {
			name = "event"
		}
		t.Run(name, func(t *testing.T) {
			h := Start(t, Options{AgentDir: "bronzethread/testdata/agent-goaldemo", WithGoalExecution: true, DefaultTimeout: 60 * time.Second})
			h.WaitForAgentClassValid("goaldemo", 30*time.Second)
			h.WaitForAuthzSchema(60 * time.Second)
			h.LLM.OnUserMessage("setup goal source").Reply(RespondToUser("Source ready"))
			h.LLM.OnToolResult("respond_to_user", AnyResult()).Reply(ToolUse("agent_work_complete", map[string]any{"summary": "source ready"}))
			h.SendUserMessage("setup goal source")
			h.ExpectAgentReply(Contains("Source ready"))
			ns, sourceName := h.SessionRef()
			h.WaitForSessionPhase(ns, sourceName, "Idle", h.DefaultTimeout())
			ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
			var source v1.AgentSession
			require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: sourceName}, &source))
			var class v1.AgentClass
			require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "goaldemo"}, &class))
			class.Spec.IdentityMode = "userPassthrough"
			class.Spec.UserPreferences = []v1.UserPreferenceSchema{{Name: "goal_execution_enabled", Type: "bool", Default: &apiextv1.JSON{Raw: []byte(`true`)}}}
			class.Spec.Authz.ToolCalls = &v1.ToolCallsAuthz{Mode: "enforcing"}
			class.Spec.Authz.PlanGate = &v1.PlanGateConfig{Mode: "enforcing", RequirePlan: ptr.To(true), Rendering: &v1.PlanRendering{MaxAutoApproveHandles: 0}}
			require.NoError(t, h.K8s.Update(ctx, &class))
			// Raw zero must survive the API default on this omitempty int.
			require.NoError(t, h.K8s.Patch(ctx, &class, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"authz":{"planGate":{"rendering":{"maxAutoApproveHandles":0}}}}}`))))
			h.WaitForAgentClassValid("goaldemo", 30*time.Second)
			owner := v1.StartedByCanonical(&source)
			user := &v1.UserIdentity{ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(owner.Subject())}, Spec: v1.UserIdentitySpec{Subject: owner.Subject().String()}}
			require.NoError(t, h.K8s.Get(ctx, client.ObjectKeyFromObject(user), user))
			channel := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "goal-private"}, Spec: v1.ChannelSpec{Kind: "browser", Role: "both", AgentClass: "goaldemo"}}
			require.NoError(t, h.K8s.Create(ctx, channel))
			source.Spec.OutputChannel = &v1.ChannelBinding{Name: channel.Name, Kind: "browser", Key: "private-goal-test", NATSSubjectPrefix: channelevents.SubjectPrefix(ns, sourceName), Capabilities: []string{"send_user_message"}}
			bindingPatch, err := json.Marshal(map[string]any{"spec": map[string]any{"outputChannel": source.Spec.OutputChannel}})
			require.NoError(t, err)
			require.NoError(t, h.K8s.Patch(ctx, &source, client.RawPatch(types.MergePatchType, bindingPatch)))
			c := h.runnerFactory.preferencesClientFor(&source)
			call := func(req domain.Request) domain.Response {
				t.Helper()
				out, err := c.Goals(ctx, ns, sourceName, req)
				require.NoError(t, err)
				return out
			}
			listed := call(domain.Request{Operation: "list"})
			created := call(domain.Request{Operation: "create", Resource: listed.Resource, Create: domain.CreateRequest{RequestID: "create", Title: "Private reminder", Outcome: "Remind me to stretch"}})
			activated := call(domain.Request{Operation: "update", Resource: listed.Resource, Change: domain.Change{ID: created.Goal.ID, Revision: created.Goal.Revision, RequestID: "activate", Action: "activate"}})
			now := time.Now().UTC()
			terms := domain.ExecutionTerms{ActionApproval: "standing_private", DueAt: now.Add(3 * time.Second), ExpiresAt: now.Add(5 * time.Minute), Bounds: domain.ExecutionBounds{DurationSeconds: 180, Turns: 20, Tokens: 20000, ApprovalSeconds: 90}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"Private browser delivery receipt"}}
			if eventDriven {
				terms.Event = &domain.EventWatch{Source: sessionevents.Source{Kind: "native", Namespace: ns, ID: ns + "/" + sourceName}, Predicate: sessionevents.Predicate{Kind: "trip.changed", Subject: "trip-1"}, MaxRuns: 1, RunWindowSeconds: 180, Timezone: "UTC", Burst: "skip_pending"}
			}
			execution := call(domain.Request{Operation: "request_execution", Resource: listed.Resource, Execution: domain.ExecutionRequest{RequestID: "execute", ID: activated.Goal.ID, Revision: activated.Goal.Revision, Terms: terms}})
			// The newly dispatched root freezes its own plan and gets a standing
			// derivation for that exact occurrence, never a broad tool grant.
			h.LLM.OnUserMessage("one bounded execution of private goal").Reply(ToolUse("update_plan", map[string]any{"name": "main", "items": []any{map[string]any{"id": "remind", "label": "Deliver reminder and report result", "status": "pending", "phase": "deliver"}}, "phases": []any{map[string]any{"id": "deliver", "label": "Deliver reminder", "why": "Approved private reminder", "permissions": []any{map[string]any{"handle": "perm:execute:agent_goal_execution", "why": "Deliver and record the reminder"}}}}}))
			h.LLM.OnToolResult("update_plan", AnyResult()).Reply(ToolUse("select_phase", map[string]any{"phase": "deliver"}))
			h.LLM.OnToolResult("select_phase", AnyResult()).Reply(RespondToUser("Stand up and stretch!"))
			h.LLM.OnToolResult("respond_to_user", AnyResult()).Reply(ToolUse("report_goal_result", map[string]any{"requestID": "result", "status": "reported_success", "summary": "Delivered private reminder", "evidence": []string{"private browser receipt"}}))
			h.LLM.OnToolResult("report_goal_result", AnyResult()).Reply(ToolUse("agent_work_complete", map[string]any{"summary": "reminder delivered"}))
			var card channelevents.InteractionRequestPayload
			require.Eventually(t, func() bool {
				entry, found, err := goalconsent.Find(ctx, h.memStore, memory.Scope{Kind: "session", ID: ns + "/" + sourceName}, "goalconsent-request-"+execution.Goal.Execution.Digest)
				if err != nil || !found {
					return false
				}
				var content goalconsent.Content
				require.NoError(t, json.Unmarshal(entry.Content, &content))
				require.NoError(t, json.Unmarshal(content.Request, &card))
				return true
			}, h.DefaultTimeout(), 25*time.Millisecond)
			handler, ok := channelinteractions.HandlerFor(categories.GoalExecutionConsent)
			require.True(t, ok)
			_, err = handler(channelevents.WithComponentDecisionIngress(ctx), channelinteractions.Decision{Session: channelevents.SessionRef{Namespace: ns, Name: sourceName}, Payload: channelevents.InteractionDecisionPayload{AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: sourceName}, Category: card.Category, RequestRef: card.RequestRef, ActionID: "approve", Decider: channelevents.ExternalIdentity{Kind: "email", Email: identity.Email(h.opts.DefaultUser), ExternalID: identity.RawExternalID(h.opts.DefaultUser)}}, Request: &card})
			require.NoError(t, err)
			if eventDriven {
				time.Sleep(time.Until(terms.DueAt))
				observation := sessionevents.Observation{Source: execution.Goal.Execution.Terms.Event.Source, EventID: "trip-changed", Kind: "trip.changed", Subject: "trip-1", ObservedAt: time.Now().UTC(), Data: []byte(`{"status":"delayed"}`), Dependencies: []sessionevents.Dependency{{ResourceType: "agentsession", ResourceID: ns + "/" + sourceName, Permission: "read_transcript"}}}
				raw, err := json.Marshal(sessionobservation.Content{Observation: observation})
				require.NoError(t, err)
				witness, err := h.goalActorSigned.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: ns + "/" + sourceName}, Kind: sessionobservation.KindName, ID: "sessobs-e2e-change", CreatedAt: observation.ObservedAt, Content: raw})
				require.NoError(t, err)
				raw, err = json.Marshal(witness)
				require.NoError(t, err)
				for n := 0; n < 2; n++ {
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.runnerFactory.prefsSrv.URL+"/session-events/native", bytes.NewReader(raw))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer preftok-channelsd")
					response, err := http.DefaultClient.Do(request)
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, response.StatusCode)
					require.NoError(t, response.Body.Close())
				}
			}
			var run domain.Occurrence
			finished := assert.Eventually(t, func() bool {
				page, err := h.goals.store.Runs(ctx, execution.Goal.Domain, execution.Goal.ID, domain.ListRequest{Limit: 100})
				if err != nil || len(page.Runs) != 1 {
					return false
				}
				run = page.Runs[0]
				return run.Reply != nil && run.Reply.State == "accepted" && run.Proposal != nil && run.Proposal.Status == "reported_success" && run.Cost != nil && run.Cost.Final
			}, h.DefaultTimeout(), 100*time.Millisecond, "goal must finish through a real runner, with accepted delivery and final accounting")
			if !finished {
				raw, _ := json.Marshal(run)
				t.Logf("unfinished run: %s", raw)
				t.FailNow()
			}
			require.NoError(t, run.Reply.Receipt.Validate(run.Reply.Intent))
			require.Equal(t, "Stand up and stretch!", run.Reply.Intent.Payload.Text)
			approvals, err := h.memStore.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: ns + "/" + run.SessionName}, Kinds: []string{(plangateaudit.Kind{}).Name()}})
			require.NoError(t, err)
			require.NotEmpty(t, approvals.Entries, "fresh occurrence plan must leave approval evidence")
			all, err := h.goals.store.Runs(ctx, execution.Goal.Domain, execution.Goal.ID, domain.ListRequest{Limit: 100})
			require.NoError(t, err)
			require.Len(t, all.Runs, 1)
			h.LLM.AssertAllRulesConsumed()
		})
	}
}
