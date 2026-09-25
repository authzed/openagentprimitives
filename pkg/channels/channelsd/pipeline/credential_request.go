// pkg/channels/channelsd/pipeline/credential_request.go
//
// CredentialRequestWatcher is the channelsd-side bridge between an
// operator-parked passthrough AgentSession (phase=AwaitingCredentials)
// and the user-visible "Connect your accounts" prompt, rendered on every
// channel kind's "interaction" sub-channel via the unified Interaction
// model (pkg/channels/channelevents/interaction.go).
//
// Per tick it lists channel-attached sessions in AwaitingCredentials, loads the
// matching SessionUserIdentity, mints a signed deep-link, builds an
// InteractionRequestPayload (category=credential_link), and PUBLISHES it on the
// session's .out subject. The outbound relay then resolves the bound kind's
// "interaction" sub-channel sender and delivers it.
//
// A publish, NOT a direct send: SubChannelSenderFor("credential_request")
// returns nil for any kind that has not implemented that specific sub-channel,
// silently hanging the session with no prompt ever rendered. Every kind
// implements "interaction", so publishing through it works uniformly.
//
// AgentSessionConditionCredentialRequestPublished is the dedup key — once True
// the watcher skips the session, so a long-parked link does not re-prompt every
// poll cycle.
//
// MissingCredentials is passed to the link signer VERBATIM as the signed link's
// RequiredCredentials, while the payload's Items come from
// SUI.Status.Explanation.Items. The prompt must enumerate every account the
// user has not connected; any filtering or truncation here silently breaks that
// contract, which is why a test guards it.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/explainer"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// DefaultCredentialLinkTimeout is the fallback link ExpiresAt window
// when AgentClass.Spec.CredentialLinkTimeout is nil. Matches the
// kubebuilder default on the CRD (30m).
const DefaultCredentialLinkTimeout = 30 * time.Minute

// CredentialRequestWatcherInterval is the polling cadence. Matches
// sessionWatcher's interval (5s) — the operator stamp + park is the
// race-prone moment; once Explanation is in place, a 5s ceiling on
// time-to-prompt is acceptable.
const CredentialRequestWatcherInterval = 5 * time.Second

// SubChannelSenderResolver is the slice of senderResolver's interface
// the watcher needs. Kept narrow so tests can fake just this one
// method rather than the full outbound.SenderResolver shape.
type SubChannelSenderResolver interface {
	// SubChannelSenderFor returns the channel-kind's Sender for the
	// named sub-channel. (nil, nil) means "kind doesn't implement this
	// sub-channel" — the watcher skips delivery and logs.
	SubChannelSenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error)
}

// CredentialRequestWatcher polls AgentSessions, mints links, and
// publishes KindInteractionRequest (category=credential_link) envelopes
// on each session's .out subject.
type CredentialRequestWatcher struct {
	// K8s is the client used to List sessions, Get the matching SUI +
	// AgentClass, and Patch the dedup condition.
	K8s client.Client

	// Senders is unused by this watcher's publish path — delivery goes through
	// NATSPublish and the outbound relay's "interaction" sub-channel resolution.
	// Retained because other callers still set it (e2e scenario harnesses), and
	// SubChannelSenderResolver itself stays live for portal_access.go.
	Senders SubChannelSenderResolver

	// LinkSigner mints the HMAC-signed deep-link payload. Shared with
	// identityd via the spicebox-passthrough-link-key Secret.
	LinkSigner *passthroughlink.Signer

	// ExternalBaseURL returns the identityd-reachable URL that the
	// link embeds. A getter (not a string) so the `oap init --local`
	// ngrok-tunnel URL can change without restarting the pod —
	// main.go wires this to externalurl.Provider.Get. Required.
	ExternalBaseURL func() string

	// DefaultLinkTimeout overrides DefaultCredentialLinkTimeout (used
	// when AgentClass.Spec.CredentialLinkTimeout is nil). Tests set it
	// to a known value so they can assert on the link's ExpiresAt.
	// Zero means use DefaultCredentialLinkTimeout.
	DefaultLinkTimeout time.Duration

	// Now is the time source for ExpiresAt + condition transition
	// times. Tests inject a fixed value; production code leaves it nil
	// to default to time.Now().UTC().
	Now func() time.Time

	// Explainer, when non-nil, is used to generate LLM-backed {what,
	// why} text for the credential_request envelope. Nil (or any error
	// from Explain) falls back to the operator-stamped static
	// explanation from the SessionUserIdentity. Declared as the
	// interface type (not a concrete pointer) so the zero value is a
	// true nil interface — see AGENTS.md "Nil interfaces: never assign
	// a typed-nil pointer".
	Explainer explainer.Explainer

	// NATSPublish carries two things: the KindInteractionRequest envelope that
	// IS the credential prompt (the primary delivery path), and a best-effort
	// KindNotification afterwards that moves the thread's status indicator from
	// the generic "is starting…" to "is waiting for identity setup…", so the
	// user understands why the agent appears paused. REQUIRED: doPublish errors
	// and Run refuses to start when nil, because a silent no-op here is exactly
	// the hang that leaves a parked session with no prompt at all.
	NATSPublish channelevents.PublishFunc
}

