// pkg/channels/channelsd/pipeline/credential_linked.go
//
// CredentialLinkedWatcher is the detective control pairing with the single-use
// signed deep-links: when a credential is added or replaced on a UserIdentity it
// publishes a KindInteractionApplied envelope (category=credential_link,
// outcome=resolved) to the legitimate user's most recent channel binding, so
// they notice in real time if a forwarded link was used by someone else.
//
// It polls UserIdentity every CredentialLinkedWatcherInterval and diffs each
// user's spec.credentials against an in-memory per-user snapshot:
//
//   - name in current but not observed: ADDED → emit.
//   - name in both, fingerprint changed: REPLACED → emit.
//   - name in observed but not current: REMOVED → DO NOT emit; revocation has
//     its own UX in the portal.
//
// On startup the snapshot is primed from the current UserIdentity list, so a
// restart does not spam confirmations for credentials linked before it.
//
// Channel lookup mirrors the credential_request watcher: find the most recent
// AgentSession whose AnnotationStartedByCanonicalID equals user.Spec.Subject
// (both carry the "user:" prefix), take its InputChannel, and publish on that
// session's .out subject. When no recent session matches — a portal-only linker
// — the watcher logs and skips.
package pipeline

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// CredentialLinkedWatcherInterval is the polling cadence. 5s matches
// the other channelsd watchers; the user-visible latency target is
// "noticeable within seconds of link completion."
const CredentialLinkedWatcherInterval = 5 * time.Second

// observedCredentials is one UserIdentity's last-seen credential set,
// keyed by credential name → a fingerprint we diff against. The
// fingerprint covers (type, secretRef.Name) so replacing a credential
// (same name, new Secret) reads as REPLACED even if the underlying
// Secret contents change without a Spec edit.
type observedCredentials map[string]string

// CredentialLinkedWatcher polls UserIdentity, diffs each user's
// spec.credentials, and publishes KindInteractionApplied
// (category=credential_link, outcome=resolved) envelopes on the user's
// most recent AgentSession's .out subject.
type CredentialLinkedWatcher struct {
	// K8s is the client used to list UserIdentity + AgentSessions and
	// resolve the matching channel binding.
	K8s client.Client

	// Senders is unused by this watcher's own publish path since the flip
	// to interaction_applied (delivery now goes through NATSPublish + the
	// outbound relay's "interaction" sub-channel resolution, not a direct
	// SubChannelSenderFor call here). Retained on the struct for the same
	// reason as CredentialRequestWatcher.Senders — see its doc comment.
	Senders SubChannelSenderResolver

	// NATSPublish publishes the KindInteractionApplied envelope on the
	// session's .out subject — the primary (and only) delivery path for
	// this watcher's out-of-band "X just connected" confirmation.
	// Required: emit returns an error when this is nil rather than
	// silently doing nothing.
	NATSPublish channelevents.PublishFunc

	// PollInterval overrides CredentialLinkedWatcherInterval. Tests set
	// it to a small value (or drive ReconcileOnce directly).
	PollInterval time.Duration

	mu       sync.Mutex
	observed map[string]observedCredentials // UserIdentity.Name → state
}

// Run primes the observed state from the current UserIdentity list,
// then polls until ctx is canceled. Mirrors
// CredentialRequestWatcher.Run.
func (w *CredentialLinkedWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("credentiallinked-watcher")

	// Prime observed state so we don't emit on startup for credentials
	// that were already linked before channelsd came up. A failure here
	// degrades safely: w.observed is left nil → the first scan treats
	// every existing credential as ADDED, which would spam users.
	// Retry until prime succeeds, then start the ticker.
	for {
		if err := w.primeObserved(ctx); err != nil {
			logger.Info("prime observed credentials failed; will retry", "err", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.interval()):
			}
			continue
		}
		break
	}

	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan(ctx, logger)
		}
	}
}

// interval returns the configured PollInterval or the default.
func (w *CredentialLinkedWatcher) interval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return CredentialLinkedWatcherInterval
}

