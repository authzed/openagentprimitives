//go:build e2e

// Package e2e — identity-choice API.
//
// identity_choice is a category on the generic Interaction model
// (pkg/channels/channelevents/interaction.go, pkg/channels/channelinteractions) — not a
// bespoke envelope family. The harness's identity-choice API is a thin,
// category-scoped projection over the SAME fake "interaction" sub-channel
// sender every other migrated category (credential_link, …) shares:
//
//   - ExpectIdentityChoicePrompt blocks until the fake "interaction"
//     sub-channel sender records a KindInteractionRequest envelope whose
//     Category == categories.IdentityChoice. The envelope arrives via:
//     runner IdentityChoiceGate → publish interaction_request (OUT) →
//     outbound relay → fake "interaction" sub-channel sender →
//     Driver.InteractionPrompts().
//
//   - IdentityChoice.Choose publishes a KindInteractionDecision envelope
//     (Category=identity_choice, ActionID one of the 3-way answers) on the
//     IN subject — the harness's equivalent of the initiating user clicking
//     one of the "Run as <agent>" / "Run as me" / "Cancel" buttons. The
//     pipeline's HandleInteractionDecision (subscribed in
//     startChannelsdPlumbing) validates the decider against the cached
//     request's Requester (DecideRequester — fail-closed unless the
//     decider's canonical identity matches the addressee EXACTLY; see
//     pkg/channels/channelsd/pipeline/interaction_decision.go), invokes the bound
//     IdentityChoiceDecisionHandler, and publishes a KindInteractionApplied
//     envelope on OUT (surface ack) AND IN — which the runner-side
//     subscribeInteractionApplied (internal/cmd/runner/main.go) delivers into the
//     gate's approval Orchestrator so the Await unblocks.
//
// Unlike tool-approval, there is no SpiceDB approver-set check: identity
// choice is the initiator's private call (the request is Requester-scoped —
// AudienceRequester, not AudienceApprovers), so ONLY the identity the prompt
// was addressed to may decide it. That standing check is keyed on verified
// email (identity.Canonical, WITHOUT AllowSynthetic) — Choose's cfg.user
// (defaulting to the harness's DefaultUser, overridable via AsUser) supplies
// BOTH the decider's ExternalID and Email, matching how SendUserMessage
// stamps the session's AnnotationStartedByEmail from the SAME field (see
// conversation.go's ExternalIdentity{ExternalID: cfg.user, Email: cfg.user}).
// A Choose call from an AsUser whose email differs from the session's
// starter is REJECTED fail-closed by the pipeline (logged, no Applied
// published) — see identity_choice_test.go's
// TestIdentityChoice_DeciderMustMatchRequester for the round-trip proof.
//
// No real names — tests use "alice@example.com" etc. per AGENTS.md.
package e2e