// Run polls until ctx is canceled. Mirrors sessionWatcher.Run.
func (w *CredentialRequestWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("credentialrequest-watcher")
	if w.LinkSigner == nil || w.ExternalBaseURL == nil || w.NATSPublish == nil {
		// A misconfigured channelsd — no signing key, no external URL getter,
		// no NATS publisher — would silently fail to deliver the prompt at all.
		// Refuse to run rather than fail invisibly. NATSPublish is load-bearing
		// for the primary delivery path, not just the waiting-status notice, so
		// it is required alongside the other two.
		logger.Info("watcher disabled: missing LinkSigner, ExternalBaseURL, or NATSPublish")
		return
	}
	ticker := time.NewTicker(CredentialRequestWatcherInterval)
	defer ticker.Stop()

	w.reconcileAll(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcileAll(ctx, logger)
		}
	}
}

// reconcileAll lists AwaitingCredentials sessions and dispatches each
// to ReconcileOne. Errors on per-session work are logged but not
// returned — the next tick retries.
func (w *CredentialRequestWatcher) reconcileAll(ctx context.Context, logger logr.Logger) {
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := w.K8s.List(ctx, &sessions,
		client.HasLabels{spiceboxv1alpha1.LabelChannelName},
	); err != nil {
		logger.Error(err, "list agentsessions")
		return
	}
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
			continue
		}
		if credentialUpdateParked(sess) {
			// Parked for an Open CredentialUpdateRequest — CredentialUpdateWatcher's
			// job — not the passthrough identity gate. Without this check every
			// such session logs "SUI not found" or "operator bug" every tick for
			// its entire park, and one that ALSO has missingCredentials gets a
			// second, unrelated "Connect your accounts" card.
			continue
		}
		if sess.Spec.InputChannel == nil {
			// Not channel-attached → no surface to deliver on. Skip
			// silently; not an error, just an inapplicable session.
			continue
		}
		if alreadyPublished(sess) {
			continue
		}
		if err := w.ReconcileOne(ctx, sess); err != nil {
			logger.Info("credential_request reconcile failed",
				"session", sess.Namespace+"/"+sess.Name,
				"err", err.Error())
			// Next tick retries; per-session failure does not abort the loop.
			continue
		}
	}
}

// ReconcileOne does the per-session work for one AwaitingCredentials
// session. Exported (not just lowercase) so tests can drive it
// directly without spinning up a polling loop.
//
// Idempotent: a no-op once the dedup condition is True. The condition is
// stamped AFTER a successful Send, so a transient Send failure retries next tick
// rather than wedging the session — at the cost of a duplicate prompt when Send
// returns nil but the transport silently 5xx'd. That trade deliberately favors
// "the user might see two prompts" over "the user sees none and it hangs".
func (w *CredentialRequestWatcher) ReconcileOne(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
		return nil
	}
	if credentialUpdateParked(sess) {
		return nil
	}
	if sess.Spec.InputChannel == nil {
		return nil
	}
	if alreadyPublished(sess) {
		return nil
	}
	return w.doPublish(ctx, sess, false /* skipExplainer */)
}

// credentialUpdateParked reports whether sess sits in AwaitingCredentials for an
// Open CredentialUpdateRequest (the AgentSessionConditionCredentialUpdatePending
// marker) rather than the passthrough identity gate this watcher serves. The two
// causes share one phase value and nothing else: a credential_update park has no
// MissingCredentials to act on, so treating it as one spams "operator bug" logs
// for the whole park, or publishes an unrelated "Connect your accounts" card
// beside the credential_update card the OTHER watcher already delivered.
func credentialUpdateParked(sess *spiceboxv1alpha1.AgentSession) bool {
	return conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)
}

