package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ComputeRestartCut resolves a Slack (channelID, threadTS, messageTS) tuple to
// the inbox turn index in the session's memory plus the count of turns that
// would be discarded if restart-from-here proceeds.
//
// Returns (cut, discard, true, nil) when found, or (0, 0, false, nil) when no
// channel_msg_ref entry matches.
func ComputeRestartCut(ctx context.Context, mem memory.Memory, scope memory.Scope, channelID, threadTS, messageTS string) (int, int, bool, error) {
	ref := channelID + ":" + threadTS + ":" + messageTS
	cutTurn, ok, err := channel_msg_ref.Lookup(ctx, mem, scope, "slack", ref)
	if err != nil {
		return 0, 0, false, fmt.Errorf("ComputeRestartCut: lookup: %w", err)
	}
	if !ok {
		return 0, 0, false, nil
	}
	turns, err := turn.NewAppender(mem, scope).ReadAll(ctx)
	if err != nil {
		return 0, 0, false, fmt.Errorf("ComputeRestartCut: read turns: %w", err)
	}
	discard := 0
	for _, t := range turns {
		if t.Index > cutTurn {
			discard++
		}
	}
	return cutTurn, discard, true, nil
}

// handleRestartShortcut processes an InteractionTypeMessageAction callback with
// CallbackID "ap_restart_from_here". It resolves the session, checks eligibility,
// computes the discard count, and opens the restart confirmation modal.
func (l *slackListener) handleRestartShortcut(ctx context.Context, cb slackapi.InteractionCallback) error {
	channelID := cb.Channel.ID
	threadTS := cb.Message.ThreadTimestamp
	if threadTS == "" {
		threadTS = cb.Message.Timestamp
	}
	msgTS := cb.Message.Timestamp

	sess, err := l.findSessionByChannelKey(ctx, channelID, threadTS)
	if err != nil {
		return fmt.Errorf("handleRestartShortcut: find session: %w", err)
	}
	if sess == nil {
		return l.openRestartErrorModal(cb.TriggerID, "Restart isn't available for this message — no session bound to this thread.")
	}
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle &&
		sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseSucceeded {
		return l.openRestartErrorModal(cb.TriggerID, "Session is still running — wait for it to come to rest before restarting.")
	}
	// Verify the invoking Slack user has interact permission on the session.
	// Cross-user restart attempts (someone else triggers restart on another
	// user's session) are rejected here before any cut computation.
	canonical, err := l.resolveCanonicalForSlackUser(ctx, cb.User.ID)
	if err != nil || canonical == "" {
		return l.openRestartErrorModal(cb.TriggerID, "Couldn't resolve your identity for permission check.")
	}
	if l.deps.AuthzReader != nil {
		// resolveCanonicalForSlackUser returns "user:<id>"; strip prefix
		// since CheckInteract expects the bare canonical ID.
		rawCanonical := identity.CanonicalFromTrusted(strings.TrimPrefix(canonical, "user:"),
			"resolved server-side from the Slack-signed interaction's user id")
		allowed, cerr := l.deps.AuthzReader.CheckInteract(ctx, sess.Namespace, sess.Name, rawCanonical, true)
		if cerr != nil {
			return fmt.Errorf("handleRestartShortcut: CheckInteract: %w", cerr)
		}
		if !allowed {
			return l.openRestartErrorModal(cb.TriggerID, "You don't have permission to restart this session.")
		}
	}
	if l.restartMem == nil {
		return l.openRestartErrorModal(cb.TriggerID, "Restart is not currently configured on this server.")
	}
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	_, discard, ok, err := ComputeRestartCut(ctx, l.restartMem, scope, channelID, threadTS, msgTS)
	if err != nil {
		return fmt.Errorf("handleRestartShortcut: ComputeRestartCut: %w", err)
	}
	if !ok {
		return l.openRestartErrorModal(cb.TriggerID, "No matching turn for this message — it may predate the bot's session.")
	}
	view := BuildRestartModal(RestartModalArgs{
		OriginalText:     cb.Message.Text,
		DiscardCount:     discard,
		SessionNamespace: sess.Namespace,
		SessionName:      sess.Name,
		ChannelID:        channelID,
		ThreadTS:         threadTS,
		MessageTS:        msgTS,
		SiblingFork:      sess.Status.SupersededBy != "",
	})
	_, err = l.api.OpenView(cb.TriggerID, view)
	return err
}

