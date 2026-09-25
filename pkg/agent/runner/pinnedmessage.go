package runner

import (
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/pinnedmessage"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// pinnedMessageWebdBaseURL is a live getter for webd's externally reachable
// base URL, consulted only by deliveredLinkFor to compose a concluded
// session's optional report link. It is package-level rather than an
// argument threaded through recomputePinnedMessage's signature so the
// extracted-patchFn form the unit test drives (ctx, sess, hasOpening, patch)
// stays exactly that shape — mirroring the codebase's existing
// single-process-per-session DI knobs (see prompt.go's SetNowForTest,
// host.go's hostNow). internal/cmd/runner installs the real getter once at
// startup, the same cfg.webdBaseURL value TriggerStatusConfig.WebdBaseURL
// reads (pkg/agent/tool/meta/trigger_status.go). Left nil by every test and
// by a kubectl-driven runner with no webd address: deliveredLinkFor then
// returns "" rather than panicking, which is exactly what a session that
// cannot compose a link should project — never an error.
var pinnedMessageWebdBaseURL func() string

// SetWebdBaseURL installs the process-wide getter recomputePinnedMessage
// consults to compose a concluded session's report link. Called once by
// internal/cmd/runner at startup; safe to leave unset (the link stays "").
func SetWebdBaseURL(f func() string) { pinnedMessageWebdBaseURL = f }

// pinnedMessageMemo is the in-process "last patched" cache recomputePinnedMessage
// diffs against to skip a redundant status write. Keyed by the SessionContext
// pointer itself: production wiring holds exactly one live sess per runner
// process (l.SessionContext, set once at Run start and reused for that
// process's whole lifetime), so this holds exactly one entry per pod and is
// released when the process exits — it does not need to survive a resume
// (one harmless redundant patch on restart is not a bug) or be pruned.
var (
	pinnedMessageMemoMu sync.Mutex
	pinnedMessageMemo   = map[*tool.SessionContext]*spiceboxv1alpha1.PinnedMessageStatus{}
)

func lastPinnedFor(sess *tool.SessionContext) *spiceboxv1alpha1.PinnedMessageStatus {
	pinnedMessageMemoMu.Lock()
	defer pinnedMessageMemoMu.Unlock()
	return pinnedMessageMemo[sess]
}

func rememberPinned(sess *tool.SessionContext, p *spiceboxv1alpha1.PinnedMessageStatus) {
	pinnedMessageMemoMu.Lock()
	defer pinnedMessageMemoMu.Unlock()
	pinnedMessageMemo[sess] = p
}

// equalPinned reports whether a and b carry the same projection. Both nil
// counts as equal (no opening message, on both sides); one nil and one not
// never does. PinnedMessageStatus is comparable (three plain string-kind
// fields), so a dereferenced value compare is exact.
func equalPinned(a, b *spiceboxv1alpha1.PinnedMessageStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// recomputePinnedMessage reads this session's opening-summary body and
// trigger-conclusion state, derives the desired status.pinnedMessage
// projection via the pure pinnedmessage.Derive, and invokes patch only when
// the derived value differs from the last one this function wrote for sess.
//
// hasOpening is passed in rather than resolved here because it depends on the
// session's input channel kind (chregistry.TriggerDescriberFor), which is a
// caller concern (loop.go resolves it once from l.ChannelKind); this function
// stays a pure reader of session state plus that one upstream fact.
//
// patch is a func rather than a *StatusPatcher so the unit test can drive
// this entirely off an in-memory SessionContext, with no k8s client involved
// — the same "extract the write as a func" shape status.go's other per-turn
// writers use.
func recomputePinnedMessage(ctx context.Context, sess *tool.SessionContext, hasOpening bool,
	patch func(context.Context, *spiceboxv1alpha1.PinnedMessageStatus) error) error {

	var body string
	if os, ok := openingsummary.TryFrom(sess); ok {
		body = os.Body()
	}
	var concluded bool
	var outcome, link string
	if ts, ok := triggerstatus.TryFrom(sess); ok {
		if o, done := ts.Concluded(); done {
			concluded, outcome = true, string(o)
		}
	}
	if concluded {
		link = deliveredLinkFor(ctx, sess)
	}
	desired := pinnedmessage.Derive(hasOpening, concluded, outcome, body, link)
	if equalPinned(lastPinnedFor(sess), desired) {
		return nil
	}
	// Remember only AFTER a successful write: a transient patch failure (API
	// server outage) must leave the memo at its old value, so the next turn's
	// recompute sees a real difference again and retries — remembering first
	// would make the failed write look already-applied and never be retried.
	if err := patch(ctx, desired); err != nil {
		return err
	}
	rememberPinned(sess, desired)
	return nil
}

// recomputeAndPatchPinnedMessage is loop.go's thin per-turn wrapper wiring
// recomputePinnedMessage to this Loop's live status writer. l.Status is nil
// for a kubectl-driven/local session or a bare test Loop — no AgentSession
// status exists to patch there, so this is a clean no-op rather than a panic.
func (l *Loop) recomputeAndPatchPinnedMessage(ctx context.Context, sess *tool.SessionContext, hasOpening bool) error {
	if l.Status == nil {
		return nil
	}
	return recomputePinnedMessage(ctx, sess, hasOpening, l.Status.setPinnedMessage)
}

// deliveredLinkFor composes the durable, subject-independent report-view link
// for the artifact this session most recently delivered, or "" when there is
// nothing to link to. Mirrors TriggerStatusConfig.deliveredResultLink
// (pkg/agent/tool/meta/trigger_status.go:343) — same inputs (deliveries'
// LastArtifactID, webd's base URL, channelkinds.ComposeArtifactViewURL), same
// "every failure degrades to no link, never an error" contract — factored
// separately rather than shared because that method hangs off
// TriggerStatusConfig and this call site has neither one.
//
// Every "nothing to link to" path logs at Info before returning "" — like the
// sibling — so a pinned message that quietly stopped carrying a link is
// traceable to the reason here rather than being silently swallowed.
func deliveredLinkFor(ctx context.Context, sess *tool.SessionContext) string {
	logger := log.FromContext(ctx)
	sessionRef := sess.Namespace + "/" + sess.Name
	if pinnedMessageWebdBaseURL == nil {
		logger.Info("pinnedMessage: no webd base URL is wired into this runner, so the pinned message will carry no report link",
			"session", sessionRef)
		return ""
	}
	delivered, ok := deliveries.TryFrom(sess)
	if !ok {
		logger.Info("pinnedMessage: this session carries no delivery state, so the pinned message will carry no report link",
			"session", sessionRef)
		return ""
	}
	artifactID := delivered.LastArtifactID()
	if artifactID == "" {
		// Ordinary for a session that answered in prose alone; also what a
		// render carrying no artifact-id label produces.
		logger.Info("pinnedMessage: this session delivered no linkable artifact, so the pinned message will carry no report link",
			"session", sessionRef)
		return ""
	}
	url, err := channelkinds.ComposeArtifactViewURL(pinnedMessageWebdBaseURL(), sessionRef, artifactID)
	if err != nil {
		logger.Info("pinnedMessage: could not compose the artifact link; the pinned message will carry no report link",
			"session", sessionRef, "artifactID", artifactID, "err", err.Error())
		return ""
	}
	if url == "" {
		logger.Info("pinnedMessage: webd has no external URL yet, so the pinned message will carry no report link",
			"session", sessionRef, "artifactID", artifactID)
	}
	return url
}
