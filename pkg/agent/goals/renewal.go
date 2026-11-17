package goals

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
)

// SystemActor authors what no person or session did: a renewal request.
const SystemActor = "system:operator"

// BlockedApprovalExpired is the derived BlockedReason of a group goal whose
// approval has run out with no renewal approved.
const BlockedApprovalExpired = "approval expired"

// maxRenewalSpan caps a renewed approval like a group consent (validate).
const maxRenewalSpan = 90 * 24 * time.Hour

// RenewalHost is the session a renewal card is posted in: the group run being
// dispatched when the renewal came due. A system-authored request has no
// requesting session, so the hosting run session takes that place in the
// consent (ExecutionConsent.Session/SessionUID): it is where the card is
// retained, where the click lands, and what verifyConsent scopes the signed
// request and decision to.
type RenewalHost struct {
	Session    string
	SessionUID string
}

// RenewalRequestID is the one request id a renewal of the approval with this
// digest gets, which makes the request idempotent per expiring approval.
func RenewalRequestID(digest string) string { return "renewal-" + digest }

// IsRenewal reports whether c renews another approval (Renews names it).
func (c *ExecutionConsent) IsRenewal() bool {
	return c != nil && c.Renews != ""
}

// SystemRenewal reports whether c is the operator's renewal, asked for from
// a run, rather than one a manager asked for in their own conversation.
func (c *ExecutionConsent) SystemRenewal() bool {
	return c.IsRenewal() && c.RequestID == RenewalRequestID(c.Renews)
}

// RenewalPending reports whether g's approval in force already has a renewal,
// pending or decided: the operator asks for one per approval. (A manager may
// still ask again, which replaces an undecided or declined one.)
func RenewalPending(g Goal) bool {
	return g.Execution != nil && g.PendingRenewal != nil && g.PendingRenewal.Renews == g.Execution.Digest
}

// ApprovalInForce reports whether g has an approval runs may still dispatch
// under at now: decided, approved, bound to the current revision, unexpired.
func ApprovalInForce(g Goal, now time.Time) bool {
	c := g.Execution
	return g.State == Active && c != nil && c.Decision != nil && c.Decision.Approved && g.Revision == c.RequestRevision+1 && now.Before(c.Terms.ExpiresAt)
}

