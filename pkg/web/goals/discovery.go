package goals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Discovery struct {
	Server   *Server
	Store    domain.DiscoveryStore
	Triggers *sessionevents.Triggers
	Sources  *sessionevents.Registry
	After    string
}

func (d *Discovery) now() time.Time {
	if d.Server.Service.Now != nil {
		return d.Server.Service.Now().UTC()
	}
	return time.Now().UTC()
}
func (d *Discovery) available() bool {
	return d != nil && d.Server != nil && d.Server.Service != nil && d.Store != nil && d.Triggers != nil && d.Sources != nil && d.Server.Consent != nil
}
func (d *Discovery) Request(ctx context.Context, a domain.Actor, r domain.DiscoveryRequest) (domain.DiscoveryPolicy, error) {
	if !d.available() {
		return domain.DiscoveryPolicy{}, domain.ErrDenied
	}
	if err := d.Server.Authorize(ctx, a, true); err != nil {
		return domain.DiscoveryPolicy{}, err
	}
	execution := domain.ExecutionRequest{Terms: r.Terms}
	if err := d.Server.PrepareExecution(ctx, a, &execution); err != nil {
		return domain.DiscoveryPolicy{}, err
	}
	r.Terms = execution.Terms
	now := d.now()
	if err := r.Validate(now, a.Domain.Owner); err != nil {
		return domain.DiscoveryPolicy{}, err
	}
	idHash, err := digest(struct {
		Domain                domain.Domain
		SessionUID, RequestID string
	}{a.Domain, a.SessionUID, r.RequestID})
	if err != nil {
		return domain.DiscoveryPolicy{}, err
	}
	id := "discoverypolicy-" + idHash
	// Existing exact requests keep their original timestamp and authority pins.
	prior, err := d.Store.DiscoveryPolicy(ctx, a.Domain, id)
	if err == nil {
		if !reflect.DeepEqual(prior.Request, r) {
			return prior, domain.ErrConflict
		}
		return prior, d.NotifyPolicy(ctx, prior)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return prior, err
	}
	sources, err := d.Server.Sources(ctx, a)
	if err != nil {
		return prior, err
	}
	g := domain.Goal{ID: id, Domain: a.Domain, Revision: 1, Title: r.Title, Outcome: r.Outcome, State: domain.Active, Sources: sources, OriginSession: a.Session, CreatedAt: now, UpdatedAt: now, Execution: &domain.ExecutionConsent{Purpose: "discovery_policy", Session: a.Session, SessionUID: a.SessionUID, RequestID: r.RequestID, RequestRevision: 1, Terms: r.Terms}}
	if err = d.Server.Validate(ctx, g, r.Terms); err != nil {
		return prior, err
	}
	p := domain.DiscoveryPolicy{ID: id, Actor: a, Request: r, Template: g}
	h, err := digest(p)
	if err != nil {
		return p, err
	}
	p.Template.Execution.Digest = h
	p, err = d.Store.CreateDiscoveryPolicy(ctx, p)
	if err != nil {
		return p, err
	}
	return p, d.NotifyPolicy(ctx, p)
}
func (d *Discovery) livePolicy(ctx context.Context, p domain.DiscoveryPolicy) error {
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	if !d.available() || p.Stopped || p.Decision == nil || !p.Decision.Approved || !d.now().Before(p.Template.Execution.Terms.ExpiresAt) {
		return sessionevents.ErrDenied
	}
	if err := d.Server.verifyConsent(ctx, p.Template, *p.Decision, "discovery_policy"); err != nil {
		return err
	}
	ns, name, ok := strings.Cut(p.Actor.Session, "/")
	if !ok {
		return sessionevents.ErrDenied
	}
	current, err := d.Server.resolve(ctx, ns, name)
	if err != nil {
		return err
	}
	if current.Domain != p.Actor.Domain || current.SessionUID != p.Actor.SessionUID {
		return sessionevents.ErrDenied
	}
	if err = d.Server.Validate(ctx, p.Template, p.Template.Execution.Terms); err != nil {
		return err
	}
	return d.Sources.Check(ctx, p.Template.Domain.Owner, p.Template.Execution.Terms.Event.Source, nil)
}
func (d *Discovery) policyFor(ctx context.Context, s sessionevents.Subscription) (domain.DiscoveryPolicy, error) {
	if !d.available() {
		return domain.DiscoveryPolicy{}, sessionevents.ErrDenied
	}
	var target struct {
		Domain domain.Domain `json:"domain"`
		GoalID string        `json:"goalID"`
	}
	if err := json.Unmarshal([]byte(s.Target), &target); err != nil {
		return domain.DiscoveryPolicy{}, sessionevents.ErrDenied
	}
	p, err := d.Store.DiscoveryPolicy(ctx, target.Domain, target.GoalID)
	if err != nil {
		return p, err
	}
	expected, err := domain.DiscoverySubscription(p)
	if err != nil || !reflect.DeepEqual(expected, s) {
		return p, sessionevents.ErrDenied
	}
	return p, nil
}
func discoveryDenied(err error) error {
	if errors.Is(err, domain.ErrDenied) || errors.Is(err, domain.ErrNotFound) {
		return errors.Join(sessionevents.ErrDenied, err)
	}
	return err
}
func (d *Discovery) CheckSubscription(ctx context.Context, s sessionevents.Subscription) error {
	p, err := d.policyFor(ctx, s)
	if err != nil {
		return discoveryDenied(err)
	}
	return discoveryDenied(d.livePolicy(ctx, p))
}
func (d *Discovery) CheckObservation(ctx context.Context, s sessionevents.Subscription, o sessionevents.Observation) error {
	if o.Source != s.Source || o.Validate() != nil {
		return sessionevents.ErrDenied
	}
	if err := d.CheckSubscription(ctx, s); err != nil {
		return err
	}
	return discoveryDenied(d.Sources.Check(ctx, s.Principal, s.Source, o.Dependencies))
}
func (d *Discovery) Materialize(ctx context.Context, l sessionevents.Launch) error {
	if !d.available() {
		return sessionevents.ErrDenied
	}
	if err := d.Triggers.CheckLaunch(ctx, l, d.now()); err != nil {
		return err
	}
	p, err := d.policyFor(ctx, l.Subscription)
	if err != nil {
		return err
	}
	q, err := domain.NewDiscoveryProposal(p, l, d.now())
	if errors.Is(err, domain.ErrInvalid) {
		log.FromContext(ctx).Info("discovery observation does not contain a valid reviewed subject", "policy", p.ID, "observation", l.Observation.ID(), "error", err)
		return nil
	}
	if err != nil {
		return err
	}
	_, err = d.Store.CreateDiscoveryProposal(ctx, p, q)
	return err
}
func (d *Discovery) NotifyPolicy(ctx context.Context, p domain.DiscoveryPolicy) error {
	if p.Decision != nil || p.Stopped {
		return nil
	}
	notified, err := d.Store.DiscoveryPolicyNotification(ctx, p.ID)
	if err != nil || notified {
		return err
	}
	expiry := p.Template.CreatedAt.Add(time.Duration(p.Template.Execution.Terms.Bounds.ApprovalSeconds) * time.Second)
	if expiry.After(p.Template.Execution.Terms.ExpiresAt) {
		expiry = p.Template.Execution.Terms.ExpiresAt
	}
	fields := []channelevents.InteractionField{{Label: "Scope", Value: p.Request.Predicate.Kind + " · " + p.Request.Predicate.Subject}, {Label: "Source", Value: p.Template.Execution.Terms.Event.Source.ID}, {Label: "Limits", Value: fmt.Sprintf("Up to %d questions · %d pending at once", p.Request.MaxProposals, p.Request.MaxPending)}, {Label: "Until", Value: p.Template.Execution.Terms.ExpiresAt.Format("2 Jan 2006, 15:04 MST")}}
	if err = d.publish(ctx, p.Template, "discovery_policy", p, expiry, "Suggest goals from these changes?", "Allow private monitoring suggestions within this scope. Each suggestion needs your approval before a goal or watch is created. This does not authorize execution.", fields); err != nil {
		if errors.Is(err, errConsentDeliveryPending) {
			return nil
		}
		return err
	}
	return d.Store.DiscoveryPolicyNotified(ctx, p.ID)
}
func (d *Discovery) NotifyProposal(ctx context.Context, q domain.DiscoveryProposal) error {
	p, err := d.Store.DiscoveryPolicy(ctx, q.Goal.Domain, q.PolicyID)
	if err != nil {
		return err
	}
	if err = d.livePolicy(ctx, p); err != nil {
		return err
	}
	if err = d.Sources.Check(ctx, q.Goal.Domain.Owner, q.Observation.Source, q.Observation.Dependencies); err != nil {
		return err
	}
	if err = d.Server.Validate(ctx, q.Goal, q.Goal.Execution.Terms); err != nil {
		return err
	}
	watch := q.Goal.Execution.Terms.Event
	fields := []channelevents.InteractionField{{Label: "Monitor", Value: watch.Predicate.Subject}, {Label: "Until", Value: q.Goal.Execution.Terms.ExpiresAt.Format("2 Jan 2006, 15:04 MST")}, {Label: "Limits", Value: fmt.Sprintf("Up to %d private sessions", watch.MaxRuns)}}
	body := "Approval creates this goal and its finite watch. Each run needs your plan approval."
	if q.Goal.Execution.Terms.ActionApproval == "standing_private" {
		body = "Approval creates this goal and permits unattended private reports within the exact watch, recipient and limits. Other actions need separate approval."
	}
	return d.publish(ctx, q.Goal, "discovery_proposal", q, q.ExpiresAt, "Shall I monitor this for you?", body, fields)
}
func (d *Discovery) publish(ctx context.Context, g domain.Goal, purpose string, data any, expires time.Time, lead, body string, fields []channelevents.InteractionField) error {
	if !d.available() {
		return domain.ErrDenied
	}
	if !d.now().Before(expires) {
		return domain.ErrDenied
	}
	publisher := d.Server.Consent
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.Memory == nil || publisher.Signer == nil || publisher.Publish == nil {
		return domain.ErrDenied
	}
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	scope := memory.Scope{Kind: "session", ID: g.Execution.Session}
	id := "goalconsent-request-" + g.Execution.Digest
	entry, found, err := goalconsent.Find(ctx, publisher.Memory, scope, id)
	if err != nil {
		return err
	}
	if !found {
		ns, name, ok := strings.Cut(g.Execution.Session, "/")
		if !ok {
			return domain.ErrDenied
		}
		email := identity.DecodeForDisplay(g.Domain.Owner)
		audience := channelevents.ExternalIdentity{Kind: "email", ExternalID: identity.RawExternalID(email), Email: identity.Email(email)}
		owner, err := audience.Principal().Canonical()
		if err != nil || owner.String() != g.Domain.Owner {
			return domain.ErrDenied
		}
		detail, err := json.Marshal(struct {
			Goal      domain.Goal `json:"goal"`
			Authority any         `json:"authority"`
		}{g, data})
		if err != nil {
			return err
		}
		// Bound signed decision size rather than creating an unapprovable request.
		if len(detail) > 48000 {
			return fmt.Errorf("%w: discovery evidence too large", domain.ErrInvalid)
		}
		request := channelevents.InteractionRequestPayload{AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name}, Category: categories.GoalExecutionConsent, RequestRef: id, Lead: lead, Body: body, Fields: fields, Excerpt: &channelevents.InteractionExcerpt{Label: g.Title, Content: g.Outcome}, Details: detail, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &audience}, ExpiresAt: &expires, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Allow", Kind: channelevents.ActionKindDecision}, {ID: "deny", Label: "Decline", Kind: channelevents.ActionKindDecision}}}
		requestRaw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		extra, err := json.Marshal(data)
		if err != nil {
			return err
		}
		content, err := json.Marshal(goalconsent.Content{Purpose: purpose, Data: extra, Goal: g, Request: requestRaw})
		if err != nil {
			return err
		}
		if len(content) > 30000 {
			return fmt.Errorf("%w: discovery consent too large", domain.ErrInvalid)
		}
		entry = memory.Entry{Scope: scope, ID: id, Kind: goalconsent.KindName, CreatedAt: d.now().Truncate(time.Microsecond), Content: content}
		if err = publisher.Signer.EnsureSeeded(ctx, publisher.Memory, scope); err != nil {
			return err
		}
		if err = publisher.Signer.Sign(&entry); err != nil {
			return err
		}
		entry, err = publisher.Memory.Put(ctx, entry)
		if err != nil {
			publisher.Signer.InvalidateSeed(scope)
			return err
		}
	}
	var content goalconsent.Content
	if err = json.Unmarshal(entry.Content, &content); err != nil {
		return err
	}
	var request channelevents.InteractionRequestPayload
	if err = json.Unmarshal(content.Request, &request); err != nil {
		return err
	}
	return publisher.publishRetainedRequest(ctx, scope, request)
}
func (d *Discovery) Decide(ctx context.Context, content goalconsent.Content, decision domain.ExecutionDecision) error {
	if !d.available() {
		return domain.ErrDenied
	}
	switch content.Purpose {
	case "discovery_policy":
		p, err := d.Store.DiscoveryPolicy(ctx, content.Goal.Domain, content.Goal.ID)
		if err != nil {
			return err
		}
		original := p
		original.Decision = nil
		original.Stopped = false
		extra, err := json.Marshal(original)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(content.Goal, p.Template) || !bytes.Equal(extra, content.Data) {
			return domain.ErrDenied
		}
		if err = d.Server.verifyConsent(ctx, p.Template, decision, "discovery_policy"); err != nil {
			return err
		}
		if decision.Approved {
			if err = d.Server.Validate(ctx, p.Template, p.Template.Execution.Terms); err != nil {
				return err
			}
		}
		p, err = d.Store.DecideDiscoveryPolicy(ctx, p, decision)
		if err != nil {
			return err
		}
		if decision.Approved {
			if err = d.livePolicy(ctx, p); err != nil {
				return err
			}
			sub, err := domain.DiscoverySubscription(p)
			if err != nil {
				return err
			}
			_, err = d.Triggers.Activate(ctx, sub)
			return err
		}
		return nil
	case "discovery_proposal":
		if content.Goal.Discovery == nil {
			return domain.ErrDenied
		}
		q, err := d.Store.DiscoveryProposal(ctx, content.Goal.Domain, content.Goal.Discovery.ProposalID)
		if err != nil {
			return err
		}
		original := q
		original.Decision = nil
		original.State = "pending"
		extra, err := json.Marshal(original)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(q.Goal, content.Goal) || !bytes.Equal(extra, content.Data) {
			return domain.ErrDenied
		}
		if err = d.Server.VerifyDecision(ctx, q.Goal, decision); err != nil {
			return err
		}
		if decision.Approved {
			p, err := d.Store.DiscoveryPolicy(ctx, q.Goal.Domain, q.PolicyID)
			if err != nil {
				return err
			}
			if err = d.livePolicy(ctx, p); err != nil {
				return err
			}
			if err = d.Sources.Check(ctx, q.Goal.Domain.Owner, q.Observation.Source, q.Observation.Dependencies); err != nil {
				return err
			}
			if err = d.Server.Validate(ctx, q.Goal, q.Goal.Execution.Terms); err != nil {
				return err
			}
		}
		_, err = d.Store.DecideDiscoveryProposal(ctx, q, decision, d.now())
		if err != nil || !decision.Approved {
			return err
		}
		accepted, err := d.Server.Service.Store.Get(ctx, q.Goal.Domain, q.Goal.ID)
		if err != nil {
			return err
		}
		return d.Server.EnsureDispatchGrant(ctx, accepted)
	default:
		return domain.ErrDenied
	}
}

