package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errConsentDeliveryPending = errors.New("consent notification awaiting durable relay receipt")

type ConsentPublisher struct {
	mu      sync.Mutex
	Service *domain.Service
	Memory  memory.Memory
	Writer  memory.Memory
	Publish channelevents.PublishFunc
}

// Notify is part of the durable goal outbox: publication failures leave the
// event pending. The exact card is signed and saved before it reaches the bus.
func (p *ConsentPublisher) Notify(ctx context.Context, event domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Service == nil || p.Service.Store == nil || p.Memory == nil || p.Writer == nil {
		return fmt.Errorf("goal consent collaborators unavailable")
	}
	if event.Action == "execution_decision" {
		g, err := p.Service.Store.Get(ctx, event.Goal.Domain, event.Goal.ID)
		if err != nil {
			return err
		}
		if g.Execution == nil || g.Revision != event.Goal.Revision || g.Execution.Decision == nil || !g.Execution.Decision.Approved || !time.Now().Before(g.Execution.Terms.ExpiresAt) {
			return nil
		}
		store, ok := p.Service.Store.(domain.OccurrenceStore)
		if !ok {
			return domain.ErrDenied
		}
		if err := p.Service.ExecutionAuth.VerifyDecision(ctx, g, *g.Execution.Decision); err != nil {
			return err
		}
		if err := p.Service.ExecutionAuth.Validate(ctx, g, g.Execution.Terms); err != nil {
			if errors.Is(err, domain.ErrDenied) {
				log.FromContext(ctx).Info("approved goal is no longer eligible for scheduling", "goal", g.ID, "error", err)
				return nil
			}
			return err
		}
		if err := p.Service.ExecutionAuth.AuthorizeDispatch(ctx, g); err != nil {
			return err
		}
		if g.Execution.Terms.Event != nil {
			if p.Service.Events == nil {
				return domain.ErrDenied
			}
			return p.Service.Events.Activate(ctx, g)
		}
		_, err = store.Schedule(ctx, g)
		return err
	}
	if event.Action != "request_execution" {
		return nil
	}
	g, err := p.Service.Store.Get(ctx, event.Goal.Domain, event.Goal.ID)
	if err != nil {
		return err
	}
	if g.Execution == nil || g.Revision != event.Goal.Revision || g.Execution.Decision != nil {
		return nil
	}
	c := g.Execution
	ns, name, ok := strings.Cut(c.Session, "/")
	if !ok {
		return domain.ErrInvalid
	}
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	id := "goalconsent-request-" + c.Digest
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	entry, found, err := goalconsent.Find(ctx, p.Memory, scope, id)
	if err != nil {
		return err
	}
	if !found {
		entry, err = consentEntry(g, time.Now().UTC())
		if err != nil {
			return err
		}
		entry, err = p.Writer.Put(ctx, entry)
		if err != nil {
			return err
		}
	}
	var content goalconsent.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil {
		return err
	}
	var request channelevents.InteractionRequestPayload
	if err := json.Unmarshal(content.Request, &request); err != nil {
		return err
	}
	if request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) {
		return nil
	}
	if c.ApprovalMode == "plan" {
		return nil
	}
	return p.publishRetainedRequest(ctx, scope, request)
}

// publishRetainedRequest keeps the outbox pending until the relay records the
// exact request. A successful bus publish alone can lose a card during restart.
func (p *ConsentPublisher) publishRetainedRequest(ctx context.Context, scope memory.Scope, request channelevents.InteractionRequestPayload) error {
	retained := func() (bool, error) {
		record, found, err := parkedprompt.Find(ctx, p.Memory, scope, request.RequestRef)
		if err != nil || !found {
			return false, err
		}
		var envelope channelevents.Envelope
		if err := json.Unmarshal(record.Envelope, &envelope); err != nil {
			return false, err
		}
		var recorded channelevents.InteractionRequestPayload
		if err := json.Unmarshal(envelope.Payload, &recorded); err != nil {
			return false, err
		}
		want, err := json.Marshal(request)
		if err != nil {
			return false, err
		}
		actual, err := json.Marshal(recorded)
		if err != nil {
			return false, err
		}
		if record.Category != request.Category || string(actual) != string(want) {
			return false, fmt.Errorf("%w: retained consent request mismatch", domain.ErrConflict)
		}
		return true, nil
	}
	if found, err := retained(); err != nil || found {
		return err
	}
	ns, name, ok := strings.Cut(scope.ID, "/")
	if !ok || p.Publish == nil {
		return domain.ErrDenied
	}
	if err := channelevents.PublishOut(p.Publish, ns, name, channelevents.KindInteractionRequest, request); err != nil {
		return err
	}
	if found, err := retained(); err != nil || found {
		return err
	}
	return errConsentDeliveryPending
}

