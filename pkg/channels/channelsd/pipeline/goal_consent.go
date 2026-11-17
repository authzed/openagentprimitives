package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type GoalConsentCommitter interface {
	CommitGoalConsent(context.Context, memory.Entry) error
}

// GoalConsentChecker is a GoalConsentCommitter that can ask the goal server,
// before a decision is recorded, whether the clicker may decide the signed
// request. Group goal consent requires it; a committer without it refuses
// group decisions.
type GoalConsentChecker interface {
	CheckGoalConsent(ctx context.Context, request memory.Entry, decider string, approved bool) error
}

// BindGoalConsentHandler binds both goal consent categories: the private
// card, decided by the goal's one owner, and the group card, decided by any
// manager of that goal at click time (validateGoalConsent tells them apart).
func BindGoalConsentHandler(p *Pipeline) {
	channelinteractions.BindValidator(categories.GoalExecutionConsent, func(ctx context.Context, d channelinteractions.Decision) error {
		_, _, _, err := p.validateGoalConsent(ctx, categories.GoalExecutionConsent, d)
		return err
	})
	for _, category := range []string{categories.GoalExecutionConsent, categories.GoalGroupExecutionConsent} {
		channelinteractions.Bind(category, func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
			return p.decideGoalConsent(ctx, category, d)
		})
	}
}

