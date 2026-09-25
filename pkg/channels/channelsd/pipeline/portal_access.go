// pkg/channels/channelsd/pipeline/portal_access.go
//
// PortalAccessTriggerer detects "manage my accounts" and its siblings in
// inbound user messages and short-circuits the pipeline: it mints a 10-minute
// portal-purpose signed deep-link and PUBLISHES an InteractionRequestPayload
// (category=portal_access) on the session's .out subject, which the outbound
// relay delivers through the bound kind's "interaction" sub-channel sender.
//
// A publish, NOT a direct send: SubChannelSenderFor("portal_access") is a
// sub-channel only some kinds implement, so every other one silently drops the
// message. The generic-Interaction path credential_request uses renders the
// button uniformly across every kind.
//
// The trigger is a UI command, NOT agent work: when it fires, channelsd
// consumes the message and it never reaches the agent — no memory append, no
// NATS wakeup. Deliver calls TryHandle BEFORE the agent-dispatch steps, and a
// true return means "stop, already handled".
//
// The trigger phrases are hardcoded English literals. Per-deployment
// configurability is a future enhancement, but it cannot be left to
// AgentClass-author wiring: every passthrough user needs portal access.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// portalAccessTTL bounds how long a portal-access deep-link stays valid
// after channelsd mints it. Short — the user clicks within a minute or
// two of asking; longer than that and they can re-trigger.
const portalAccessTTL = 10 * time.Minute

// portalPurpose is the Payload.Purpose value identityd's GET /my/accounts
// expects on the link. Mirrors identityd's purposePortal constant. We
// keep them as separate string literals (rather than re-importing) to
// preserve the package boundary — pkg/channels/channelsd → pkg/platform/identityd is one-way.
const portalPurpose = "portal"

// portalTriggers is the case-insensitive, whitespace-trimmed exact-match set of
// phrases that activate the portal-access flow. English-only; see the package
// doc for the configurability follow-up.
var portalTriggers = map[string]bool{
	"manage my accounts": true,
	"link my accounts":   true,
	"!my/accounts":       true,
}

// matchesPortalTrigger reports whether text exactly matches a portal
// trigger phrase after lowercasing + trimming surrounding whitespace.
// Exact-match (not prefix) so legitimate agent prompts beginning with
// "manage my accounts and also..." still reach the agent.
func matchesPortalTrigger(text string) bool {
	return portalTriggers[strings.ToLower(strings.TrimSpace(text))]
}

// PortalAccessTriggerer detects portal-trigger phrases on inbound user
// messages, mints a portal-purpose signed link, and publishes an
// interaction_request(portal_access) envelope. Wired into the pipeline's
// Deliver flow; the matcher fires AFTER the permission check and BEFORE
// memory append / agent wakeup so a denied user can't trigger the link.
type PortalAccessTriggerer struct {
	// LinkSigner mints the portal link. Same HMAC key channelsd uses for
	// credential_request + identityd uses to verify (the
	// spicebox-passthrough-link-key Secret).
	LinkSigner *passthroughlink.Signer

	// ExternalBaseURL returns identityd's externally reachable URL —
	// the link origin embedded in the envelope's LinkURL. A getter
	// (not a string) so the `oap init --local` ngrok-tunnel URL can
	// change without restarting the pod. Required; a nil/empty
	// returner makes the triggerer a no-op (logged, message consumed).
	ExternalBaseURL func() string

	// Senders is unused by this triggerer's publish path — delivery goes through
	// NATS and the outbound relay's "interaction" sub-channel resolution.
	// Retained because callers still set it (internal/cmd/channelsd's wiring, the
	// passthrough_portal e2e scenario), same as CredentialRequestWatcher.Senders.
	Senders SubChannelSenderResolver

	// NATS publishes the interaction_request(portal_access) envelope on
	// the session's .out subject — the outbound relay then resolves the
	// bound channel kind's "interaction" sub-channel sender and delivers
	// it. Required; a nil NATS makes the triggerer a no-op (logged,
	// message consumed) — same fail-closed shape as the missing-signer
	// case below.
	NATS NATS

	// Now is the time source for ExpiresAt. Tests inject a fixed value
	// so passthroughlink.Verify (which compares against the real wall
	// clock) keeps the link valid; production code leaves it nil to
	// default to time.Now().UTC().
	Now func() time.Time
}