// Request returns only the exact platform-signed card for this live session's
// request. The plan presenter cannot rewrite its authority or destination.
func (p *ConsentPublisher) Request(ctx context.Context, g domain.Goal) (*channelevents.InteractionRequestPayload, error) {
	if g.Execution == nil || g.Execution.ApprovalMode != "plan" || g.Execution.Decision != nil {
		return nil, domain.ErrConflict
	}
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	entry, found, err := goalconsent.Find(ctx, p.Memory, memory.Scope{Kind: "session", ID: g.Execution.Session}, "goalconsent-request-"+g.Execution.Digest)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, domain.ErrNotFound
	}
	var content goalconsent.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil {
		return nil, err
	}
	var request channelevents.InteractionRequestPayload
	if err := json.Unmarshal(content.Request, &request); err != nil {
		return nil, err
	}
	if request.ExpiresAt == nil {
		return nil, domain.ErrConflict
	}
	if !time.Now().Before(*request.ExpiresAt) {
		current, err := p.Service.Store.Get(ctx, g.Domain, g.ID)
		if err != nil {
			return nil, err
		}
		if current.Execution == nil || current.Execution.Digest != g.Execution.Digest || current.Execution.Decision == nil || !current.Execution.Decision.Approved {
			return nil, domain.ErrConflict
		}
	}
	return &request, nil
}