// ForcePublish re-sends the credential prompt to the requester regardless
// of the CredentialRequestPublished dedup condition, minting a fresh
// signed link. Used to re-surface the prompt when the user re-interacts
// from another device while the session is still parked in
// AwaitingCredentials — see reconcile-on-user-reinteraction callers in
// internal/cmd/channelsd.
//
// Keeps ReconcileOne's phase, credentialUpdateParked and InputChannel guards —
// a session in the wrong phase, parked for a different watcher, or with no
// channel has nothing to re-surface — but deliberately omits alreadyPublished,
// the one guard ForcePublish exists to bypass. doPublish's "nothing missing"
// no-op still applies.
//
// credentialUpdateParked matters here even though "nothing missing" usually
// catches the same sessions: one parked on an Open CredentialUpdateRequest that
// ALSO has missing passthrough credentials would get a second, unrelated card
// on top of the credential-update card. Bypassing alreadyPublished is precisely
// what lets this path reach that case.
//
// The LLM "why" explainer is skipped: the user saw the reasons on first publish,
// and static/declared whys keep a re-surface cheap.
func (w *CredentialRequestWatcher) ForcePublish(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
		return nil
	}
	if credentialUpdateParked(sess) {
		return nil
	}
	if sess.Spec.InputChannel == nil {
		return nil
	}
	return w.doPublish(ctx, sess, true /* skipExplainer */)
}

