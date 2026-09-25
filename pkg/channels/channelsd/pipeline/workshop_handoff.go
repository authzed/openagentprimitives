// pkg/channels/channelsd/pipeline/workshop_handoff.go
//
// WorkshopHandoffWatcher is the channelsd-side bridge between the workshop
// sidecar's install/capability handoff (which records Workshop.spec.installRequest
// / spec.capabilityRequest via its update grant on this workshop's own CR) and
// the platform-admin-visible notice that a workshop is ready for a human
// decision. It mirrors WorkshopCredentialWatcher's polling shape
// (workshop_credential.go) but delivers to a DIFFERENT audience through a
// DIFFERENT surface:
//
//   - workshop_credential prompts the SESSION's own requester, through a
//     signed passthrough deep-link, because the person who can connect a bot
//     credential is whoever is already in the builder's thread.
//   - install/capability requests need a PLATFORM ADMIN, and platform admins
//     are not generally in any one builder's thread. The only mechanism that
//     reaches them cluster-wide is a MonitoringEvent fanned out to every
//     role=monitoring Channel (see monitoring_recipient.go) — the same
//     surface CredentialUpdateWatcher uses for an agent-owned credential.
//
// The card's action therefore links to the admin console (admin-authenticated
// already) rather than minting a subject-bound signed link: there is no
// per-admin identity to bind a passthrough link to, and the console itself
// gates the actual install/decline on the admin's own session.
//
// Dedup is a per-Workshop delivery stamp on status.install.deliveredAt /
// status.capabilityRequest.deliveredAt, written SOLELY by this watcher for
// those fields — status.install's Phase progression past Requested
// (Approved/Installed/Declined/Failed) and ApprovedBy/InstalledRef/Message
// belong to admind (a later task); this watcher never touches them.
package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// WorkshopHandoffWatcherInterval is the polling cadence. Matches
// WorkshopCredentialWatcherInterval: like that flow, nobody is parked waiting
// on the next tick, so a coarser ceiling on time-to-notice is acceptable.
const WorkshopHandoffWatcherInterval = 15 * time.Second

// WorkshopHandoffWatcher polls Workshops, and for each undelivered
// spec.installRequest / spec.capabilityRequest publishes ONE admin
// MonitoringEvent card, then stamps the observed status so it delivers
// exactly once.
type WorkshopHandoffWatcher struct {
	// K8s is the client used to List Workshops, Get the builder AgentSession,
	// resolve a monitoring recipient, and Patch the delivery stamp onto
	// Workshop.status.
	K8s client.Client

	// ExternalBaseURL returns the webd-reachable base URL the card's console
	// link embeds. A getter (not a string) so the external URL can change
	// without restarting the pod. Required.
	ExternalBaseURL func() string

	// NATSPublish delivers the MonitoringEvent. Required: Run refuses to start
	// when nil, and ReconcileOne fails loud on a direct call — a silent no-op
	// here would leave the request recorded on spec forever with no card ever
	// delivered.
	NATSPublish channelevents.PublishFunc

	// Now is the time source for the delivery timestamps. Tests inject a fixed
	// value; production code leaves it nil to default to time.Now().UTC().
	Now func() time.Time
}