// TryHandle inspects text for a portal trigger; if it matches, mints the
// link, publishes the envelope, and returns (true, nil/err). Otherwise
// returns (false, nil) — non-trigger messages pass through unchanged.
//
// The handled bool is true whenever the trigger MATCHED, regardless of
// whether the downstream mint/send succeeded — the agent must never see
// the UI command (a transient delivery failure shouldn't smuggle "link
// my accounts" into the agent's prompt). When err is non-nil, callers
// should log it but still treat the message as handled.
func (t *PortalAccessTriggerer) TryHandle(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	text string,
) (bool, error) {
	if !matchesPortalTrigger(text) {
		return false, nil
	}
	logger := log.FromContext(ctx).WithName("portal-access-trigger").WithValues(
		"session", sess.Namespace+"/"+sess.Name,
	)

	if t.LinkSigner == nil || t.ExternalBaseURL == nil || t.ExternalBaseURL() == "" || t.NATS == nil {
		// Misconfigured channelsd (no signing key, no external URL, or no
		// NATS publisher). Consume the message so the agent doesn't see
		// it; log so an operator can correlate the user complaint with
		// the missing config.
		logger.Info("portal trigger received but signer, external URL, or NATS publisher not configured; message consumed")
		return true, nil
	}

	subject := spiceboxv1alpha1.StartedBySubject(sess)
	if subject == "" {
		// kubectl-driven session (no started-by annotation). The trigger
		// needs a canonical SpiceDB subject for the link; without it,
		// there's nothing to mint. Consume + log; the user will retry
		// in a chat-spawned session.
		logger.Info("portal trigger received from session with no started-by annotation; message consumed")
		return true, nil
	}

	if sess.Spec.InputChannel == nil {
		// No bound channel to publish on. Should not happen in practice
		// (we wouldn't have reached the inbound pipeline for an unbound
		// session) but be defensive.
		logger.Info("portal trigger received on session with no InputChannel; message consumed")
		return true, nil
	}

	rawLink, err := t.LinkSigner.Mint(passthroughlink.Payload{
		Subject:   subject,
		Purpose:   portalPurpose,
		ExpiresAt: t.now().Add(portalAccessTTL).Unix(),
	})
	if err != nil {
		return true, fmt.Errorf("portal-access: mint link: %w", err)
	}
	linkURL, err := buildPortalLinkURL(t.ExternalBaseURL(), rawLink)
	if err != nil {
		return true, fmt.Errorf("portal-access: build link URL: %w", err)
	}

	// The requester's channel-scoped identity, addressed the same way every
	// other publisher in this codebase addresses a channel-attributed user
	// (see credential_request.go's doPublish comment): Kind is the
	// session's OWN channel kind (denormalized onto InputChannel.Kind), not
	// a hardcoded "slack" — the outbound relay routes this envelope to
	// whichever channel kind's "interaction" sub-channel sender is bound to
	// this session. ExternalID + Email carry the natural raw+email form
	// (started-by annotations); the Slack sender derives the canonical
	// itself via Principal().AllowSynthetic().Canonical().
	requesterIdentity := channelevents.ExternalIdentity{
		Kind:       identity.Kind(sess.Spec.InputChannel.Kind),
		ExternalID: spiceboxv1alpha1.StartedByExternalID(sess),
		Email:      spiceboxv1alpha1.StartedByEmail(sess),
	}
	if requesterIdentity.Email == "" {
		// No verified email on record for the started-by user — see
		// credential_request.go's identical fallback for why an empty
		// TeamScope can't safely re-derive the synthetic canonical here.
		// Fall back to the exact precomputed canonical (subject, "user:"
		// prefixed) via the Subject passthrough for byte-identical delivery.
		requesterIdentity.Subject = subject
	}
	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.PortalAccess,
		// portal has no decision leg (a link action needs no server-side
		// resolution), so RequestRef only needs to be stable per (session,
		// recipient) for dedup/logging purposes — unlike credential_request's
		// mintRequestID(), there is no Applied/Decision round-trip to key.
		RequestRef: "portal-" + subject.String(),
		Lead:       "Manage your linked accounts",
		Body:       "Your connected accounts are listed here. You can revoke any account or add a new one.",
		Actions: []channelevents.InteractionAction{{
			ID:    "manage_accounts",
			Label: "Manage your linked accounts",
			Style: channelevents.ActionStylePrimary,
			Kind:  channelevents.ActionKindLink,
			URL:   linkURL,
		}},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &requesterIdentity,
		},
	}
	if err := req.Validate(); err != nil {
		return true, fmt.Errorf("portal-access: built an invalid interaction_request payload (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	if err := channelevents.PublishOut(t.NATS.Publish, sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, req); err != nil {
		return true, fmt.Errorf("portal-access: publish interaction_request: %w", err)
	}

	logger.Info("portal-access link published",
		"subject", subject,
		"linkTimeout", portalAccessTTL.String())
	return true, nil
}

func (t *PortalAccessTriggerer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

// buildPortalLinkURL stitches the externalBaseURL with the signed-link
// halves, targeting /my/accounts (NOT /link — /my/accounts is the portal
// surface that bootstraps the idd_session cookie from the link's Subject
// when Purpose == "portal"). Mirrors buildLinkURL's split but with the
// /my/accounts path.
func buildPortalLinkURL(externalBaseURL, raw string) (string, error) {
	idx := strings.IndexByte(raw, '.')
	if idx <= 0 || idx == len(raw)-1 {
		return "", errors.New("malformed signed link: missing or empty halves")
	}
	b64, sig := raw[:idx], raw[idx+1:]
	base := strings.TrimRight(externalBaseURL, "/")
	q := url.Values{}
	q.Set("d", b64)
	q.Set("sig", sig)
	return base + "/my/accounts?" + q.Encode(), nil
}
