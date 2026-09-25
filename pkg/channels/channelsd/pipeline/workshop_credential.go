// pkg/channels/channelsd/pipeline/workshop_credential.go
//
// WorkshopCredentialWatcher is the channelsd-side bridge between the workshop
// sidecar's request_credential tool (which records a bot-credential request
// on Workshop.spec.credentialRequests via its update grant on that one CR)
// and the user-visible "Connect a credential" prompt delivered to the
// builder session's requester.
//
// It is the Workshop-CR analog of CredentialRequestWatcher
// (credential_request.go): per tick it lists Workshops carrying undelivered
// spec.credentialRequests entries, resolves the builder AgentSession
// (spec.session), mints a signed passthroughlink.Payload{Purpose:
// PurposeWorkshopCredential} deep-link, builds an InteractionRequestPayload
// (category=workshop_credential), and PUBLISHES it on the builder session's
// .out subject. The outbound relay then resolves the bound channel kind's
// "interaction" sub-channel sender and delivers it.
//
// Unlike CredentialRequestWatcher, this credential belongs to the workshop
// bot's OWN AgentIdentity, not the builder session's — so delivery never
// parks the builder (categories.WorkshopCredential carries no Park phase;
// see that category's registration doc). The builder keeps authoring while
// the requester connects the credential asynchronously.
//
// Dedup is a per-(identity, credential) delivery stamp on
// Workshop.status.credentialRequests, written SOLELY by this watcher — the
// sidecar holds no update on the status subresource.
package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// WorkshopCredentialWatcherInterval is the polling cadence. Slower than
// CredentialRequestWatcherInterval (5s): this flow does not park anything —
// nobody is blocked waiting on the next tick — so a coarser ceiling on
// time-to-prompt is acceptable.
const WorkshopCredentialWatcherInterval = 15 * time.Second

// DefaultWorkshopCredentialLinkTimeout is the fallback link ExpiresAt window
// when LinkTimeout is zero. Longer than DefaultCredentialLinkTimeout (30m):
// that gate blocks the builder session's own progress and so wants a short
// fuse, while this credential belongs to the workshop bot under
// construction — the requester can act on their own schedule while the
// builder keeps working.
const DefaultWorkshopCredentialLinkTimeout = 24 * time.Hour

// WorkshopCredentialWatcher polls Workshops, mints workshop_credential links,
// and publishes KindInteractionRequest (category=workshop_credential)
// envelopes on the builder session's .out subject. Mirrors
// CredentialRequestWatcher's shape.
type WorkshopCredentialWatcher struct {
	// K8s is the client used to List Workshops, Get the builder AgentSession,
	// and Patch the per-entry delivery stamp onto Workshop.status.
	K8s client.Client

	// LinkSigner mints the HMAC-signed deep-link payload. Shared with
	// identityd via the spicebox-passthrough-link-key Secret.
	LinkSigner *passthroughlink.Signer

	// ExternalBaseURL returns the identityd-reachable URL the link embeds. A
	// getter (not a string) so the external URL can change without
	// restarting the pod. Required.
	ExternalBaseURL func() string

	// LinkTimeout overrides DefaultWorkshopCredentialLinkTimeout. Zero means
	// use the default. Tests set it to a known value so they can assert on
	// the link's ExpiresAt.
	LinkTimeout time.Duration

	// Now is the time source for ExpiresAt. Tests inject a fixed value;
	// production code leaves it nil to default to time.Now().UTC().
	Now func() time.Time

	// NATSPublish delivers the KindInteractionRequest envelope that IS the
	// workshop_credential prompt. Required: Run refuses to start when nil, and
	// ReconcileOne fails loud on a direct call — a silent no-op here would
	// leave the request recorded on spec forever with no card ever delivered.
	NATSPublish channelevents.PublishFunc
}