// doPublish is the shared publish body for ReconcileOne and ForcePublish:
// load the SUI, mint a signed link, build + publish the credential_link
// interaction_request envelope, and stamp the dedup condition. skipExplainer,
// when true, bypasses the LLM explainer call (ForcePublish's re-surface
// path) — the static-fallback why-fill still runs regardless, so every row
// keeps a non-empty reason.
func (w *CredentialRequestWatcher) doPublish(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, skipExplainer bool) error {
	logger := log.FromContext(ctx).WithValues("session", sess.Namespace+"/"+sess.Name)

	// Fail fast on a wiring bug: without a NATS publisher there is no way to
	// deliver the prompt at all, and silently returning here would hang the
	// parked session exactly as a nil sub-channel sender would. Checked before
	// any K8s Get or mint work, so a misconfigured channelsd fails loudly on the
	// first tick rather than burning API calls per session.
	if w.NATSPublish == nil {
		return fmt.Errorf("credential_request: NATSPublish not configured (wiring bug); cannot deliver interaction_request for session %s/%s", sess.Namespace, sess.Name)
	}

	// SUI is a same-name, same-namespace projection of the session.
	var sui spiceboxv1alpha1.SessionUserIdentity
	suiKey := client.ObjectKey{Namespace: sess.Namespace, Name: sess.Name}
	if err := w.K8s.Get(ctx, suiKey, &sui); err != nil {
		if apierrors.IsNotFound(err) {
			// Race: the operator hasn't created the SUI for this
			// session yet (or never will — kubectl-driven session
			// outside identityMode=userPassthrough that somehow
			// reached AwaitingCredentials). Log and skip; the next
			// tick retries.
			logger.Info("SessionUserIdentity not found; will retry next tick")
			return nil
		}
		return fmt.Errorf("get SessionUserIdentity %s: %w", suiKey, err)
	}

	// Race-handling: the operator stamps Explanation AND parks. The two
	// writes are independent enough that an observer can see the phase
	// change first and the explanation later. Treat "not yet stamped"
	// as "wait another tick" — NOT a permanent skip.
	if sui.Status.Explanation == nil || len(sui.Status.Explanation.Items) == 0 {
		logger.Info("SUI explanation not stamped yet; will retry next tick")
		return nil
	}

	// MissingCredentials must be non-empty: an empty list means the
	// session is parked-but-actually-ready (operator bug — the operator
	// should have un-parked instead). Log so the bug is grep-able; do
	// not stamp the condition (we never sent anything).
	if len(sui.Status.MissingCredentials) == 0 {
		logger.Info("SUI parked with empty missingCredentials; operator bug, skipping",
			"phase", sess.Status.Phase)
		return nil
	}

	starter := spiceboxv1alpha1.StartedBySubject(sess)
	if starter == "" {
		// kubectl-driven session has no started-by annotation. Without
		// a recipient the channel kind has no one to DM. Log + skip.
		logger.Info("session has no started-by canonical-id annotation; skipping")
		return nil
	}

	// Resolve AgentClass — used for link timeout AND explainer display
	// name. Best-effort: a missing class degrades to defaults for both,
	// not a fatal error.
	var ac spiceboxv1alpha1.AgentClass
	acFound := false
	if sess.Spec.Class != "" {
		acKey := client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}
		if err := w.K8s.Get(ctx, acKey, &ac); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Info("AgentClass lookup failed; using defaults",
					"class", sess.Spec.Class, "err", err.Error())
			}
			// IsNotFound on a session that already exists is unusual but
			// non-fatal — same fallback as the API error case.
		} else {
			acFound = true
		}
	}

	// Resolve link timeout from the AgentClass when one is set; fall
	// back to the watcher default (which itself falls back to
	// DefaultCredentialLinkTimeout).
	timeout := w.DefaultLinkTimeout
	if timeout == 0 {
		timeout = DefaultCredentialLinkTimeout
	}
	if acFound && ac.Spec.CredentialLinkTimeout != nil {
		timeout = ac.Spec.CredentialLinkTimeout.Duration
	}

	now := w.now()
	// Defensive copy of MissingCredentials so a downstream consumer
	// can't mutate the SUI's status slice. NO FILTERING — every entry
	// must be covered by SOMETHING in the rendered prompt.
	missing := append([]string(nil), sui.Status.MissingCredentials...)

	// ONE signed link covers all missing credentials: identityd renders /link as
	// a menu page with per-credential status badges and connect actions, and the
	// same link backs both the bootstrap GET and every per-row submit.
	raw, err := w.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef:          sess.Namespace + "/" + sess.Name,
		Subject:             starter,
		RequiredCredentials: missing,
		ExpiresAt:           now.Add(timeout).Unix(),
	})
	if err != nil {
		return fmt.Errorf("mint passthrough link: %w", err)
	}
	externalURL := w.ExternalBaseURL()
	if externalURL == "" {
		// Fail closed: the external web address is unconfigured, so a link
		// minted now would be hostless. Refuse to emit a broken button. Surface
		// it on the SUI status, the monitoring channel AND the session thread,
		// then error so the next tick retries once the ConfigMap is fixed.
		return w.handleExternalURLUnconfigured(ctx, sess, &sui, logger)
	}
	linkURL, err := buildLinkURL(externalURL, raw)
	if err != nil {
		return fmt.Errorf("build link URL: %w", err)
	}

	// One channel button. When the user has exactly one credential to
	// link, address it by provider name so the button is concrete
	// ("Connect Linear" / "Connect GitHub"). With more than one missing,
	// the button is generic and the menu page enumerates them.
	buttonLabel := w.singleButtonLabel(ctx, logger, missing)

	// Read the operator-stamped per-credential items. Titles and
	// Descriptions are deterministic (tool field → provider catalog →
	// humanized name), so they never come from the LLM. Whys may be
	// empty — the deliberate "render-time fallback" signal.
	sessRef := sess.Namespace + "/" + sess.Name
	items := sui.Status.Explanation.Items
	why := make([]string, len(items))
	for i, it := range items {
		why[i] = it.Why // may be empty → filled below
	}

	// Fill empty whys with the precedence declared > LLM (gaps only) >
	// static. Declared whys are rendered verbatim and never sent to the
	// LLM. The static sentence references the AgentClass display name.
	staticWhy := fmt.Sprintf(
		"%s needs to call this service as you to complete the work you asked for.",
		agentClassDisplayName(&ac, acFound),
	)
	gaps := credsNeedingWhy(items)
	if !skipExplainer && w.Explainer != nil && len(gaps) > 0 {
		in := buildExplainerInput(sess.Spec.Prompt.Inline, gaps, items, &ac, acFound)
		out, expErr := w.Explainer.Explain(ctx, in)
		if expErr == nil && len(out.Why) == len(gaps) {
			fillWhys(why, items, gaps, out.Why)
			logger.Info("credential_request: LLM explainer filled why gaps",
				"session", sessRef, "gaps", len(gaps))
		} else {
			// Any failure (error, wrong-cardinality, empty) leaves the gap
			// whys blank so the static fallback below fills them. The UX
			// requirement that EVERY row carry a reason is preserved.
			logger.Info("credential_request: LLM explainer unusable; using static for gaps",
				"session", sessRef, "gaps", len(gaps), "err", errStr(expErr))
		}
	}
	for i := range why {
		if strings.TrimSpace(why[i]) == "" {
			why[i] = staticWhy
		}
	}

	// Assemble payload Items: one per SUI explanation item, with the
	// per-row why (declared>LLM>static) now guaranteed non-empty.
	payloadItems := make([]channelevents.CredentialRequestItem, len(items))
	for i, it := range items {
		payloadItems[i] = channelevents.CredentialRequestItem{
			Credential:  it.Credential,
			Title:       it.Title,
			Description: it.Description,
			Why:         why[i],
		}
	}

	// Render each item as an InteractionField: "Title: Description — Why"
	// (Description/Why omitted when empty). This is the generic renderer's
	// only structured-content surface — the per-credential icon accessory
	// the old Slack-only sender attached has no equivalent on the unified
	// Interaction model and is dropped here (Slice 1 scope: see
	// pkg/channels/channelkinds/slack/interaction.go's file-top comment).
	fields := make([]channelevents.InteractionField, len(payloadItems))
	for i, it := range payloadItems {
		value := strings.TrimSpace(it.Description)
		if it.Why != "" {
			if value != "" {
				value += " — "
			}
			value += it.Why
		}
		fields[i] = channelevents.InteractionField{Label: it.Title, Value: value}
	}

	// The requester's channel-scoped identity, addressed the same way
	// every other publisher in this codebase addresses a channel-attributed
	// user (see pkg/channels/channelsd/pipeline/pipeline.go's toEnvelopeIdentity /
	// ExternalIdentity{Kind: ev.Channel.Spec.Kind, ...} callers): Kind is
	// the session's OWN channel kind (denormalized onto InputChannel.Kind),
	// not a hardcoded "slack" — the same envelope is routed by the outbound
	// relay to whichever channel kind's "interaction" sub-channel sender is
	// bound to this session, and only Slack's sender additionally checks
	// Kind=="slack" defensively before resolving it. ExternalID + Email carry
	// the natural raw+email form (started-by annotations, stamped by
	// backfill.go) rather than a precomputed canonical stuffed into
	// ExternalID — the Slack sender derives the canonical itself via
	// r.Principal().AllowSynthetic().Canonical() before calling
	// resolveSlackUserIDFromCanonical, matching the exact form
	// pkg/channels/channelkinds/slack/resolve_canonical.go's
	// resolveSlackUserIDFromCanonical expects downstream of that derivation.
	starterIdentity := channelevents.ExternalIdentity{
		Kind:       identity.Kind(sess.Spec.InputChannel.Kind),
		ExternalID: spiceboxv1alpha1.StartedByExternalID(sess),
		Email:      spiceboxv1alpha1.StartedByEmail(sess),
	}
	if starterIdentity.Email == "" {
		// No verified email on record for the started-by user: Principal()'s
		// synthetic canonical (base64(kind:teamScope:externalID)) can't be
		// safely re-derived here — there is no StartedByTeamScope annotation
		// preserving the TeamScope the ORIGINAL canonical was minted with, so
		// a re-derivation with an empty TeamScope could diverge from the
		// bytes `starter` (StartedBySubject) holds. Fall back to the exact
		// precomputed canonical via the Subject passthrough so delivery
		// stays byte-identical even off the email-present happy path.
		starterIdentity.Subject = starter
	}
	expiresAt := now.Add(timeout)

	reqPl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.CredentialLink,
		RequestRef:      mintRequestID(),
		Lead:            "Connect your accounts",
		Fields:          fields,
		Actions: []channelevents.InteractionAction{
			{
				ID:    "connect_accounts",
				Label: buttonLabel,
				Style: channelevents.ActionStylePrimary,
				Kind:  channelevents.ActionKindLink,
				URL:   linkURL,
			},
		},
		Audience:  channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &starterIdentity},
		ExpiresAt: &expiresAt,
	}
	if err := reqPl.Validate(); err != nil {
		return fmt.Errorf("credential_request: built an invalid interaction_request payload (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name, channelevents.KindInteractionRequest, reqPl); err != nil {
		return fmt.Errorf("publish credential interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}

	// Best-effort: flip the channel-thread status indicator from the
	// generic "<agentClass> is starting…" placeholder to a specific
	// "waiting for identity setup…" so the user understands why the
	// agent appears paused. NATSPublish is guaranteed non-nil here (the
	// doPublish entry guard), but a transport failure is still just
	// logged, not fatal — the interaction_request just published is what
	// actually unblocks the user.
	w.publishWaitingStatus(ctx, sess, &ac, acFound, logger)

	// Publish succeeded — mark dedup so the next tick skips. We patch the
	// Status sub-resource directly via MergeFrom so a concurrent
	// reconciler that touched a different field doesn't 409.
	// markPublished stamps the dedup condition — only meaningful on the FIRST
	// publish. On a re-surface (ForcePublish) the condition is already stamped,
	// so re-stamping it would rewrite the condition Message with each freshly
	// minted link URL, churning the session's status/resourceVersion on every
	// re-interaction. Skip when already published; still stamp on the rare race
	// where a re-surface beats the watcher's first publish (dedup not yet set).
	if !alreadyPublished(sess) {
		if err := w.markPublished(ctx, sess, len(missing), linkURL); err != nil {
			// Patch failed AFTER a successful publish: the user has been
			// prompted, but the dedup condition isn't recorded — next tick
			// will re-publish and double-prompt. Log loudly so operators can
			// see the duplication source.
			logger.Info("credential_request published but condition patch failed; next tick may re-publish",
				"err", err.Error())
			return fmt.Errorf("patch CredentialRequestPublished condition: %w", err)
		}
	}
	logger.Info("credential_request published",
		"recipient", starter,
		"missingCount", len(missing),
		"buttonLabel", buttonLabel,
		"linkTimeout", timeout.String())
	return nil
}

// publishWaitingStatus emits a KindNotification envelope so the channel
// kind's setStatus surface flips from "<agentClass> is starting…" to
// "<agentClass> is waiting for identity setup…" while the user links
// credentials. Best-effort — a nil publisher or transport failure logs
// and continues; the credential-request DM is the actual unblock path.
//
// agentDisplayLabel: AgentClass.Spec.DisplayName when set, falling back
// to AgentClass.metadata.name, and finally "agent" if neither is
// available.
func (w *CredentialRequestWatcher) publishWaitingStatus(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession,
	ac *spiceboxv1alpha1.AgentClass, acFound bool,
	logger logr.Logger,
) {
	if w.NATSPublish == nil {
		return
	}
	name := "agent"
	if acFound {
		if ac.Spec.DisplayName != "" {
			name = ac.Spec.DisplayName
		} else if ac.Name != "" {
			name = ac.Name
		}
	}
	text := name + " is waiting for identity setup…"
	short := "Waiting for identity…"
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: text, Short: short},
	); err != nil {
		logger.Info("credential_request: publish waiting-status notification failed (best-effort, continuing)",
			"err", err.Error())
	}
}