// decideGoalConsent records and commits a decision on a card of category, the
// category the handler is bound for; a card of the other category is refused.
func (p *Pipeline) decideGoalConsent(ctx context.Context, category string, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
	entry, content, owner, err := p.validateGoalConsent(ctx, category, d)
	if err != nil {
		return channelinteractions.Outcome{}, err
	}
	scope := entry.Scope

	approved := d.Payload.ActionID == "approve"
	id := "goalconsent-decision-" + content.Goal.Execution.Digest
	decision, exists, err := goalconsent.Find(ctx, p.Mem, scope, id)
	if err != nil {
		return channelinteractions.Outcome{}, err
	}
	if !exists {
		witness, err := json.Marshal(entry)
		if err != nil {
			return channelinteractions.Outcome{}, err
		}
		// Owner is who decided: the goal's owner for a private goal, the
		// deciding manager (the approver) for a group goal.
		content.Owner = owner
		content.Approved = &approved
		content.RequestWitness = witness
		raw, err := json.Marshal(content)
		if err != nil {
			return channelinteractions.Outcome{}, err
		}
		decision = memory.Entry{Scope: scope, Kind: goalconsent.KindName, ID: id, Content: raw, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		decision, err = p.Mem.Put(ctx, decision)
		if err != nil {
			return channelinteractions.Outcome{}, err
		}
	} else {
		var prior goalconsent.Content
		if err := json.Unmarshal(decision.Content, &prior); err != nil {
			return channelinteractions.Outcome{}, err
		}
		if err := priorDecisionConflict(content.Goal, prior, owner, approved); err != nil {
			return channelinteractions.Outcome{}, err
		}
	}
	if err := p.GoalConsentCommitter.CommitGoalConsent(ctx, decision); err != nil {
		return channelinteractions.Outcome{}, err
	}
	outcome := channelevents.OutcomeDenied
	if approved {
		outcome = channelevents.OutcomeApproved
	}
	return channelinteractions.Outcome{Result: outcome}, nil
}

// priorDecisionConflict lets only the original decider re-commit a recorded
// decision (a retry after a failed commit). For a group goal, the goal server
// has already answered "already decided" if one was committed (the pre-write
// check), so a recorded decision by someone else here is one that never
// completed — only a fresh request can replace it.
func priorDecisionConflict(g goals.Goal, prior goalconsent.Content, owner string, approved bool) error {
	if prior.Owner == owner && prior.Approved != nil && *prior.Approved == approved {
		return nil
	}
	if g.Domain.GroupOwned() {
		return fmt.Errorf("this approval could not be completed; ask a manager of this goal to request it again")
	}
	return fmt.Errorf("goal consent was already decided differently")
}

// validateGoalConsent checks a click on a goal consent card of category (the
// category the caller is bound for) against the platform-signed request.
func (p *Pipeline) validateGoalConsent(ctx context.Context, category string, d channelinteractions.Decision) (memory.Entry, goalconsent.Content, string, error) {
	// A group goal's card (standing-plan approval, and the renewal card a run
	// posts) is clicked in the group's own conversation, so its decision
	// arrives on the ordinary inbound subject like any transport click. What
	// guards it does not depend on ingress: the generic pipe's resource-owner
	// check on agent_goal:<id>#manage, checkGoalManager below, the goal
	// server's pre-write check, and the append-only record. A private goal's
	// card keeps component-only ingress.
	if !channelevents.ComponentDecisionIngress(ctx) && category != categories.GoalGroupExecutionConsent {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent requires verified component decision ingress")
	}
	if p.Mem == nil || p.GoalConsentCommitter == nil {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent collaborators unavailable")
	}
	if d.Payload.ActionID != "approve" && d.Payload.ActionID != "deny" {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("invalid goal consent action")
	}
	scope := memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}
	entry, found, err := goalconsent.Find(ctx, p.Mem, scope, d.Payload.RequestRef)
	if err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	if !found || entry.Provenance == nil || entry.Provenance.Publisher != "system:operator" {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("platform goal consent request missing")
	}
	var content goalconsent.Content
	if err := json.Unmarshal(entry.Content, &content); err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	var request channelevents.InteractionRequestPayload
	if err := json.Unmarshal(content.Request, &request); err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	// Compare the complete card, including its body and destination. An agent
	// cannot reuse a real request's digest on a misleading replacement card.
	// The category is fixed by the goal — it is what chose the decider policy
	// — and must be the one this caller is bound for.
	group := content.Goal.Domain.GroupOwned()
	if group != (category == categories.GoalGroupExecutionConsent) {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent card is not a %s request", category)
	}
	if d.Request == nil || !reflect.DeepEqual(*d.Request, request) || request.RequestRef != entry.ID || request.Category != category {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent card differs from signed request")
	}
	owner, err := d.Payload.Decider.Principal().Canonical()
	if err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	if request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) || content.Goal.Execution == nil {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent owner or expiry invalid")
	}
	if group {
		if err := p.checkGoalManager(ctx, content.Goal, request, owner); err != nil {
			return memory.Entry{}, goalconsent.Content{}, "", err
		}
		// The decision record is append-only and one per digest, so a
		// decision the goal server would refuse must never be written: it
		// would answer every other manager "already decided". Ask the server
		// first, by the predicate its decide path uses.
		checker, ok := p.GoalConsentCommitter.(GoalConsentChecker)
		if !ok {
			return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("group goal consent unavailable: the goal server cannot check a decision before it is recorded")
		}
		if err := checker.CheckGoalConsent(ctx, entry, owner.String(), d.Payload.ActionID == "approve"); err != nil {
			return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("the goal server refused this decision: %w", err)
		}
	} else if owner.String() != content.Goal.Domain.Owner {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent owner or expiry invalid")
	}
	if d.Request.AgentSessionRef != d.Session {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent session mismatch")
	}
	prior, exists, err := goalconsent.Find(ctx, p.Mem, scope, "goalconsent-decision-"+content.Goal.Execution.Digest)
	if err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	if exists {
		var previous goalconsent.Content
		if err := json.Unmarshal(prior.Content, &previous); err != nil {
			return memory.Entry{}, goalconsent.Content{}, "", err
		}
		if err := priorDecisionConflict(content.Goal, previous, owner.String(), d.Payload.ActionID == "approve"); err != nil {
			return memory.Entry{}, goalconsent.Content{}, "", err
		}
	}
	return entry, content, owner.String(), nil
}

// checkGoalManager requires the clicker to hold manage on the group goal, live
// at click time. The signed card must name exactly that goal's manage ref as
// its decider resource, so the generic decision pipe's resource-owner check
// and this one ask the same question.
func (p *Pipeline) checkGoalManager(ctx context.Context, g goals.Goal, request channelevents.InteractionRequestPayload, who identity.CanonicalUserID) error {
	typ, id, perm := g.ManageRef()
	if len(request.Resources) != 1 || request.Resources[0] != (channelevents.InteractionResourceRef{Type: typ, ID: id, Permission: perm}) {
		return fmt.Errorf("group goal consent card does not name its goal's managers")
	}
	if p.Authz == nil {
		return fmt.Errorf("group goal consent unavailable: no authority to check the goal's managers")
	}
	ok, err := p.Authz.CheckOnResource(ctx, typ, id, perm, who, true)
	if err != nil {
		return fmt.Errorf("group goal consent: checking manage on %s:%s: %w", typ, id, err)
	}
	if !ok {
		return fmt.Errorf("only a manager of this group goal can decide its consent (you do not manage it)")
	}
	return nil
}