// RenewedTerms are t moved forward to start at the next nominal run after
// now, for the same length of time (capped at 90 days from now), with every
// other term unchanged. Windows are left for the request to resolve. Only a
// scheduled approval has a next run; a one-time one is ErrInvalid.
func RenewedTerms(t ExecutionTerms, now time.Time) (ExecutionTerms, error) {
	if t.Schedule == nil {
		return ExecutionTerms{}, fmt.Errorf("%w: only a scheduled approval can be renewed", ErrInvalid)
	}
	due, ok, err := sessionschedule.NextNominal(*t.Schedule, t.DueAt, now)
	if err != nil {
		return ExecutionTerms{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !ok {
		return ExecutionTerms{}, fmt.Errorf("%w: the schedule has no run after %s", ErrInvalid, now.UTC().Format(time.RFC3339))
	}
	span := t.ExpiresAt.Sub(t.DueAt)
	t.DueAt = due.UTC()
	t.ExpiresAt = t.DueAt.Add(span)
	if limit := now.UTC().Add(maxRenewalSpan); t.ExpiresAt.After(limit) {
		t.ExpiresAt = limit
	}
	t.ScheduleWindows = nil
	return t, nil
}

// RenewalView is g as its pending renewal's card shows it: the renewal in
// Execution. A renewal's card is built from it, and its decision verified
// and validated against it. g must have a PendingRenewal.
//
// The approval it renews is in PriorExecution, as it will be once approved.
func RenewalView(g Goal) Goal {
	view := g
	p := *g.PendingRenewal
	view.Execution, view.PriorExecution, view.PendingRenewal = &p, g.Execution, nil
	return view
}

// ForConsent returns g as it applies to work bound to the consent with this
// digest: with that consent in Execution. That is the approval in force, or
// the approval a renewal replaced (PriorExecution), which keeps governing the
// runs it scheduled until it runs out. ok is false when g holds neither.
// Every check of an occurrence against its goal goes through it.
func (g Goal) ForConsent(digest string) (Goal, bool) {
	if g.Execution != nil && g.Execution.Digest == digest {
		return g, true
	}
	if g.Execution != nil && g.PriorExecution != nil && g.PriorExecution.Digest == digest {
		view := g
		view.Execution = g.PriorExecution
		return view, true
	}
	return g, false
}

// pendingRevision is the goal revision an undecided consent c is decided at.
// A request bumps the revision when made, so it is decided at its own
// RequestRevision. A renewal is made and decided in place, at the revision of
// the approval it renews, whose RequestRevision it carries.
func pendingRevision(c *ExecutionConsent, renewal bool) int64 {
	if renewal {
		return c.RequestRevision + 1
	}
	return c.RequestRevision
}

// consentFor finds the consent digest names on g — Execution, or a pending
// renewal — with the goal view its decision is checked against.
func consentFor(g Goal, digest string) (c *ExecutionConsent, view Goal, renewal bool) {
	if g.Execution != nil && g.Execution.Digest == digest {
		return g.Execution, g, false
	}
	if g.PendingRenewal != nil && g.PendingRenewal.Digest == digest {
		view = RenewalView(g)
		return view.Execution, view, true
	}
	return nil, g, false
}

// RequestRenewal records a system-authored renewal of group goal id's
// approval in force (see renew), hosted in the run session host. Nobody is
// asked for at request time; the deciding manager's start gate is checked at
// decision. A renewal already requested for the approval in force, by
// anyone, is ErrConflict.
//
// It is an operator-internal path (the dispatcher's), not reachable from a
// session: it takes no Actor and audits SystemActor as the author.
func (s *Service) RequestRenewal(ctx context.Context, d Domain, id string, host RenewalHost, now time.Time) (Goal, error) {
	if s.ExecutionAuth == nil || s.Auth == nil || s.validateDomain(d) != nil || !d.GroupOwned() {
		return Goal{}, ErrDenied
	}
	now = now.UTC()
	g, err := s.Store.Get(ctx, d, id)
	if err != nil {
		return Goal{}, err
	}
	if !ApprovalInForce(g, now) {
		return Goal{}, fmt.Errorf("%w: only an approval in force can be renewed", ErrInvalid)
	}
	if RenewalPending(g) {
		return Goal{}, fmt.Errorf("%w: a renewal of this approval was already requested", ErrConflict)
	}
	digest := g.Execution.Digest
	requestID := RenewalRequestID(digest)
	h, err := requestHash(struct{ Action, ID, Renews string }{"request_renewal", id, digest})
	if err != nil {
		return Goal{}, err
	}
	if prior, ok, err := s.Store.Receipt(ctx, d, requestID, h); err != nil || ok {
		return prior, err
	}
	// The pins the terms carry must still hold; the decider is checked later.
	return s.renew(ctx, g, host, now, requestID, h, func(terms ExecutionTerms) error { return s.validateDispatch(ctx, g, terms) },
		Event{Action: "request_renewal", Actor: SystemActor, Proof: SystemActor})
}

// RenewExecution is a manager's own request to renew group goal r.ID's
// approval in force — request_goal_execution naming no terms while an
// approval is in force. It is renew hosted in the manager's own session, so
// the card appears where they asked; the approval in force is untouched until
// a manager approves it, exactly as for the operator's renewal. It replaces a
// pending renewal nobody approved (whose card may have lapsed). The start
// gate is checked for the requester, as for any request.
func (s *Service) RenewExecution(ctx context.Context, a Actor, r ExecutionRequest) (Goal, error) {
	if err := s.authorize(ctx, a, true); err != nil {
		return Goal{}, err
	}
	if s.ExecutionAuth == nil || !a.Domain.GroupOwned() {
		return Goal{}, ErrDenied
	}
	if !validRequestID(r.RequestID) || r.Revision < 1 || !reflect.DeepEqual(r.Terms, ExecutionTerms{}) || r.ApprovalMode != "" {
		return Goal{}, ErrInvalid
	}
	h, err := requestHash(struct {
		Action  string
		Request ExecutionRequest
	}{"renew_execution", r})
	if err != nil {
		return Goal{}, err
	}
	if prior, ok, err := s.Store.Receipt(ctx, a.Domain, r.RequestID, h); err != nil || ok {
		if err == nil {
			err = s.Auth.ReadGoal(ctx, a, prior)
		}
		return prior, err
	}
	g, err := s.get(ctx, a, r.ID)
	if err != nil {
		return Goal{}, err
	}
	if g.Revision != r.Revision {
		return Goal{}, ErrConflict
	}
	if err := s.requireGroupManager(ctx, a, g, "renew_execution"); err != nil {
		return Goal{}, err
	}
	now := s.now()
	if !ApprovalInForce(g, now) {
		return Goal{}, fmt.Errorf("%w: only an approval in force can be renewed", ErrInvalid)
	}
	accepted, err := s.renew(ctx, g, RenewalHost{Session: a.Session, SessionUID: a.SessionUID}, now, r.RequestID, h,
		func(terms ExecutionTerms) error { return s.validateExecution(ctx, g, terms, a.User) },
		Event{Action: "request_renewal", Session: a.Session, Proof: a.Proof, ActorProof: json.RawMessage(a.Attestation)})
	if err != nil {
		return Goal{}, err
	}
	if err := s.Auth.ReadGoal(ctx, a, accepted); err != nil {
		return Goal{}, err
	}
	return accepted, nil
}

// renew records, in place, a renewal of g's approval in force: the same terms
// with a fresh DueAt (the next nominal run after now) and ExpiresAt (the same
// window length, capped at 90 days), hosted in host and awaiting a manager's
// decision (DecideExecution), which makes it the approval in force. check
// runs the authority's rules on the renewed terms. event is the audit event,
// completed here.
func (s *Service) renew(ctx context.Context, g Goal, host RenewalHost, now time.Time, requestID, h string, check func(ExecutionTerms) error, event Event) (Goal, error) {
	store, ok := s.Store.(InPlaceStore)
	if !ok {
		return Goal{}, fmt.Errorf("%w: renewals are not stored here", ErrDenied)
	}
	ns, name, ok := strings.Cut(host.Session, "/")
	if !ok || ns != g.Domain.Namespace || name == "" || host.SessionUID == "" {
		return Goal{}, fmt.Errorf("%w: a renewal needs the session that hosts it", ErrInvalid)
	}
	c := g.Execution
	terms, err := RenewedTerms(c.Terms, now)
	if err != nil {
		return Goal{}, err
	}
	windows, err := sessionschedule.Resolve(*terms.Schedule, terms.DueAt, terms.ExpiresAt)
	if err != nil {
		return Goal{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	terms.ScheduleWindows = windows
	if err := terms.validate(now, g.Domain.Owner); err != nil {
		return Goal{}, err
	}
	if err := check(terms); err != nil {
		return Goal{}, err
	}
	reviewed := g
	reviewed.Execution, reviewed.PendingRenewal, reviewed.PriorExecution, reviewed.BlockedReason = nil, nil, nil, ""
	digest, err := requestHash(struct {
		Goal         Goal
		Session      string
		SessionUID   string
		Terms        ExecutionTerms
		ApprovalMode string
		Renews       string
	}{reviewed, host.Session, host.SessionUID, terms, "", c.Digest})
	if err != nil {
		return Goal{}, err
	}
	prior, err := EncodePrior(g)
	if err != nil {
		return Goal{}, err
	}
	next := g
	next.UpdatedAt = now
	// RequestRevision is the renewed approval's: the renewal is decided in
	// place, so once approved it is bound to the same revision.
	next.PendingRenewal = &ExecutionConsent{Session: host.Session, SessionUID: host.SessionUID, RequestID: requestID, RequestRevision: c.RequestRevision, Digest: digest, Terms: terms, Renews: c.Digest}
	if preflight, ok := s.ExecutionAuth.(interface {
		CheckExecutionConsent(context.Context, Goal) error
	}); ok {
		if err := preflight.CheckExecutionConsent(ctx, RenewalView(next)); err != nil {
			return Goal{}, err
		}
	}
	eventID, err := newID("goalev-inplace-")
	if err != nil {
		return Goal{}, err
	}
	event.ID, event.Goal = eventID, next
	return store.CommitInPlace(ctx, Mutation{Goal: next, Expected: g.Revision, RequestID: requestID, Hash: h, Prior: prior, Event: event})
}

// blockedReason derives why a goal that looks active cannot run.
func blockedReason(g Goal, now time.Time) string {
	c := g.Execution
	if g.Domain.GroupOwned() && g.State == Active && c != nil && c.Decision != nil && c.Decision.Approved && !now.Before(c.Terms.ExpiresAt) {
		return BlockedApprovalExpired
	}
	return ""
}