// handleExternalURLUnconfigured is the fail-closed path taken when
// w.ExternalBaseURL() returns "" — the platform's external web address is
// not configured. It surfaces the misconfiguration on three independent
// surfaces and returns an error so reconcileAll logs it and retries on the
// next tick (recovering automatically once the operator patches the
// spicebox-webd-external-url ConfigMap):
//
//	a. CR status: SessionUserIdentity.CredentialLinkAvailable=False
//	   (session-observable via `kubectl get suid`/`oap` surfaces).
//	b. Monitoring channel: a MonitoringEvent fanned out to every
//	   role=monitoring Channel so an operator sees it without source-diving.
//	c. Session thread: a KindNotification so the waiting user isn't left
//	   with silence or a dead button — they're told linking is temporarily
//	   unavailable and to ask their operator.
//
// The noisy surfaces (b + c) fire only on the FIRST transition into the
// unconfigured state — the 5s watcher tick would otherwise spam them every
// poll. The SUI condition is the dedup key: it is re-read fresh each
// ReconcileOne, so a prior False/WebdExternalURLNotConfigured means
// "already surfaced; just keep retrying."
func (w *CredentialRequestWatcher) handleExternalURLUnconfigured(
	ctx context.Context, sess *spiceboxv1alpha1.AgentSession,
	sui *spiceboxv1alpha1.SessionUserIdentity, logger logr.Logger,
) error {
	const condType = spiceboxv1alpha1.SessionUserIdentityConditionCredentialLinkAvailable
	const reason = spiceboxv1alpha1.ReasonWebdExternalURLNotConfigured
	msg := fmt.Sprintf("the platform's web address is not configured (%s ConfigMap); credential links cannot be generated — set it via `oap install --trusted-hostname` or patch the ConfigMap",
		spiceboxv1alpha1.WebdExternalURLConfigMap)

	prev := conditions.Find(sui.Status.Conditions, condType)
	firstTransition := prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason

	if firstTransition {
		// a. CR status — session-observable. Patch failure is logged but
		// does not abort the other surfaces or the returned error.
		if err := w.patchSUICredentialLinkUnavailable(ctx, sui, condType, reason, msg); err != nil {
			logger.Info("credential_request: patch CredentialLinkAvailable=False failed; will retry next tick",
				"err", err.Error())
		}
		// b. Monitoring channel.
		w.publishExternalURLMonitoring(sess, condType, reason, msg, logger)
		// c. Session-visible notification.
		w.publishCredentialLinkUnavailableNotification(sess, logger)
	}

	// d. Return an error so reconcileAll logs it + the next tick retries.
	return fmt.Errorf("webd external URL not configured (%s ConfigMap); cannot build credential link for session %s/%s",
		spiceboxv1alpha1.WebdExternalURLConfigMap, sess.Namespace, sess.Name)
}

