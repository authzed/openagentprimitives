package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/goalconsent"
)

type GoalConsentCommitter interface {
	CommitGoalConsent(context.Context, memory.Entry) error
}

func BindGoalConsentHandler(p *Pipeline) {
	channelinteractions.BindValidator(categories.GoalExecutionConsent, func(ctx context.Context, d channelinteractions.Decision) error {
		_, _, _, err := p.validateGoalConsent(ctx, d)
		return err
	})
	channelinteractions.Bind(categories.GoalExecutionConsent, func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		entry, content, owner, err := p.validateGoalConsent(ctx, d)
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
			if prior.Owner != owner || prior.Approved == nil || *prior.Approved != approved {
				return channelinteractions.Outcome{}, fmt.Errorf("goal consent was already decided differently")
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
	})
}

func (p *Pipeline) validateGoalConsent(ctx context.Context, d channelinteractions.Decision) (memory.Entry, goalconsent.Content, string, error) {
	if !channelevents.ComponentDecisionIngress(ctx) {
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
	if d.Request == nil || !reflect.DeepEqual(*d.Request, request) || request.RequestRef != entry.ID || request.Category != categories.GoalExecutionConsent {
		return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent card differs from signed request")
	}
	owner, err := d.Payload.Decider.Principal().Canonical()
	if err != nil {
		return memory.Entry{}, goalconsent.Content{}, "", err
	}
	if owner.String() != content.Goal.Domain.Owner || request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) || content.Goal.Execution == nil {
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
		if previous.Owner != owner.String() || previous.Approved == nil || *previous.Approved != (d.Payload.ActionID == "approve") {
			return memory.Entry{}, goalconsent.Content{}, "", fmt.Errorf("goal consent was already decided differently")
		}
	}
	return entry, content, owner.String(), nil
}
