package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"sync"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type ConsentPublisher struct {
	mu      sync.Mutex
	Service *domain.Service
	Memory  memory.Memory
	Signer  *provenance.Signer
	Publish channelevents.PublishFunc
}

// Notify is part of the durable goal outbox: publication failures leave the
// event pending. The exact card is signed and saved before it reaches the bus.
func (p *ConsentPublisher) Notify(ctx context.Context, event domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Service == nil || p.Service.Store == nil || p.Memory == nil || p.Signer == nil {
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
		owner := identity.CanonicalFromTrusted(g.Domain.Owner, "verified goal domain owner")
		email := identity.DecodeForDisplay(owner.String())
		audience := channelevents.ExternalIdentity{Kind: "email", ExternalID: identity.RawExternalID(email), Email: identity.Email(email)}
		// The canonical addressee must round-trip; non-email channel identities
		// need their durable directory mapping before this path can support them.
		canonical, e := identity.FromExternal(identity.Kind(audience.Kind), "", "", identity.Email(email)).Canonical()
		if e != nil || canonical != owner {
			return domain.ErrDenied
		}
		details, e := json.Marshal(g)
		if e != nil {
			return e
		}
		expiry := time.Now().UTC().Add(time.Duration(c.Terms.Bounds.ApprovalSeconds) * time.Second)
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
				return err
			}
			windows, err := c.Terms.ExecutionWindows()
			if err != nil {
				return err
			}
			loc, err := time.LoadLocation(schedule.Timezone)
			if err != nil {
				return err
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
		if c.Terms.ActionApproval == "standing_private" {
			request.Body = "Authorize private delivery without another approval for each run. Each fresh plan must stay within this schedule, recipient, and limits. Quiet hours defer reminders; missed windows are skipped. You can pause or cancel the goal."
			request.Fields = append(request.Fields, channelevents.InteractionField{Label: "Action approval", Value: "Unattended private delivery only. Other actions require separate approval."})
		}
		raw, e := json.Marshal(request)
		if e != nil {
			return e
		}
		content, e := json.Marshal(goalconsent.Content{Goal: g, Request: raw})
		if e != nil {
			return e
		}
		entry = memory.Entry{Scope: scope, ID: id, Kind: goalconsent.KindName, CreatedAt: time.Now().UTC().Truncate(time.Microsecond), Content: content}
		if e := p.Signer.EnsureSeeded(ctx, p.Memory, scope); e != nil {
			return e
		}
		if e := p.Signer.Sign(&entry); e != nil {
			return e
		}
		if _, e := p.Memory.Put(ctx, entry); e != nil {
			p.Signer.InvalidateSeed(scope)
			return e
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
	return channelevents.PublishOut(p.Publish, ns, name, channelevents.KindInteractionRequest, request)
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
