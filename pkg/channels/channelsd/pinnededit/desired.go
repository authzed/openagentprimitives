// Package pinnededit derives the desired pinned-opening-message content for a
// session from its CR. Pure; the channelsd poll loop applies the result.
package pinnededit

import (
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func isTerminalOutcome(b v1alpha1.OpeningBadge) bool {
	switch b {
	case v1alpha1.OpeningBadgeClean, v1alpha1.OpeningBadgeProblemsFound, v1alpha1.OpeningBadgeCouldNotFinish:
		return true
	}
	return false
}

// Desired returns the opening-message content to render, or ok=false when the
// session has no anchored opening to edit.
func Desired(sess *v1alpha1.AgentSession) (channelkinds.OpeningMessageContent, bool) {
	oc := sess.Spec.OutputChannel
	if oc == nil {
		return channelkinds.OpeningMessageContent{}, false
	}
	chID, ts := oc.External["channel_id"], oc.External["thread_ts"]
	openingText := sess.Annotations[v1alpha1.AnnotationSessionOpening]
	var instructions string
	if sess.Spec.OpeningSummary != "" {
		openingText = sess.Spec.OpeningSummary
		chID = sess.Annotations[v1alpha1.AnnotationSessionOpeningMessageChannel]
		ts = sess.Annotations[v1alpha1.AnnotationSessionOpeningMessageID]
		instructions = sess.Spec.Prompt.Inline
	}
	if chID == "" || ts == "" || openingText == "" {
		return channelkinds.OpeningMessageContent{}, false
	}

	var badge v1alpha1.OpeningBadge
	var body, link string
	if p := sess.Status.PinnedMessage; p != nil {
		badge, body, link = p.Badge, p.Body, p.Link
	}
	// A recorded outcome always wins. Otherwise derive from terminal phase.
	if !isTerminalOutcome(badge) {
		switch sess.Status.Phase {
		case v1alpha1.AgentSessionPhaseFailed:
			badge = v1alpha1.OpeningBadgeUnfinished
		case v1alpha1.AgentSessionPhaseSucceeded:
			badge = v1alpha1.OpeningBadgeDone
		case "":
			// not started yet; leave whatever the projection had
		default:
			if badge == "" {
				// An Idle session's runner has YIELDED — nothing is actively in
				// progress. A registered idle-status gate may force a terminal
				// badge (e.g. a triggered, humanless session that will never
				// self-resume must not read "in progress" forever); the gate acts
				// on the visible status ONLY — the phase stays Idle so a later
				// inbound re-joins the session. Running/Pending keep in_progress.
				if sess.Status.Phase == v1alpha1.AgentSessionPhaseIdle {
					if b, ok := terminalBadgeForIdle(sess); ok {
						badge = b
					}
				}
				if badge == "" {
					badge = v1alpha1.OpeningBadgeInProgress
				}
			}
		}
	}
	return channelkinds.OpeningMessageContent{
		Ref:          channelkinds.MessageRef{ChannelID: chID, TS: ts},
		OpeningText:  openingText,
		Badge:        badge,
		Body:         body,
		Link:         link,
		Instructions: instructions,
	}, true
}