// openRestartErrorModal shows a simple informational modal when the restart
// shortcut cannot proceed (session not found, wrong phase, no memory, etc.).
func (l *slackListener) openRestartErrorModal(triggerID, message string) error {
	view := slackapi.ModalViewRequest{
		Type:  slackapi.VTModal,
		Title: slackapi.NewTextBlockObject(slackapi.PlainTextType, "Restart from here", false, false),
		Close: slackapi.NewTextBlockObject(slackapi.PlainTextType, "OK", false, false),
		Blocks: slackapi.Blocks{BlockSet: []slackapi.Block{
			slackapi.NewSectionBlock(slackapi.NewTextBlockObject(slackapi.MarkdownType, message, false, false), nil, nil),
		}},
	}
	_, err := l.api.OpenView(triggerID, view)
	return err
}

// ParsedRestartSubmission is the structured contents of a
// view_submission for the restart modal.
type ParsedRestartSubmission struct {
	NewUserText      string
	SessionNamespace string
	SessionName      string
	ChannelID        string
	ThreadTS         string
	MessageTS        string
	SubmitterID      string
}

// ParseRestartViewSubmission extracts the user's edited text and the
// round-tripped private_metadata from a view_submission callback.
func ParseRestartViewSubmission(cb slackapi.InteractionCallback) (ParsedRestartSubmission, error) {
	var p ParsedRestartSubmission
	pm, err := DecodeRestartPrivateMetadata(cb.View.PrivateMetadata)
	if err != nil {
		return p, fmt.Errorf("ParseRestartViewSubmission: private_metadata: %w", err)
	}
	p.SessionNamespace = pm.SessionNamespace
	p.SessionName = pm.SessionName
	p.ChannelID = pm.ChannelID
	p.ThreadTS = pm.ThreadTS
	p.MessageTS = pm.MessageTS
	p.SubmitterID = cb.User.ID

	block, ok := cb.View.State.Values[RestartTextBlockID]
	if !ok {
		return p, fmt.Errorf("ParseRestartViewSubmission: missing %s in state.values", RestartTextBlockID)
	}
	elem, ok := block[RestartTextActionID]
	if !ok {
		return p, fmt.Errorf("ParseRestartViewSubmission: missing %s element in block", RestartTextActionID)
	}
	p.NewUserText = elem.Value
	return p, nil
}