import (
	"time"

	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// IdentityChoiceActionAgent / userPassthrough / cancel are the three answers
// IdentityChoice.Choose can inject, matching InteractionDecisionPayload.ActionID
// and the runner gate's switch (pkg/agent/runner/identitygate.go).
const (
	IdentityChoiceActionAgent           = "agent"
	IdentityChoiceActionUserPassthrough = "userPassthrough"
	IdentityChoiceActionCancel          = "cancel"
)

// IdentityChoicePrompt is the harness's projection of one captured
// KindInteractionRequest envelope (Category=identity_choice). Lead/Body/
// Actions are trusted publisher copy (rendered live by every surface);
// Reason carries the dynamic recommender's advisory justification, which the
// gate routes through Excerpt (untrusted) rather than Body — see
// identitygate.go's Step 4 comment. SessionRef is "<namespace>/<name>".
type IdentityChoicePrompt struct {
	RequestID  string
	Lead       string
	Body       string
	Reason     string
	Actions    []channelevents.InteractionAction
	SessionRef string
}

// ActionLabel returns the label for the named action ID ("agent" |
// "userPassthrough" | "cancel"), or "" if the prompt carries no such action.
func (p IdentityChoicePrompt) ActionLabel(id string) string {
	for _, a := range p.Actions {
		if a.ID == id {
			return a.Label
		}
	}
	return ""
}

// IdentityChoicePredicate filters IdentityChoicePrompts. ExpectIdentityChoicePrompt
// requires every predicate to match.
type IdentityChoicePredicate func(IdentityChoicePrompt) bool

// WithRecommendation matches a prompt carrying a dynamic recommender's
// suggestion (a non-empty Body — see identitygate.go's Step 4: mode=="dynamic"
// with a usable rec.Mode is the only path that sets Body). The generic
// Interaction payload has no explicit ask-vs-dynamic Mode field, so this is
// the wire-level signal a test asserts on instead.
func WithRecommendation() IdentityChoicePredicate {
	return func(p IdentityChoicePrompt) bool { return p.Body != "" }
}

// IdentityChoice is the handle returned by ExpectIdentityChoicePrompt. Tests
// call Choose to inject the initiating user's decision.
type IdentityChoice struct {
	h      *Harness
	prompt IdentityChoicePrompt
}

// Prompt returns a copy of the captured request projection so a test can
// assert on Lead / Body / Reason / Actions after matching.
func (a *IdentityChoice) Prompt() IdentityChoicePrompt { return a.prompt }

// ExpectIdentityChoicePrompt blocks until the fake "interaction" sub-channel
// sender records a KindInteractionRequest envelope with Category ==
// categories.IdentityChoice matching every predicate, or DefaultTimeout
// elapses. On timeout it fatals with the count of interaction prompts seen
// (across ALL categories, since they share one Driver queue): a non-zero
// count usually means "some interaction rendered but none matched
// identity_choice + the predicates"; zero means "the runner never published
// an interaction_request" (the gate never ran — most commonly
// IdentityGatePending false, or the session isn't an ask|dynamic first boot).
func (h *Harness) ExpectIdentityChoicePrompt(preds ...IdentityChoicePredicate) *IdentityChoice {
	h.t.Helper()
	deadline := time.Now().Add(h.opts.DefaultTimeout)
	seen := 0
	for time.Now().Before(deadline) {
		ch := h.singleChannel("ExpectIdentityChoicePrompt")
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv != nil {
			prompts := drv.InteractionPrompts()
			for i := seen; i < len(prompts); i++ {
				if prompts[i].Payload.Category != categories.IdentityChoice {
					continue
				}
				p := identityChoicePromptFromRecord(prompts[i])
				if matchesAllIdentityChoice(p, preds) {
					return &IdentityChoice{h: h, prompt: p}
				}
			}
			seen = len(prompts)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("ExpectIdentityChoicePrompt: timed out after %s (seen %d interaction prompt(s) total)\n%s",
		h.opts.DefaultTimeout, seen, h.dumpState())
	return nil
}

// Choose publishes a KindInteractionDecision envelope (Category=identity_choice)
// carrying action (IdentityChoiceActionAgent / userPassthrough / cancel). The
// decider identity defaults to the harness DefaultUser; AsUser overrides it —
// both the ExternalID and the Email the pipeline's DecideRequester standing
// check canonicalizes against the cached request's Requester. Returns once
// the envelope is published + flushed; the pipeline validation + gate resume
// happen asynchronously (a rejected decider is a silent no-op on the pipeline
// side — the session stays parked; see TestIdentityChoice_DeciderMustMatchRequester).
func (a *IdentityChoice) Choose(action string, opts ...SendOption) {
	a.h.t.Helper()
	cfg := sendCfg{user: a.h.opts.DefaultUser}
	for _, o := range opts {
		o(&cfg)
	}
	ns, name, ok := splitSessionRef(a.prompt.SessionRef)
	if !ok {
		a.h.t.Fatalf("IdentityChoice.Choose: malformed SessionRef %q", a.prompt.SessionRef)
	}
	payload := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        categories.IdentityChoice,
		RequestRef:      a.prompt.RequestID,
		ActionID:        action,
		Decider: channelevents.ExternalIdentity{
			Kind:       "fake",
			ExternalID: identity.RawExternalID(cfg.user),
			Email:      identity.Email(cfg.user),
		},
	}
	if err := channelevents.PublishIn(
		func(subj string, b []byte) error { return a.h.nc.Publish(subj, b) },
		ns, name, channelevents.KindInteractionDecision, payload,
	); err != nil {
		a.h.t.Fatalf("IdentityChoice.Choose(%s): publish interaction_decision: %v", action, err)
	}
	if err := a.h.nc.Flush(); err != nil {
		a.h.t.Logf("IdentityChoice.Choose(%s): nats Flush: %v", action, err)
	}
}

func matchesAllIdentityChoice(p IdentityChoicePrompt, preds []IdentityChoicePredicate) bool {
	for _, pred := range preds {
		if !pred(p) {
			return false
		}
	}
	return true
}

func identityChoicePromptFromRecord(r fakekind.InteractionPrompt) IdentityChoicePrompt {
	reason := ""
	if r.Payload.Excerpt != nil {
		reason = r.Payload.Excerpt.Content
	}
	return IdentityChoicePrompt{
		RequestID:  r.Payload.RequestRef,
		Lead:       r.Payload.Lead,
		Body:       r.Payload.Body,
		Reason:     reason,
		Actions:    r.Payload.Actions,
		SessionRef: r.SessionRef.Namespace + "/" + r.SessionRef.Name,
	}
}