// Run polls until ctx is canceled. Mirrors CredentialRequestWatcher.Run.
func (w *WorkshopCredentialWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("workshopcredential-watcher")
	if w.LinkSigner == nil || w.ExternalBaseURL == nil || w.NATSPublish == nil {
		// A misconfigured channelsd would silently fail to ever deliver the
		// card. Refuse to run rather than fail invisibly, same as
		// CredentialRequestWatcher.Run.
		logger.Info("watcher disabled: missing LinkSigner, ExternalBaseURL, or NATSPublish")
		return
	}
	ticker := time.NewTicker(WorkshopCredentialWatcherInterval)
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

// reconcileAll lists every Workshop and dispatches those carrying
// spec.credentialRequests to ReconcileOne. Errors on per-workshop work are
// logged but not returned — the next tick retries.
func (w *WorkshopCredentialWatcher) reconcileAll(ctx context.Context, logger logr.Logger) {
	var workshops spiceboxv1alpha1.WorkshopList
	if err := w.K8s.List(ctx, &workshops); err != nil {
		logger.Error(err, "list workshops")
		return
	}
	for i := range workshops.Items {
		ws := &workshops.Items[i]
		if len(ws.Spec.CredentialRequests) == 0 {
			continue
		}
		if err := w.ReconcileOne(ctx, ws); err != nil {
			logger.Info("workshop_credential reconcile failed",
				"workshop", ws.Namespace+"/"+ws.Name,
				"err", err.Error())
			// Next tick retries; per-workshop failure does not abort the loop.
			continue
		}
	}
}

// ReconcileOne does the per-workshop work: resolve the builder session once,
// then deliver a card for every undelivered entry in spec.credentialRequests
// — the minted link + card are identical regardless of AuthKind; the type
// branch (pat/static paste vs. oauth-mcp connect) lives at identityd, not
// here. Exported (not just lowercase) so tests can drive it directly without
// spinning up a polling loop.
//
// A per-entry mint/publish/stamp failure is logged and the loop continues to
// the next entry — one bad entry must not block delivery of the others. Only
// a structural failure (no NATSPublish configured, or a non-NotFound error
// resolving the builder session) is returned, so reconcileAll's log line
// names the workshop as a whole.
func (w *WorkshopCredentialWatcher) ReconcileOne(ctx context.Context, ws *spiceboxv1alpha1.Workshop) error {
	logger := log.FromContext(ctx).WithName("workshopcredential-watcher").
		WithValues("workshop", ws.Namespace+"/"+ws.Name)

	// Fail fast on a wiring bug: without a NATS publisher there is no way to
	// deliver the card at all. Checked before any K8s Get or mint work, same
	// as CredentialRequestWatcher.doPublish.
	if w.NATSPublish == nil {
		return fmt.Errorf("workshop_credential: NATSPublish not configured (wiring bug); cannot deliver for workshop %s/%s", ws.Namespace, ws.Name)
	}
	if len(ws.Spec.CredentialRequests) == 0 {
		return nil
	}

	if ws.Status.Namespace == "" {
		// The workshop hasn't finished provisioning yet — AgentIdentityRef
		// needs the workshop namespace. Not an error; the next tick retries
		// once provisioning stamps it.
		logger.Info("workshop has no status.namespace yet; will retry next tick")
		return nil
	}

	sessKey := client.ObjectKey{Namespace: ws.Spec.Session.Namespace, Name: ws.Spec.Session.Name}
	var sess spiceboxv1alpha1.AgentSession
	if err := w.K8s.Get(ctx, sessKey, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("builder AgentSession not found; will retry next tick",
				"session", sessKey.Namespace+"/"+sessKey.Name)
			return nil
		}
		return fmt.Errorf("get builder AgentSession %s/%s: %w", sessKey.Namespace, sessKey.Name, err)
	}

	if sess.Spec.InputChannel == nil {
		// No bound channel to publish on — nothing to deliver to.
		logger.Info("builder session has no InputChannel; will retry next tick",
			"session", sessKey.Namespace+"/"+sessKey.Name)
		return nil
	}

	starter := spiceboxv1alpha1.StartedBySubject(&sess)
	if starter == "" {
		// kubectl-driven session has no started-by annotation. Without a
		// recipient the channel kind has no one to address the card to.
		logger.Info("builder session has no started-by canonical-id annotation; will retry next tick",
			"session", sessKey.Namespace+"/"+sessKey.Name)
		return nil
	}

	// The requester's channel-scoped identity, addressed the same way
	// CredentialRequestWatcher.doPublish and PortalAccessTriggerer.TryHandle
	// address the session starter: Kind is the builder session's OWN channel
	// kind, ExternalID+Email carry the natural raw+email form, and Subject is
	// the precomputed canonical fallback when no verified email is on record
	// (see those callers' comments for why an empty TeamScope can't safely
	// re-derive the synthetic canonical here).
	starterIdentity := channelevents.ExternalIdentity{
		Kind:       identity.Kind(sess.Spec.InputChannel.Kind),
		ExternalID: spiceboxv1alpha1.StartedByExternalID(&sess),
		Email:      spiceboxv1alpha1.StartedByEmail(&sess),
	}
	if starterIdentity.Email == "" {
		starterIdentity.Subject = starter
	}

	for i := range ws.Spec.CredentialRequests {
		cr := ws.Spec.CredentialRequests[i]
		entryLogger := logger.WithValues("identity", cr.Identity, "credential", cr.Credential)

		if workshopCredentialDelivered(ws, cr.Identity, cr.Credential) {
			continue
		}
		if err := w.deliverOne(ctx, ws, &sess, starterIdentity, cr); err != nil {
			entryLogger.Info("workshop_credential deliver failed; will retry next tick",
				"err", err.Error())
			// Continue to the next entry — one bad row must not block the rest.
		}
	}
	return nil
}

