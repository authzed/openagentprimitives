package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
)

// StartupStatusWatcherInterval is the polling cadence. Matches the credential
// watcher's: both answer "what should this not-yet-running session's thread be
// showing right now", so a slower tick here would let the stale caption the
// credential watcher left behind linger visibly after credentials resolve.
const StartupStatusWatcherInterval = 5 * time.Second

// StartupStatusWatcher keeps a not-yet-running session's thread caption honest
// about what is blocking it — see agentstatus.StartupCaption for why deriving
// this beats pushing it.
//
// It publishes ONLY on change. The caption is a persistent surface, so
// re-announcing an unchanged blocker every tick would rewrite the thread's
// status on every poll: a flickering caption in the web chat and, on Slack, a
// message-edit storm against a rate-limited API.
type StartupStatusWatcher struct {
	// K8s lists the sessions to caption. Required by Run; ReconcileOne itself
	// takes the session, so tests can drive it without a client.
	K8s client.Client

	// NATSPublish publishes the KindNotification carrying the caption.
	// Required — a nil publisher would silently reinstate the stale-caption
	// defect this watcher exists to fix, so Run refuses to start without it.
	NATSPublish channelevents.PublishFunc

	// mu guards announced, which maps a session's namespace/name to the last
	// caption published for it. Run's loop is single-goroutine today, but the
	// map is the watcher's only mutable state and cheap to guard properly.
	mu        sync.Mutex
	announced map[string]string
}

// Run polls until ctx is canceled. Mirrors CredentialRequestWatcher.Run.
func (w *StartupStatusWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("startupstatus-watcher")
	if w.K8s == nil || w.NATSPublish == nil {
		logger.Info("watcher disabled: missing K8s or NATSPublish")
		return
	}
	ticker := time.NewTicker(StartupStatusWatcherInterval)
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

// reconcileAll captions every channel-attached session. A per-session failure
// is logged and skipped; the next tick retries.
func (w *StartupStatusWatcher) reconcileAll(ctx context.Context, logger logr.Logger) {
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := w.K8s.List(ctx, &sessions, client.HasLabels{spiceboxv1alpha1.LabelChannelName}); err != nil {
		logger.Error(err, "list agentsessions")
		return
	}
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if err := w.ReconcileOne(ctx, sess); err != nil {
			logger.Info("startup status caption failed",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			continue
		}
	}
}

// ReconcileOne publishes sess's blocking-reason caption if it has one and it
// differs from the last one published. Exported so tests can drive it directly
// without a polling loop.
func (w *StartupStatusWatcher) ReconcileOne(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	if sess == nil || sess.Spec.InputChannel == nil {
		return nil // no thread to caption
	}
	key := sess.Namespace + "/" + sess.Name

	// A terminal FAILURE never produced an agent closing message — the runner was
	// evicted, crashed, or never started — so the thread just stops with nothing
	// naming why. Post the failure notice — the failure mode where a
	// webhook-triggered session evicted mid-run left its channel thread silent.
	// sessionnotice.Derive is the single
	// source of what a status MEANS; we publish only its friendly Lead — the raw
	// condition detail is infrastructure context, not something a channel user
	// should read. Deduped by text like the caption, so it lands once.
	if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed {
		for _, n := range sessionnotice.Derive(sess) {
			if n.Kind != sessionnotice.KindFailed {
				continue
			}
			if !w.shouldAnnounce(key, n.Lead) {
				return nil
			}
			if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name,
				channelevents.KindNotification,
				channelevents.NotificationPayload{Text: n.Lead, Short: n.Lead},
			); err != nil {
				w.forget(key)
				return fmt.Errorf("publish terminal-failure notice (session %s): %w", key, err)
			}
			return nil
		}
		return nil
	}

	text, short, ok := agentstatus.StartupCaption(sess)
	if !ok {
		// Nothing to say. If the session has STARTED it never will again, so
		// drop its entry rather than carry it for the process's lifetime.
		if spiceboxv1alpha1.AgentSessionPhaseStarted(sess.Status.Phase) {
			w.forget(key)
		}
		return nil
	}
	if !w.shouldAnnounce(key, text) {
		return nil
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: text, Short: short},
	); err != nil {
		// Roll the record back so the next tick retries instead of treating a
		// failed publish as delivered — the user would otherwise keep staring
		// at the stale caption with nothing to correct it.
		w.forget(key)
		return fmt.Errorf("publish startup status caption (session %s): %w", key, err)
	}
	return nil
}

// shouldAnnounce records text as the caption for key and reports whether it
// differs from what was last recorded — the publish-only-on-change gate.
func (w *StartupStatusWatcher) shouldAnnounce(key, text string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.announced == nil {
		w.announced = map[string]string{}
	}
	if w.announced[key] == text {
		return false
	}
	w.announced[key] = text
	return true
}

func (w *StartupStatusWatcher) forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.announced, key)
}

// trackedSessions reports how many sessions the dedup map is holding. Exists
// for the test that pins the map to the lifetime of the sessions it describes.
func (w *StartupStatusWatcher) trackedSessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.announced)
}