// patchSUICredentialLinkUnavailable stamps CredentialLinkAvailable=False on
// the SessionUserIdentity status sub-resource via a MergeFrom patch so a
// concurrent writer touching a different field doesn't 409.
func (w *CredentialRequestWatcher) patchSUICredentialLinkUnavailable(
	ctx context.Context, sui *spiceboxv1alpha1.SessionUserIdentity,
	condType, reason, msg string,
) error {
	prior := sui.DeepCopy()
	conditions.SetFalse(sui, &sui.Status.Conditions, condType, reason, msg)
	if err := w.K8s.Status().Patch(ctx, sui, client.MergeFrom(prior)); err != nil {
		return fmt.Errorf("patch SessionUserIdentity %s/%s %s condition: %w",
			sui.Namespace, sui.Name, condType, err)
	}
	return nil
}

// publishExternalURLMonitoring fans a MonitoringEvent out to every
// role=monitoring Channel describing the unconfigured-external-URL failure.
// Best-effort: a nil publisher returns early; a publish error is logged (not
// swallowed) — the SUI condition + returned error are the load-bearing
// surfaces.
func (w *CredentialRequestWatcher) publishExternalURLMonitoring(
	sess *spiceboxv1alpha1.AgentSession,
	condType, reason, summary string, logger logr.Logger,
) {
	if w.NATSPublish == nil {
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "AgentSession",
			Namespace: sess.Namespace,
			Name:      sess.Name,
		},
		Condition: condType,
		Reason:    reason,
		Summary:   summary,
		Hint: fmt.Sprintf("set the %s ConfigMap (re-run `oap install --trusted-hostname` or patch it)",
			spiceboxv1alpha1.WebdExternalURLConfigMap),
		Timestamp: w.now(),
	}
	if err := channelevents.PublishMonitoring(w.NATSPublish, ev); err != nil {
		logger.Info("credential_request: publish external-URL-unconfigured monitoring event failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
}

// publishCredentialLinkUnavailableNotification emits a KindNotification on
// the session's out-subject so the waiting user sees an explanation instead
// of silence or a dead button. We use the notification surface (not the
// credential_request sub-channel sender) deliberately: that sender hard-
// requires a link button and would itself error on a buttonless payload —
// a status notification is the least-surface mechanism that tells the user
// linking is temporarily unavailable. Best-effort: nil publisher returns
// early; a publish error is logged.
func (w *CredentialRequestWatcher) publishCredentialLinkUnavailableNotification(
	sess *spiceboxv1alpha1.AgentSession, logger logr.Logger,
) {
	if w.NATSPublish == nil {
		return
	}
	text := "Credential linking is temporarily unavailable: the platform's web address isn't configured. Please ask your operator to set it; the agent will resume automatically once it is."
	short := "Credential linking unavailable…"
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: text, Short: short},
	); err != nil {
		logger.Info("credential_request: publish credential-link-unavailable notification failed (best-effort, continuing)",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
}

// singleButtonLabel resolves the user-facing label for the single
// channel button. With one missing credential we name the provider
// concretely ("Connect Linear") so the click is unambiguous; with more
// than one we use the generic label since the menu page (rendered by
// identityd's /link handler) enumerates them.
//
// Provider resolution falls through MCPServer.Spec.Auth.Provider →
// MCPServer.metadata.name → the credential name itself, mirroring
// passthroughcatalog.ProviderLabel. A lookup failure (no MCPServer
// for this credential, e.g. envvar-style PAT bindings) falls back to
// a humanized credential name.
func (w *CredentialRequestWatcher) singleButtonLabel(
	ctx context.Context,
	logger logr.Logger,
	missing []string,
) string {
	if len(missing) != 1 {
		return "Connect your accounts"
	}
	credName := missing[0]
	if srv, err := passthroughcatalog.LookupMCPServerByCredential(ctx, w.K8s, credName); err == nil {
		if label := passthroughcatalog.ProviderLabel(srv); label != "" {
			return "Connect " + label
		}
	} else {
		// No backing MCPServer (e.g. agentIdentity envvar PAT). Surface
		// the lookup miss at INFO for grep-ability, then fall through to
		// humanizing the credential name.
		logger.Info("credential_request: single-credential MCPServer lookup miss; falling back to humanized credName",
			"cred", credName, "err", err.Error())
	}
	return "Connect " + humanizeCredName(credName)
}

// humanizeCredName turns a kebab/snake credential name into a
// title-cased label. Used as a last-resort label when no MCPServer
// declares a provider — e.g. envvar PAT bindings like github-token.
func humanizeCredName(credName string) string {
	if credName == "" {
		return "your account"
	}
	// Split on - and _ and title-case each word.
	parts := strings.FieldsFunc(credName, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// markPublished stamps the dedup condition True on the AgentSession's
// status sub-resource. Uses a status patch so it doesn't conflict with
// spec edits in flight elsewhere.
func (w *CredentialRequestWatcher) markPublished(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, missingCount int, linkURL string) error {
	patched := sess.DeepCopy()
	msg := fmt.Sprintf("Delivered credential link prompt covering %d missing credential(s).", missingCount)
	// Include a hint of the link in the condition message for debug
	// observability — the link is signed but not secret, and the
	// SUI/AgentSession YAML output should at least surface that a
	// prompt was sent.
	if linkURL != "" {
		end := len(linkURL)
		if end > 64 {
			end = 64
		}
		msg = msg + " URL prefix=" + linkURL[:end]
	}
	conditions.Set(patched, &patched.Status.Conditions, metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonCredentialRequestPublished,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(w.now()),
	})
	return applyApprovalStatus(ctx, w.K8s, patched, sess)
}

// alreadyPublished reports whether the dedup condition is already True
// on sess.
func alreadyPublished(sess *spiceboxv1alpha1.AgentSession) bool {
	return conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialRequestPublished)
}

// buildLinkURL stitches the externalBaseURL with the signed link
// payload + signature. passthroughlink.Mint returns "<b64>.<sig>";
// identityd parses the URL by separately decoding ?d= and ?sig=
// (handlers_link.go). Splitting here mirrors that consumer.
//
// Defense in depth: externalBaseURL MUST be an absolute http(s) URL with a
// non-empty host. An empty or hostless base would yield a relative
// "/link?…" link (the localhost-seed regression) — refuse it so no caller
// can ever emit a hostless credential button. ReconcileOne already
// fails-closed on an empty externalURL upstream; this is the last-line
// guard for every other call path.
func buildLinkURL(externalBaseURL, raw string) (string, error) {
	idx := strings.IndexByte(raw, '.')
	if idx <= 0 || idx == len(raw)-1 {
		return "", errors.New("malformed signed link: missing or empty halves")
	}
	base := strings.TrimRight(externalBaseURL, "/")
	if base == "" {
		return "", errors.New("external base URL is empty: cannot build an absolute credential link")
	}
	u, perr := url.Parse(base)
	if perr != nil {
		return "", fmt.Errorf("external base URL %q is not parseable: %w", externalBaseURL, perr)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("external base URL %q is not an absolute http(s) URL (scheme=%q host=%q)", externalBaseURL, u.Scheme, u.Host)
	}
	b64, sig := raw[:idx], raw[idx+1:]
	q := url.Values{}
	q.Set("d", b64)
	q.Set("sig", sig)
	return base + "/link?" + q.Encode(), nil
}

func (w *CredentialRequestWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// buildExplainerInput assembles an explainer.Input for the credentials
// whose Why is empty (the gaps). Each gap's provider label comes from the
// matching item's Title (deterministic), falling back to a humanized
// credential name. out.What from the explainer is ignored — titles are
// owned by the operator now.
//
// Prompt-injection boundary: initiatingMessage is attacker-controlled
// and passed into explainer.Input.InitiatingMessage, where the
// explainer fences it in XML delimiters. The other fields are
// operator-controlled.
func buildExplainerInput(initiatingMessage string, gaps []string, items []spiceboxv1alpha1.CredentialExplanationItem, ac *spiceboxv1alpha1.AgentClass, acFound bool) explainer.Input {
	titleByCred := make(map[string]string, len(items))
	for _, it := range items {
		titleByCred[it.Credential] = it.Title
	}
	creds := make([]explainer.CredentialInfo, 0, len(gaps))
	for _, name := range gaps {
		provider := titleByCred[name]
		if provider == "" {
			provider = humanizeCredName(name)
		}
		creds = append(creds, explainer.CredentialInfo{Name: name, Provider: provider})
	}
	agentName := ""
	if acFound {
		agentName = ac.Spec.DisplayName
		if agentName == "" {
			agentName = ac.Name
		}
	}
	return explainer.Input{
		InitiatingMessage: initiatingMessage,
		Credentials:       creds,
		AgentDisplayName:  agentName,
	}
}

// credsNeedingWhy returns the credential names whose Why is empty (the
// rows the explainer/static fallback must fill).
func credsNeedingWhy(items []spiceboxv1alpha1.CredentialExplanationItem) []string {
	var out []string
	for _, it := range items {
		if strings.TrimSpace(it.Why) == "" {
			out = append(out, it.Credential)
		}
	}
	return out
}

// fillWhys writes the explainer's per-gap whys back into the why slice at
// the positions of the gap credentials.
func fillWhys(why []string, items []spiceboxv1alpha1.CredentialExplanationItem, gaps, filled []string) {
	pos := map[string]int{}
	for i, it := range items {
		pos[it.Credential] = i
	}
	for i, cred := range gaps {
		if idx, ok := pos[cred]; ok && strings.TrimSpace(filled[i]) != "" {
			why[idx] = filled[i]
		}
	}
}

// agentClassDisplayName returns the AgentClass label for the static why
// sentence: DisplayName, then Name, then "the agent" (also when the class
// was not found).
func agentClassDisplayName(ac *spiceboxv1alpha1.AgentClass, found bool) string {
	if !found {
		return "the agent"
	}
	if ac.Spec.DisplayName != "" {
		return ac.Spec.DisplayName
	}
	if ac.Name != "" {
		return ac.Name
	}
	return "the agent"
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