// handleRestartSubmit processes a view_submission for the restart
// modal: re-resolves the session + cut, builds a RestartTrigger,
// and calls the registered submit function (RestartCapable).
func (l *slackListener) handleRestartSubmit(ctx context.Context, cb slackapi.InteractionCallback) error {
	parsed, err := ParseRestartViewSubmission(cb)
	if err != nil {
		return fmt.Errorf("handleRestartSubmit: parse: %w", err)
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := l.deps.K8sClient.Get(ctx, types.NamespacedName{Namespace: parsed.SessionNamespace, Name: parsed.SessionName}, &sess); err != nil {
		return fmt.Errorf("handleRestartSubmit: get session: %w", err)
	}
	if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle && sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseSucceeded {
		log.FromContext(ctx).Info("restart submitted but session no longer at rest",
			"session", parsed.SessionNamespace+"/"+parsed.SessionName, "phase", sess.Status.Phase)
		return nil
	}
	// Defense-in-depth: re-check interact permission on submit, in case
	// a user crafts a modal submit bypassing the shortcut gate.
	submitterCanonical, err := l.resolveCanonicalForSlackUser(ctx, parsed.SubmitterID)
	if err != nil || submitterCanonical == "" {
		return fmt.Errorf("handleRestartSubmit: resolve canonical: %w", err)
	}
	if l.deps.AuthzReader != nil {
		rawCanonical := identity.CanonicalFromTrusted(strings.TrimPrefix(submitterCanonical, "user:"),
			"resolved server-side from the Slack-signed interaction's user id")
		allowed, cerr := l.deps.AuthzReader.CheckInteract(ctx, sess.Namespace, sess.Name, rawCanonical, true)
		if cerr != nil {
			return fmt.Errorf("handleRestartSubmit: CheckInteract: %w", cerr)
		}
		if !allowed {
			log.FromContext(ctx).Info("restart submit rejected: user lacks interact",
				"session", sess.Namespace+"/"+sess.Name, "user", submitterCanonical)
			return nil // best-effort drop
		}
	}
	if l.restartMem == nil {
		log.FromContext(ctx).Info("restart submitted but restartMem not configured; dropping",
			"session", parsed.SessionNamespace+"/"+parsed.SessionName)
		return nil
	}
	scope := memory.Scope{Kind: "session", ID: parsed.SessionNamespace + "/" + parsed.SessionName}
	cut, _, ok, err := ComputeRestartCut(ctx, l.restartMem, scope, parsed.ChannelID, parsed.ThreadTS, parsed.MessageTS)
	if err != nil {
		return fmt.Errorf("handleRestartSubmit: recompute cut: %w", err)
	}
	if !ok {
		log.FromContext(ctx).Info("restart submitted but message ref vanished",
			"ref", parsed.ChannelID+":"+parsed.ThreadTS+":"+parsed.MessageTS)
		return nil
	}

	// Enrich the submitter with email + team so the downstream fork gate
	// canonicalizes them to the same user:<base64(email)> the owner-check
	// keys on. Without this the forker arrives email-less, restart_publish
	// mints a synthetic subject, and the SessionFork gate denies the OWNER
	// the right to restart their own session. Mirrors the approval-click
	// enrichment; the interact re-check above already resolved this identity.
	submitterExt := l.resolveIdentity(ctx, parsed.SubmitterID)
	trigger := channelkinds.RestartTrigger{
		SessionRef:   types.NamespacedName{Namespace: parsed.SessionNamespace, Name: parsed.SessionName},
		CutTurnIndex: int32(cut),
		NewUserText:  parsed.NewUserText,
		TriggeredBy: channelevents.ExternalIdentity{
			Kind:       "slack",
			ExternalID: identity.RawExternalID(parsed.SubmitterID),
			Email:      submitterExt.Email,
			TeamScope:  submitterExt.TeamScope,
		},
		KindRequestRef: cb.View.ID,
	}
	if l.restartSubmit == nil {
		return fmt.Errorf("handleRestartSubmit: no RestartSubmitFunc registered (channelsd start-up missed RestartCapable wiring)")
	}
	return l.restartSubmit(ctx, trigger)
}

// findSessionByChannelKey looks up an AgentSession by its LabelChannelKey label
// (hashed from "thread:<channel>:<thread_ts>"). Returns (nil, nil) when no
// session is found.
func (l *slackListener) findSessionByChannelKey(ctx context.Context, channelID, threadTS string) (*spiceboxv1alpha1.AgentSession, error) {
	channelKey := "thread:" + channelID + ":" + threadTS
	hash := channelkey.LabelValue(channelKey)

	var list spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &list,
		client.InNamespace(l.deps.Channel.Namespace),
		client.MatchingLabels{spiceboxv1alpha1.LabelChannelKey: hash},
	); err != nil {
		return nil, fmt.Errorf("findSessionByChannelKey: list: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return newestActiveSession(list.Items), nil
}

// newestActiveSession picks the session that owns a shared LabelChannelKey
// group today.
//
// An inherit fork now stays in the parent's thread, so a parent and its
// child routinely carry the same LabelChannelKey — a shared key is no
// longer a sign of anything unusual. Phase alone cannot tell which of the
// two is "the session for this thread": SupersedeParent deliberately leaves
// a transient-boot-Failed parent (BundleFailed, SidecarBootFailed,
// MemoryUnavailable, …) at Phase=Failed forever — only a non-Failed parent
// settles to Succeeded — so a dead superseded parent and a live child can
// both read as "non-Succeeded". List() order is also unspecified (in
// practice name-lexicographic, which puts a parent ahead of its
// "<parent>-ic<hash>" child), so a first-match loop is not just
// phase-blind, it's also order-dependent.
//
// The tie-breaker is SupersededBy: once set, that session explicitly
// continued elsewhere and its successor is the session that actually owns
// the thread now. Among whatever remains, the newest by CreationTimestamp
// that isn't Succeeded wins (the live session); if everything left is
// Succeeded, the newest Succeeded session wins. If every candidate turned
// out to be superseded (a mid-fork race), fall back to the newest session
// overall rather than returning nil — the restart shortcut must never
// strand the user on a resolvable thread.
func newestActiveSession(items []spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
	var newestLive, newestNonSuperseded, newestOverall *spiceboxv1alpha1.AgentSession
	for i := range items {
		item := &items[i]
		if newestOverall == nil || item.CreationTimestamp.Time.After(newestOverall.CreationTimestamp.Time) {
			newestOverall = item
		}
		if item.Status.SupersededBy != "" {
			continue
		}
		if newestNonSuperseded == nil || item.CreationTimestamp.Time.After(newestNonSuperseded.CreationTimestamp.Time) {
			newestNonSuperseded = item
		}
		if item.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseSucceeded {
			if newestLive == nil || item.CreationTimestamp.Time.After(newestLive.CreationTimestamp.Time) {
				newestLive = item
			}
		}
	}
	switch {
	case newestLive != nil:
		return newestLive
	case newestNonSuperseded != nil:
		return newestNonSuperseded
	default:
		return newestOverall
	}
}
