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
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type GoalConsentCommitter interface {
	CommitGoalConsent(context.Context, memory.Entry) error
}

func BindGoalConsentHandler(p *Pipeline) {
	channelinteractions.Bind(categories.GoalExecutionConsent, func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		if !channelevents.ComponentDecisionIngress(ctx) {
			return channelinteractions.Outcome{}, fmt.Errorf("goal consent requires verified component decision ingress")
		}
		if p.Mem == nil || p.GoalConsentCommitter == nil {
			return channelinteractions.Outcome{}, fmt.Errorf("goal consent collaborators unavailable")
		}
		if d.Payload.ActionID != "approve" && d.Payload.ActionID != "deny" {
			return channelinteractions.Outcome{}, fmt.Errorf("invalid goal consent action")
		}
		scope := memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}
		entry, found, err := goalconsent.Find(ctx, p.Mem, scope, d.Payload.RequestRef)
		if err != nil {
			return channelinteractions.Outcome{}, err
		}
		if !found || entry.Provenance == nil || entry.Provenance.Publisher != "system:operator" {
			return channelinteractions.Outcome{}, fmt.Errorf("platform goal consent request missing")
		}
		var content goalconsent.Content
		if err := json.Unmarshal(entry.Content, &content); err != nil {
			return channelinteractions.Outcome{}, err
		}
		var request channelevents.InteractionRequestPayload
		if err := json.Unmarshal(content.Request, &request); err != nil {
			return channelinteractions.Outcome{}, err
		}
		// Compare the complete card, including its body and destination. An agent
		// cannot reuse a real request's digest on a misleading replacement card.
		if d.Request == nil || !reflect.DeepEqual(*d.Request, request) || request.RequestRef != entry.ID || request.Category != categories.GoalExecutionConsent {
			return channelinteractions.Outcome{}, fmt.Errorf("goal consent card differs from signed request")
		}
		owner, err := identity.FromExternal(identity.Kind(d.Payload.Decider.Kind), identity.TeamScope(d.Payload.Decider.TeamScope), identity.RawExternalID(d.Payload.Decider.ExternalID), identity.Email(d.Payload.Decider.Email)).Canonical()
		if err != nil {
			return channelinteractions.Outcome{}, err
		}
		if owner.String() != content.Goal.Domain.Owner || request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) || content.Goal.Execution == nil {
			return channelinteractions.Outcome{}, fmt.Errorf("goal consent owner or expiry invalid")
		}
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
			content.Owner = owner.String()
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
			if prior.Owner != owner.String() || prior.Approved == nil || *prior.Approved != approved {
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
