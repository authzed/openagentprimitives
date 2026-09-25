package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// PreferenceCommitter is what the preference-confirm decision handler needs
// from the operator's memory client: committing a confirmed preference
// value. Narrow on purpose — the handler must depend on neither the concrete
// operator httpclient.Client nor its component-token transport, only on the
// one write it performs. *httpclient.Client satisfies this today; wired in
// internal/cmd/channelsd from the same client the memory facade already
// holds.
type PreferenceCommitter interface {
	CommitPreference(ctx context.Context, ns, name string, req preferences.CommitRequest) error
}

// BindPreferenceCommitHandler wires the SIDE-EFFECTING user_preference_confirm
// decision handler (commits the confirmed preference on approve) into the
// interaction registry. Standing is enforced FIRST by the interaction_decision
// pipe's DecideRequester policy — only the addressee the card was raised for
// may decide it — so the commit below never needs its own standing check; its
// only remaining job is to commit exactly what channelsd itself cached at
// publish time, under that addressee's own verified canonical subject.
func BindPreferenceCommitHandler(p *Pipeline) {
	channelinteractions.Bind(categories.UserPreferenceConfirm, preferenceCommitHandler(p))
}

// preferenceConfirmDetails is the user_preference_confirm interaction's
// Details payload (InteractionRequestPayload.Details, json.RawMessage) — the
// exact (key, value, display) triple the runner published when the card was
// raised and channelsd rendered from. It rides in Details, and is persisted
// on the durable memapproval record, so a channelsd restart can still recover
// it (resolvePreferenceConfirmDetails' durable leg) rather than lose the very
// value the card promised to save. Value is absent (or JSON null) for a
// clear — CommitRequest carries that through unchanged.
type preferenceConfirmDetails struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value,omitempty"`
	Display string          `json:"display,omitempty"`
}

// preferenceCommitHandler returns the bound handler. mem and committer are
// captured at Bind time (DI), matching toolApprovalHandler's shape.
func preferenceCommitHandler(p *Pipeline) channelinteractions.DecisionHandler {
	mem := p.Mem
	committer := p.PreferenceCommitter
	return func(ctx context.Context, d channelinteractions.Decision) (channelinteractions.Outcome, error) {
		switch d.Payload.ActionID {
		case "deny":
			// Deny has no side effect: the addressee said no, so nothing is
			// committed. The runner reads the outcome off interaction_applied;
			// it has no post-deny effect of its own for preference_save.
			return channelinteractions.Outcome{Result: channelevents.OutcomeDenied}, nil
		case "approve":
			det, err := resolvePreferenceConfirmDetails(ctx, d, mem)
			if err != nil {
				// Never silently drop this: the card promised to save exactly
				// this (key, value), and losing the details means committing
				// nothing while still telling the addressee "approved" would be
				// worse than surfacing the failure. Session, category and
				// requestRef context is added by resolvePreferenceConfirmDetails
				// (requestRef) and by the caller, HandleInteractionDecision,
				// which wraps every handler error with (category, session)
				// before its envelopeHandler logs it — the same chain
				// toolApprovalHandler's resolve-details error relies on.
				return channelinteractions.Outcome{}, fmt.Errorf(
					"preference confirm decision: resolve details (requestRef %q): %w", d.Payload.RequestRef, err)
			}
			if committer == nil {
				// Programming error: the pipeline was built without a preference
				// committer. Fail loud (the pipe's handler-error path surfaces
				// this to the addressee) rather than silently approving with
				// nothing written.
				return channelinteractions.Outcome{}, fmt.Errorf(
					"preference confirm decision: PreferenceCommitter not configured (programming error) (requestRef %q)", d.Payload.RequestRef)
			}
			// Subject is ALWAYS the verified decider — recomputed here the same
			// way HandleInteractionDecision computed deciderCanon
			// (interaction_decision.go:126) before ever invoking this handler,
			// via the same deterministic identity.FromExternal(...).Canonical().
			// By the time this handler runs, the pipe's DecideRequester policy
			// has already confirmed this decider IS the card's addressee — so
			// this is not a second trust decision, it is the same one, recomputed
			// because Decision carries the click's claim, not the pipe's derived
			// value. Never read a target out of det: a mis-addressed card must
			// only ever be able to write the clicking user's OWN preference, for
			// exactly the value displayed.
			deciderCanon, err := identity.FromExternal(
				identity.Kind(d.Payload.Decider.Kind), identity.TeamScope(d.Payload.Decider.TeamScope),
				identity.RawExternalID(d.Payload.Decider.ExternalID), identity.Email(d.Payload.Decider.Email),
			).Canonical()
			if err != nil {
				return channelinteractions.Outcome{}, fmt.Errorf(
					"preference confirm decision: decider has no canonical identity (requestRef %q): %w", d.Payload.RequestRef, err)
			}
			var value *apiextv1.JSON
			if len(det.Value) > 0 {
				value = &apiextv1.JSON{Raw: det.Value}
			}
			if err := committer.CommitPreference(ctx, d.Session.Namespace, d.Session.Name, preferences.CommitRequest{
				Key:     det.Key,
				Value:   value,
				Subject: deciderCanon.String(),
			}); err != nil {
				// The operator's refusal (locked since publish, schema changed) is
				// authoritative: surface it through the same handler-error path
				// every other side-effecting handler uses, and do not retry —
				// retrying here would race whatever changed on the operator side
				// since publish.
				return channelinteractions.Outcome{}, fmt.Errorf(
					"preference confirm decision: commit preference %q (requestRef %q): %w", det.Key, d.Payload.RequestRef, err)
			}
			return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
		default:
			return channelinteractions.Outcome{}, fmt.Errorf(
				"preference confirm decision: unknown actionID %q (requestRef %q)", d.Payload.ActionID, d.Payload.RequestRef)
		}
	}
}

// resolvePreferenceConfirmDetails mirrors resolveToolApprovalDetails exactly:
// the cached request's Details when the in-process/parked-prompt cache is
// warm, else the durable memapproval record recovered across a channelsd
// restart. Either way this is channelsd's OWN copy of what the card
// displayed at publish time — never anything a runner could supply
// post-publish, which is what makes the commit's (key, value) trustworthy.
func resolvePreferenceConfirmDetails(ctx context.Context, d channelinteractions.Decision, mem memory.Memory) (preferenceConfirmDetails, error) {
	if d.Request != nil && len(d.Request.Details) > 0 {
		var det preferenceConfirmDetails
		if err := json.Unmarshal(d.Request.Details, &det); err != nil {
			return preferenceConfirmDetails{}, fmt.Errorf("decode cached preference-confirm details: %w", err)
		}
		return det, nil
	}
	if mem == nil {
		return preferenceConfirmDetails{}, fmt.Errorf("no cached request and no durable memory reader")
	}
	rec, err := memapproval.RequestByID(ctx, mem, memory.Scope{Kind: "session", ID: d.Session.Namespace + "/" + d.Session.Name}, d.Payload.RequestRef)
	if err != nil {
		return preferenceConfirmDetails{}, fmt.Errorf("durable memapproval lookup: %w", err)
	}
	if rec == nil || len(rec.Details) == 0 {
		return preferenceConfirmDetails{}, fmt.Errorf("durable memapproval record missing details for requestRef %q", d.Payload.RequestRef)
	}
	var det preferenceConfirmDetails
	if err := json.Unmarshal(rec.Details, &det); err != nil {
		return preferenceConfirmDetails{}, fmt.Errorf("decode durable preference-confirm details: %w", err)
	}
	return det, nil
}