// Start rehydrates approved policies and pending notices after every restart.
func (d *Discovery) Start(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := d.Tick(ctx); err != nil {
			log.FromContext(ctx).Info("goal discovery reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (d *Discovery) NeedLeaderElection() bool { return true }
func (d *Discovery) Tick(ctx context.Context) error {
	if !d.available() {
		return domain.ErrDenied
	}
	ctx = memory.WithCaller(memory.WithSystemApproval(ctx, "system:operator"), "system:operator")
	policies, err := d.Store.ActiveDiscoveryPolicies(ctx, d.After, 100)
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		d.After = ""
	}
	var failures []error
	for _, p := range policies {
		d.After = p.ID
		if p.Decision == nil {
			if d.now().Before(p.Template.CreatedAt.Add(time.Duration(p.Template.Execution.Terms.Bounds.ApprovalSeconds) * time.Second)) {
				if err = d.NotifyPolicy(ctx, p); err != nil {
					failures = append(failures, err)
				}
			}
			continue
		}
		if err = d.livePolicy(ctx, p); err != nil {
			if errors.Is(discoveryDenied(err), sessionevents.ErrDenied) {
				if stopErr := d.Store.StopDiscoveryPolicy(ctx, p.Template.Domain, p.ID); stopErr != nil {
					failures = append(failures, stopErr)
				}
			} else {
				failures = append(failures, err)
			}
			continue
		}
		sub, err := domain.DiscoverySubscription(p)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err = d.Triggers.Activate(ctx, sub); err != nil {
			failures = append(failures, err)
		}
	}
	pending, err := d.Store.PendingDiscovery(ctx, d.now(), 100)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, q := range pending {
		if err = d.NotifyProposal(ctx, q); err != nil {
			if errors.Is(err, errConsentDeliveryPending) {
				continue
			}
			failures = append(failures, fmt.Errorf("notify proposal %s: %w", q.ID, err))
			if errors.Is(discoveryDenied(err), sessionevents.ErrDenied) || errors.Is(err, domain.ErrInvalid) {
				if markErr := d.Store.DiscoveryNotified(ctx, q.ID); markErr != nil {
					failures = append(failures, markErr)
				}
			}
			continue
		}
		if err = d.Store.DiscoveryNotified(ctx, q.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