// primeObserved captures the current credential set of every
// UserIdentity into w.observed. Called once at startup. Concurrent-safe
// (takes w.mu).
func (w *CredentialLinkedWatcher) primeObserved(ctx context.Context) error {
	var users spiceboxv1alpha1.UserIdentityList
	if err := w.K8s.List(ctx, &users); err != nil {
		return fmt.Errorf("list UserIdentity: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observed = make(map[string]observedCredentials, len(users.Items))
	for i := range users.Items {
		u := &users.Items[i]
		w.observed[u.Name] = fingerprintCredentials(ctx, u.Spec.Credentials)
	}
	return nil
}

// scan lists every UserIdentity, diffs against the observed snapshot,
// and emits a KindCredentialLinked envelope for each ADDED or REPLACED
// credential. Errors are logged; per-user failures don't abort the
// loop.
func (w *CredentialLinkedWatcher) scan(ctx context.Context, logger logr.Logger) {
	var users spiceboxv1alpha1.UserIdentityList
	if err := w.K8s.List(ctx, &users); err != nil {
		logger.Info("list UserIdentity failed; will retry next tick", "err", err.Error())
		return
	}
	for i := range users.Items {
		u := &users.Items[i]
		if err := w.reconcileUser(ctx, u); err != nil {
			logger.Info("credential_linked reconcile failed",
				"user", u.Name, "subject", u.Spec.Subject,
				"err", err.Error())
			// Per-user failure does not abort the loop; next tick retries.
			continue
		}
	}
}

// ReconcileOne is the test entry point: takes a single UserIdentity,
// diffs it against the observed snapshot, emits envelopes, and updates
// the snapshot. Exported so tests don't have to drive the polling
// loop. Safe for concurrent calls across distinct users (the snapshot
// is guarded by w.mu).
func (w *CredentialLinkedWatcher) ReconcileOne(ctx context.Context, user *spiceboxv1alpha1.UserIdentity) error {
	return w.reconcileUser(ctx, user)
}

// reconcileUser diffs one UserIdentity and emits per-change envelopes.
func (w *CredentialLinkedWatcher) reconcileUser(ctx context.Context, user *spiceboxv1alpha1.UserIdentity) error {
	logger := log.FromContext(ctx).WithValues("user", user.Name, "subject", user.Spec.Subject)

	current := fingerprintCredentials(ctx, user.Spec.Credentials)

	w.mu.Lock()
	if w.observed == nil {
		w.observed = make(map[string]observedCredentials)
	}
	prior, hadPrior := w.observed[user.Name]
	w.mu.Unlock()

	// First time we've seen this user (UserIdentity created after
	// channelsd started). Treat its current credentials as the new
	// baseline — i.e. record them but DO NOT emit. Otherwise the brand-
	// new UserIdentity that identityd just created (already populated
	// with one credential by the time channelsd's watcher sees it)
	// would emit on every restart. Subsequent additions to the same
	// UserIdentity emit normally because the entry is then in observed.
	if !hadPrior {
		w.mu.Lock()
		w.observed[user.Name] = current
		w.mu.Unlock()
		if len(current) > 0 {
			logger.V(1).Info("observed new UserIdentity; priming without emit",
				"credentialCount", len(current))
		}
		return nil
	}

	// Find ADDED / REPLACED credentials. Deterministic iteration via a
	// sorted name list — keeps tests reproducible when a single scan
	// emits multiple envelopes for one user (rare but legal).
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	sort.Strings(names)

	var firstErr error
	for _, name := range names {
		fp := current[name]
		priorFp, hadPriorCred := prior[name]
		switch {
		case !hadPriorCred:
			// ADDED.
			if err := w.emit(ctx, user, name); err != nil {
				logger.Info("emit credential_linked failed", "credential", name, "err", err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		case priorFp != fp:
			// REPLACED.
			if err := w.emit(ctx, user, name); err != nil {
				logger.Info("emit credential_linked failed (replaced)", "credential", name, "err", err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		default:
			// Unchanged — nothing to do.
		}
	}
	// REMOVED credentials are intentionally NOT emitted — revocation has its own
	// UX surface in the portal. Just update the snapshot.

	// Snapshot updated AFTER this user's emit decisions. It advances even when
	// emit failed, so a transport error cannot spam duplicate envelopes on every
	// subsequent tick — the trade favors at-most-one prompt per change over
	// guaranteed delivery, same as credential_request's dedup.
	w.mu.Lock()
	w.observed[user.Name] = current
	w.mu.Unlock()

	return firstErr
}

// emit builds the resolved-outcome envelope and publishes it on the user's most
// recent AgentSession's .out subject, where the outbound relay resolves that
// session's kind "interaction" sub-channel sender. See
// CredentialRequestWatcher.doPublish for why this is a publish rather than a
// direct SubChannelSenderFor call.
func (w *CredentialLinkedWatcher) emit(ctx context.Context, user *spiceboxv1alpha1.UserIdentity, credentialName string) error {
	logger := log.FromContext(ctx).WithValues("user", user.Name, "credential", credentialName)

	if w.NATSPublish == nil {
		return fmt.Errorf("credential_linked: NATSPublish not configured (wiring bug); cannot deliver interaction_applied for user %s credential %s", user.Name, credentialName)
	}

	sess, err := w.mostRecentSessionFor(ctx, user.Spec.Subject)
	if err != nil {
		return fmt.Errorf("lookup recent session for %q: %w", user.Spec.Subject, err)
	}
	if sess == nil {
		// No AgentSession has this user as its starter — expected for someone
		// who only ever linked proactively via the portal. A linker who came
		// through a reactive deep-link always has a recent session, so log this
		// to keep the absence grep-able.
		logger.Info("no recent AgentSession for user; skipping OOB confirmation")
		return nil
	}
	if sess.Spec.InputChannel == nil {
		// Defensive: a session without an input channel can't surface
		// the prompt. Skip + log.
		logger.Info("most recent session has no InputChannel; skipping OOB confirmation",
			"session", sess.Namespace+"/"+sess.Name)
		return nil
	}

	applied := channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.CredentialLink,
		// KNOWN GAP: RequestRef is the credentialName, not the mintRequestID()
		// the webchat interaction_request card was keyed by. This out-of-band
		// "linked" confirmation therefore never correlates to the "Connect your
		// accounts" card by requestRef, so that card does NOT auto-resolve.
		// Not a hang — the session resumes once the credential is linked, since
		// this event is a confirmation and not a gate. Fixing it needs
		// multi-credential correlation mapping credentialName back to the live
		// requestRef; tracked as a follow-up.
		RequestRef: credentialName,
		Outcome:    channelevents.OutcomeResolved,
		// OutcomeText carries the raw CredentialName: there is no
		// per-credential label lookup at this layer, since the controller's
		// labels are session-scoped. Rendered as "<OutcomeText> connected."
		OutcomeText: credentialName,
	}
	if err := applied.Validate(); err != nil {
		return fmt.Errorf("credential_linked: built an invalid interaction_applied payload (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name, channelevents.KindInteractionApplied, applied); err != nil {
		return fmt.Errorf("publish credential interaction_applied (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	logger.Info("credential_linked published",
		"session", sess.Namespace+"/"+sess.Name,
		"recipient", user.Spec.Subject)
	return nil
}

// mostRecentSessionFor finds the most recently-created AgentSession
// whose AnnotationStartedByCanonicalID matches subject. Returns nil
// (no error) if no session is found. Used to pick the channel binding
// for the user's OOB confirmation.
//
// Implementation note: the AgentSession list has no built-in
// "started-by" label (only LabelChannelName), so we filter
// client-side. UserIdentity volume is low (one per human user) so a
// full list per tick is acceptable today. If this becomes hot, a
// dedicated label or annotation index lookup is a follow-up.
func (w *CredentialLinkedWatcher) mostRecentSessionFor(ctx context.Context, subject string) (*spiceboxv1alpha1.AgentSession, error) {
	if subject == "" {
		return nil, nil
	}
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := w.K8s.List(ctx, &sessions); err != nil {
		return nil, fmt.Errorf("list AgentSessions: %w", err)
	}
	var best *spiceboxv1alpha1.AgentSession
	for i := range sessions.Items {
		s := &sessions.Items[i]
		if spiceboxv1alpha1.StartedBySubject(s).String() != subject {
			continue
		}
		if s.Spec.InputChannel == nil {
			continue
		}
		if best == nil || s.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = s
		}
	}
	return best, nil
}

// fingerprintCredentials reduces a credential list to name → fingerprint
// pairs so subsequent diffs can detect ADDED, REPLACED, and REMOVED
// entries. The fingerprint covers the credential type discriminator +
// the Secret reference name so a static→oauth migration OR a same-name
// new-Secret update both register as REPLACED.
func fingerprintCredentials(ctx context.Context, creds []spiceboxv1alpha1.AgentCredential) observedCredentials {
	logger := log.FromContext(ctx)
	out := make(observedCredentials, len(creds))
	for _, c := range creds {
		if c.Name == "" {
			continue
		}
		out[c.Name] = credentialFingerprint(c, logger)
	}
	return out
}

// credentialFingerprint computes "<type>:<secretRefName>" for one
// credential via the credkind registry. Returns just "<type>:" when the
// type has no backing Secret to name (a minted type) or is unrecognized by
// this build — that's either a legitimate minted credential or a malformed
// one, but either way we don't want a nil dereference here; the resulting
// fingerprint will compare unequal to any well-formed entry, so the next
// valid version of the credential triggers a REPLACED emit.
//
// An unrecognized type is logged at INFO rather than silently folded into
// the "<type>:" fallback with no trace.
func credentialFingerprint(c spiceboxv1alpha1.AgentCredential, logger logr.Logger) string {
	secretRef := ""
	if k, err := credkindregistry.Get(c.Type); err != nil {
		logger.Info("credential_linked: unrecognized credential type; fingerprint carries no secretRef",
			"credential", c.Name, "type", c.Type, "err", err.Error())
	} else if ref := k.SecretRef(c); ref != nil {
		secretRef = ref.Name
	}
	return c.Type + ":" + secretRef
}