// consentEntry builds the exact reviewed card before committing execution intent.
// The retained record is copied into a signed decision witness. Bounding its
// encoded size leaves room for that copy and provenance under the ingress cap.
func consentEntry(g domain.Goal, now time.Time) (memory.Entry, error) {
	c := g.Execution
	if c == nil {
		return memory.Entry{}, domain.ErrInvalid
	}
	ns, name, ok := strings.Cut(c.Session, "/")
	if !ok {
		return memory.Entry{}, domain.ErrInvalid
	}
	scope := memory.Scope{Kind: "session", ID: c.Session}
	id := "goalconsent-request-" + c.Digest
	owner := identity.CanonicalFromTrusted(g.Domain.Owner, "verified goal domain owner")
	email := identity.DecodeForDisplay(owner.String())
	audience := channelevents.ExternalIdentity{Kind: "email", ExternalID: identity.RawExternalID(email), Email: identity.Email(email)}
	// The canonical addressee must round-trip; non-email channel identities
	// need their durable directory mapping before this path can support them.
	canonical, e := identity.FromExternal(identity.Kind(audience.Kind), "", "", identity.Email(email)).Canonical()
	if e != nil || canonical != owner {
		return memory.Entry{}, domain.ErrDenied
	}
	details, e := json.Marshal(g)
	if e != nil {
		return memory.Entry{}, e
	}
	expiry := now.UTC().Add(time.Duration(c.Terms.Bounds.ApprovalSeconds) * time.Second)
	if expiry.After(c.Terms.ExpiresAt) {
		expiry = c.Terms.ExpiresAt
	}
	evidence := make([]string, 0, len(c.Terms.Evidence))
	for _, item := range c.Terms.Evidence {
		evidence = append(evidence, fmt.Sprintf("• %s", item))
	}
	b := c.Terms.Bounds
	request := channelevents.InteractionRequestPayload{AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name}, Category: categories.GoalExecutionConsent, RequestRef: id,
		Lead: "Allow one private reminder?", Body: "Runs once at the time below. You’ll approve a fresh plan before it sends the report or reminder.",
		Fields: []channelevents.InteractionField{
			{Label: "Run once", Value: c.Terms.DueAt.UTC().Format("2 Jan 2006, 15:04:05 UTC")},
			{Label: "Authorization ends", Value: c.Terms.ExpiresAt.UTC().Format("2 Jan 2006, 15:04:05 UTC")},
			{Label: "Limits", Value: fmt.Sprintf("%d seconds · %d turns · %d tokens", b.DurationSeconds, b.Turns, b.Tokens)},
			{Label: "Permitted action", Value: "Send a private report or reminder (respond_to_user)"},
			{Label: "Private recipient", Value: email, Mentions: []channelevents.ExternalIdentity{audience}},
			{Label: "Agent", Value: g.Domain.Class},
			{Label: "Plan approval timeout", Value: fmt.Sprintf("%d seconds", b.ApprovalSeconds)},
		},
		Excerpt: &channelevents.InteractionExcerpt{Label: fmt.Sprintf("Goal · revision %d", g.Revision), Content: fmt.Sprintf("%s\n\nOutcome: %s\n\nRequired evidence:\n%s", g.Title, g.Outcome, strings.Join(evidence, "\n"))},
		Details: details, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &audience}, ExpiresAt: &expiry,
		Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Authorize", Kind: channelevents.ActionKindDecision}, {ID: "deny", Label: "Decline", Kind: channelevents.ActionKindDecision}}}
	if schedule := c.Terms.Schedule; schedule != nil {
		description, err := sessionschedule.Describe(*schedule)
		if err != nil {
			return memory.Entry{}, err
		}
		windows, err := c.Terms.ExecutionWindows()
		if err != nil {
			return memory.Entry{}, err
		}
		loc, err := time.LoadLocation(schedule.Timezone)
		if err != nil {
			return memory.Entry{}, err
		}
		request.Lead = "Allow scheduled private reminders?"
		request.Body = "Each session needs a fresh plan approval before sending. Quiet hours defer reminders; missed run windows are skipped."
		request.Fields[0] = channelevents.InteractionField{Label: "Schedule", Value: fmt.Sprintf("%s at %s · %s · up to %d runs", description, c.Terms.DueAt.In(loc).Format("15:04:05"), schedule.Timezone, schedule.MaxRuns)}
		request.Fields = append(request.Fields, channelevents.InteractionField{Label: "First run", Value: windows[0].DueAt.In(loc).Format("2 Jan 2006, 15:04:05 MST")},
			channelevents.InteractionField{Label: "Run window", Value: fmt.Sprintf("%s; ends sooner at quiet hours or authorization expiry", (time.Duration(schedule.RunWindowSeconds) * time.Second).String())},
			channelevents.InteractionField{Label: "Series limits", Value: fmt.Sprintf("%d planned sessions · at most %d turns and %d tokens total", len(windows), int64(len(windows))*b.Turns, int64(len(windows))*b.Tokens)})
		request.Fields[2].Label = "Limits per session"
		if len(schedule.QuietHours) > 0 {
			request.Fields = append(request.Fields, channelevents.InteractionField{Label: "Quiet hours", Value: sessionschedule.QuietDescription(*schedule)})
		}
	}
	if event := c.Terms.Event; event != nil {
		request.Lead = "Monitor this goal for changes?"
		request.Body = "Matching events create a private session. Each session needs a fresh plan approval. Quiet hours defer work within its original deadline; events arriving while a launch is pending are skipped."
		request.Fields[0] = channelevents.InteractionField{Label: "Watch", Value: fmt.Sprintf("%s · %s · up to %d sessions", event.Predicate.Kind, event.Predicate.Subject, event.MaxRuns)}
		request.Fields[2].Label = "Limits per session"
		request.Fields = append(request.Fields, channelevents.InteractionField{Label: "Source", Value: event.Source.ID}, channelevents.InteractionField{Label: "Event deadline", Value: fmt.Sprintf("%s after observation; authorization expiry may end it sooner", time.Duration(event.RunWindowSeconds)*time.Second)}, channelevents.InteractionField{Label: "Quiet hours", Value: sessionschedule.QuietDescription(sessionschedule.Spec{Timezone: event.Timezone, QuietHours: event.QuietHours})})
	}
	if c.Terms.ActionApproval == "standing_private" {
		request.Body = "Authorize private delivery without another approval for each run. Each fresh plan must stay within this watch or schedule, recipient, and limits. Quiet hours defer reminders; missed windows are skipped. You can pause or cancel the goal."
		request.Fields = append(request.Fields, channelevents.InteractionField{Label: "Action approval", Value: "Unattended private delivery only. Other actions require separate approval."})
	}
	if report := c.Terms.Report; report != nil {
		request.Lead = "Allow this private observation collector?"
		request.Body = "Runs within the schedule and limits above. Reports are private agent observations; they do not independently verify external facts. You can pause or cancel this goal."
		if c.Terms.ActionApproval == "standing_private" {
			request.Body += " Publish within this scope without another approval for each run."
		} else {
			request.Body += " Each run asks you to approve a fresh plan before publishing."
		}
		actions := []string{"Publish a private observation for " + report.Subject}
		for _, op := range c.Terms.AllowedOperations {
			if op == "respond_to_user" {
				actions = append(actions, "Send a private report to you")
			}
		}
		request.Fields[3].Value = strings.Join(actions, "; ")
		request.Fields = append(request.Fields, channelevents.InteractionField{Label: "Observation", Value: report.Kind + " · " + report.Subject})
		if c.Terms.ActionApproval == "standing_private" {
			request.Fields[len(request.Fields)-2].Value = "Unattended private reporting within these terms. Other actions require separate approval."
		}
	}
	raw, e := json.Marshal(request)
	if e != nil {
		return memory.Entry{}, e
	}
	content, e := json.Marshal(goalconsent.Content{Goal: g, Request: raw})
	if e != nil {
		return memory.Entry{}, e
	}
	entry := memory.Entry{Scope: scope, ID: id, Kind: goalconsent.KindName, CreatedAt: now.UTC().Truncate(time.Microsecond), Content: content}
	if err := checkConsentSize(entry.Content); err != nil {
		return memory.Entry{}, err
	}
	return entry, nil
}

const maxConsentContentBytes = 30000

func checkConsentSize(content []byte) error {
	if len(content) > maxConsentContentBytes {
		return fmt.Errorf("%w: consent evidence too large to approve; shorten the outcome or evidence", domain.ErrInvalid)
	}
	return nil
}

// CheckExecutionConsent prevents an oversized card from stranding an execution
// request after its revision and audit intent have already been committed.
func (s *Server) CheckExecutionConsent(_ context.Context, g domain.Goal) error {
	_, err := consentEntry(g, time.Now().UTC())
	return err
}