// Run polls until ctx is canceled. Mirrors WorkshopCredentialWatcher.Run.
// Unlike that watcher there is no LinkSigner to guard on: the card links to
// the admin console, which needs no signed passthrough.
func (w *WorkshopHandoffWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("workshophandoff-watcher")
	if w.ExternalBaseURL == nil || w.NATSPublish == nil {
		// A misconfigured channelsd would silently fail to ever deliver the
		// card. Refuse to run rather than fail invisibly.
		logger.Info("watcher disabled: missing ExternalBaseURL or NATSPublish")
		return
	}
	ticker := time.NewTicker(WorkshopHandoffWatcherInterval)
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

// reconcileAll lists every Workshop and dispatches those carrying an
// undelivered install or capability request to ReconcileOne. Errors on
// per-workshop work are logged but not returned — the next tick retries.
func (w *WorkshopHandoffWatcher) reconcileAll(ctx context.Context, logger logr.Logger) {
	var workshops spiceboxv1alpha1.WorkshopList
	if err := w.K8s.List(ctx, &workshops); err != nil {
		logger.Error(err, "list workshops")
		return
	}
	for i := range workshops.Items {
		ws := &workshops.Items[i]
		if !needsInstallCard(ws) && !needsCapabilityCard(ws) {
			continue
		}
		if err := w.ReconcileOne(ctx, ws); err != nil {
			logger.Info("workshop_handoff reconcile failed",
				"workshop", ws.Namespace+"/"+ws.Name,
				"err", err.Error())
			// Next tick retries; per-workshop failure does not abort the loop.
			continue
		}
	}
}

// needsInstallCard reports whether ws carries an install request this
// watcher has not yet delivered a card for.
func needsInstallCard(ws *spiceboxv1alpha1.Workshop) bool {
	return ws.Spec.InstallRequest != nil && (ws.Status.Install == nil || ws.Status.Install.DeliveredAt == nil)
}

// needsCapabilityCard reports whether ws carries a capability request this
// watcher has not yet delivered a card for.
func needsCapabilityCard(ws *spiceboxv1alpha1.Workshop) bool {
	return ws.Spec.CapabilityRequest != nil && (ws.Status.CapabilityRequest == nil || ws.Status.CapabilityRequest.DeliveredAt == nil)
}

// ReconcileOne does the per-workshop work: resolve the builder session once
// (a structural precondition shared by both cards), then deliver whichever of
// the install/capability cards is still undelivered. Exported so tests can
// drive it directly without spinning up a polling loop.
//
// A per-card mint/publish/stamp failure is logged and the loop continues to
// the other card — one failing card must not block delivery of the other.
// Only a structural failure (no NATSPublish configured, or a non-NotFound
// error resolving the builder session) is returned, so reconcileAll's log
// line names the workshop as a whole.
func (w *WorkshopHandoffWatcher) ReconcileOne(ctx context.Context, ws *spiceboxv1alpha1.Workshop) error {
	logger := log.FromContext(ctx).WithName("workshophandoff-watcher").
		WithValues("workshop", ws.Namespace+"/"+ws.Name)

	// Fail fast on a wiring bug: without a NATS publisher there is no way to
	// deliver either card at all. Checked before any K8s Get, same as
	// WorkshopCredentialWatcher.ReconcileOne.
	if w.NATSPublish == nil {
		return fmt.Errorf("workshop_handoff: NATSPublish not configured (wiring bug); cannot deliver for workshop %s/%s", ws.Namespace, ws.Name)
	}

	needInstall := needsInstallCard(ws)
	needCapability := needsCapabilityCard(ws)
	if !needInstall && !needCapability {
		return nil
	}

	// The builder AgentSession is read as a structural precondition shared by
	// both cards: a workshop naming a session that no longer exists (or never
	// existed) has nobody to attribute the card to, and is not a failure worth
	// retrying loudly -- the next tick tries again on its own.
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

	if needInstall {
		if err := w.deliverInstall(ctx, ws, logger); err != nil {
			logger.Info("install_request card delivery failed; will retry next tick", "err", err.Error())
			// Continue to the capability card — one failing card must not
			// block delivery of the other.
		}
	}
	if needCapability {
		if err := w.deliverCapability(ctx, ws, logger); err != nil {
			logger.Info("capability_request card delivery failed; will retry next tick", "err", err.Error())
		}
	}
	return nil
}

// deliverInstall builds, validates and publishes the install_request
// MonitoringEvent, then stamps status.install so it delivers exactly once.
func (w *WorkshopHandoffWatcher) deliverInstall(ctx context.Context, ws *spiceboxv1alpha1.Workshop, logger logr.Logger) error {
	consoleURL, err := w.consoleURL()
	if err != nil {
		// Fail closed: an unconfigured external URL would produce a hostless
		// link. Refuse to emit a broken card; the next tick retries once the
		// ConfigMap is fixed. Mirrors credential_update.go's
		// handleExternalURLUnconfigured — never stamp delivered for a card
		// that never had a working link.
		logger.Info("external URL not configured; skipping install_request card")
		return err
	}

	ok, whyNot, err := monitoringRecipientExists(ctx, w.K8s)
	if err != nil {
		return fmt.Errorf("resolve a monitoring recipient for workshop %s/%s: %w", ws.Namespace, ws.Name, err)
	}
	if !ok {
		// Fail closed: no deliverable role=monitoring Channel means no admin
		// can see this card. Log + skip + do NOT stamp, so the next tick
		// retries once a Channel is configured.
		logger.Info("no deliverable role=monitoring Channel; skipping install_request card", "detail", whyNot)
		return fmt.Errorf("no deliverable role=monitoring Channel for workshop %s/%s: %s", ws.Namespace, ws.Name, whyNot)
	}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "install_request",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "Workshop",
			Namespace: ws.Namespace,
			Name:      ws.Name,
		},
		Condition: "WorkshopInstallRequested",
		// Every builder-supplied field (SuggestedName) is run through
		// sanitizeCardText before it reaches the card -- the card-injection
		// defense credential_update.go's publishAgentOwnedMonitoring uses for
		// the same reason.
		Summary: "A workshop is ready to install: " + sanitizeCardText(ws.Spec.InstallRequest.SuggestedName) +
			" (session " + ws.Spec.Session.Namespace + "/" + ws.Spec.Session.Name + ").",
		Hint:      "Review and install: " + consoleURL,
		Timestamp: w.now(),
	}
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("built an invalid install_request MonitoringEvent (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}
	if err := channelevents.PublishMonitoring(w.NATSPublish, ev); err != nil {
		return fmt.Errorf("publish install_request monitoring event (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}

	if err := w.stampInstallDelivered(ctx, ws); err != nil {
		// Published successfully, but the delivery stamp failed to persist:
		// the next tick will re-publish and double-notify. Logged loudly at
		// the call site so operators can see the duplication source, the same
		// trade WorkshopCredentialWatcher.deliverOne makes.
		return fmt.Errorf("stamp workshop %s/%s status.install: %w", ws.Namespace, ws.Name, err)
	}
	logger.Info("install_request card delivered")
	return nil
}

// deliverCapability builds, validates and publishes the capability_request
// MonitoringEvent, then stamps status.capabilityRequest so it delivers
// exactly once.
func (w *WorkshopHandoffWatcher) deliverCapability(ctx context.Context, ws *spiceboxv1alpha1.Workshop, logger logr.Logger) error {
	consoleURL, err := w.consoleURL()
	if err != nil {
		logger.Info("external URL not configured; skipping capability_request card")
		return err
	}

	ok, whyNot, err := monitoringRecipientExists(ctx, w.K8s)
	if err != nil {
		return fmt.Errorf("resolve a monitoring recipient for workshop %s/%s: %w", ws.Namespace, ws.Name, err)
	}
	if !ok {
		logger.Info("no deliverable role=monitoring Channel; skipping capability_request card", "detail", whyNot)
		return fmt.Errorf("no deliverable role=monitoring Channel for workshop %s/%s: %s", ws.Namespace, ws.Name, whyNot)
	}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "capability_request",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "Workshop",
			Namespace: ws.Namespace,
			Name:      ws.Name,
		},
		Condition: "WorkshopCapabilityRecommended",
		// Both the recommendation Summary and the artifact ref are
		// builder-supplied and MUST go through sanitizeCardText -- same
		// card-injection defense as the install card. No install action on
		// this card: a capability recommendation is not itself installable.
		Summary: "A workshop recommends a new capability: " + sanitizeCardText(ws.Spec.CapabilityRequest.Summary) +
			" (artifact " + sanitizeCardText(ws.Spec.CapabilityRequest.ArtifactRef) +
			", session " + ws.Spec.Session.Namespace + "/" + ws.Spec.Session.Name + ").",
		Hint:      "Review: " + consoleURL,
		Timestamp: w.now(),
	}
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("built an invalid capability_request MonitoringEvent (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}
	if err := channelevents.PublishMonitoring(w.NATSPublish, ev); err != nil {
		return fmt.Errorf("publish capability_request monitoring event (workshop %s/%s): %w", ws.Namespace, ws.Name, err)
	}

	if err := w.stampCapabilityDelivered(ctx, ws); err != nil {
		return fmt.Errorf("stamp workshop %s/%s status.capabilityRequest: %w", ws.Namespace, ws.Name, err)
	}
	logger.Info("capability_request card delivered")
	return nil
}

// consoleURL builds the admin console link the card's Hint carries, or an
// error when the external URL is not yet configured.
func (w *WorkshopHandoffWatcher) consoleURL() (string, error) {
	base := strings.TrimRight(w.ExternalBaseURL(), "/")
	if base == "" {
		return "", fmt.Errorf("external base URL is empty: cannot build a workshop_handoff card link")
	}
	return base + "/admin", nil
}

// stampInstallDelivered marks status.install as delivered by THIS watcher,
// writing only the fields it owns (Phase, when unset; RequestedAt, when
// unset; DeliveredAt, always). Fields admind owns (ApprovedBy, InstalledRef,
// Message, and any Phase beyond Requested) are read fresh and left untouched
// -- single-writer status via a Get-fresh + client.MergeFrom Status().Patch,
// so a concurrent admind write to those disjoint fields does not race this
// one.
func (w *WorkshopHandoffWatcher) stampInstallDelivered(ctx context.Context, ws *spiceboxv1alpha1.Workshop) error {
	var fresh spiceboxv1alpha1.Workshop
	if err := w.K8s.Get(ctx, client.ObjectKeyFromObject(ws), &fresh); err != nil {
		return fmt.Errorf("get workshop for status.install stamp: %w", err)
	}
	original := fresh.DeepCopy()
	now := metav1.NewTime(w.now())

	if fresh.Status.Install == nil {
		fresh.Status.Install = &spiceboxv1alpha1.WorkshopInstallStatus{}
	}
	if fresh.Status.Install.Phase == "" {
		fresh.Status.Install.Phase = spiceboxv1alpha1.WorkshopInstallPhaseRequested
	}
	if fresh.Status.Install.RequestedAt == nil {
		fresh.Status.Install.RequestedAt = &now
	}
	fresh.Status.Install.DeliveredAt = &now

	return w.K8s.Status().Patch(ctx, &fresh, client.MergeFrom(original))
}

// stampCapabilityDelivered marks status.capabilityRequest as delivered.
// Unlike status.install, this sub-object has no second writer -- the
// WorkshopHandoffWatcher is its sole owner -- so both fields are always set.
func (w *WorkshopHandoffWatcher) stampCapabilityDelivered(ctx context.Context, ws *spiceboxv1alpha1.Workshop) error {
	var fresh spiceboxv1alpha1.Workshop
	if err := w.K8s.Get(ctx, client.ObjectKeyFromObject(ws), &fresh); err != nil {
		return fmt.Errorf("get workshop for status.capabilityRequest stamp: %w", err)
	}
	original := fresh.DeepCopy()
	now := metav1.NewTime(w.now())

	fresh.Status.CapabilityRequest = &spiceboxv1alpha1.WorkshopCapabilityRequestStatus{
		DeliveredAt: &now,
		NoticeRef:   mintRequestID(),
	}

	return w.K8s.Status().Patch(ctx, &fresh, client.MergeFrom(original))
}

func (w *WorkshopHandoffWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}