// deliverOne mints the signed link for one credentialRequests entry, builds
// and publishes the workshop_credential interaction_request, and stamps the
// delivery onto Workshop.status.credentialRequests.
func (w *WorkshopCredentialWatcher) deliverOne(
	ctx context.Context,
	ws *spiceboxv1alpha1.Workshop,
	sess *spiceboxv1alpha1.AgentSession,
	starterIdentity channelevents.ExternalIdentity,
	cr spiceboxv1alpha1.WorkshopCredentialRequest,
) error {
	now := w.now()
	raw, err := w.LinkSigner.Mint(passthroughlink.Payload{
		Purpose:             passthroughlink.PurposeWorkshopCredential,
		SessionRef:          ws.Spec.Session.Namespace + "/" + ws.Spec.Session.Name,
		AgentIdentityRef:    ws.Status.Namespace + "/" + cr.Identity,
		RequiredCredentials: []string{cr.Credential},
		ExpiresAt:           now.Add(w.linkTimeout()).Unix(),
		JTI:                 mintRequestID(),
	})
	if err != nil {
		return fmt.Errorf("mint workshop credential link: %w", err)
	}

	externalURL := w.ExternalBaseURL()
	if externalURL == "" {
		// Fail closed: an unconfigured external URL would produce a hostless
		// link. Refuse to emit a broken button; the next tick retries once
		// the ConfigMap is fixed.
		return fmt.Errorf("external base URL is empty: cannot build workshop credential link for workshop %s/%s", ws.Namespace, ws.Name)
	}
	linkURL, err := buildLinkURL(externalURL, raw)
	if err != nil {
		return fmt.Errorf("build workshop credential link URL: %w", err)
	}

	reqPl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.WorkshopCredential,
		RequestRef:      mintRequestID(),
		Lead:            "Connect a credential for the agent you're building",
		Actions: []channelevents.InteractionAction{
			{
				ID:    "connect_credential",
				Label: "Connect",
				Style: channelevents.ActionStylePrimary,
				Kind:  channelevents.ActionKindLink,
				URL:   linkURL,
			},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &starterIdentity,
		},
	}
	if err := reqPl.Validate(); err != nil {
		return fmt.Errorf("built an invalid workshop_credential interaction_request payload (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name, channelevents.KindInteractionRequest, reqPl); err != nil {
		return fmt.Errorf("publish workshop_credential interaction_request (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}

	if err := w.markDelivered(ctx, ws, cr.Identity, cr.Credential, reqPl.RequestRef); err != nil {
		// Published successfully, but the delivery stamp failed to persist:
		// the next tick will re-publish and double-prompt. Log loudly at the
		// call site (entryLogger in ReconcileOne) so operators can see the
		// duplication source; the trade mirrors markPublished's identical one
		// in credential_request.go.
		return fmt.Errorf("stamp workshop %s/%s status.credentialRequests[%s/%s]: %w", ws.Namespace, ws.Name, cr.Identity, cr.Credential, err)
	}
	return nil
}

// markDelivered stamps DeliveredAt + NoticeRef onto the matching
// (identity, credential) row in Workshop.status.credentialRequests —
// appending a new row when this is the first delivery for that pair. Uses a
// status patch (MergeFrom) so a concurrent writer touching a different field
// doesn't 409. This watcher is the sole writer of status.credentialRequests,
// so there is no other concurrent mutator of this specific list to race.
func (w *WorkshopCredentialWatcher) markDelivered(ctx context.Context, ws *spiceboxv1alpha1.Workshop, identityName, credential, noticeRef string) error {
	original := ws.DeepCopy()
	deliveredAt := metav1.NewTime(w.now())

	found := false
	for i := range ws.Status.CredentialRequests {
		st := &ws.Status.CredentialRequests[i]
		if st.Identity == identityName && st.Credential == credential {
			st.DeliveredAt = &deliveredAt
			st.NoticeRef = noticeRef
			found = true
			break
		}
	}
	if !found {
		ws.Status.CredentialRequests = append(ws.Status.CredentialRequests, spiceboxv1alpha1.WorkshopCredentialRequestStatus{
			Identity:    identityName,
			Credential:  credential,
			DeliveredAt: &deliveredAt,
			NoticeRef:   noticeRef,
		})
	}
	return w.K8s.Status().Patch(ctx, ws, client.MergeFrom(original))
}

// workshopCredentialDelivered reports whether ws.status.credentialRequests
// already carries a non-nil DeliveredAt for (identityName, credential) — the
// dedup key. A row present with DeliveredAt nil (shouldn't happen given this
// watcher only ever writes a row with DeliveredAt set) is treated as
// undelivered, not skipped.
func workshopCredentialDelivered(ws *spiceboxv1alpha1.Workshop, identityName, credential string) bool {
	for _, st := range ws.Status.CredentialRequests {
		if st.Identity == identityName && st.Credential == credential {
			return st.DeliveredAt != nil
		}
	}
	return false
}

func (w *WorkshopCredentialWatcher) linkTimeout() time.Duration {
	if w.LinkTimeout != 0 {
		return w.LinkTimeout
	}
	return DefaultWorkshopCredentialLinkTimeout
}

func (w *WorkshopCredentialWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}
