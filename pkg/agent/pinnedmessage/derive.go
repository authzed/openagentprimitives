// Package pinnedmessage derives a triggered session's opening-message status
// projection from its durable session state. Pure and framework-side; channelsd
// separately derives the terminal (unfinished/done) badges from Status.Phase.
package pinnedmessage

import v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

// Derive returns the projection the runner writes to status.pinnedMessage, or
// nil when this session has no opening message. Outcome strings equal the
// OpeningBadge outcome values, so a concluded verdict folds directly.
func Derive(hasOpening, concluded bool, outcome, body, link string) *v1alpha1.PinnedMessageStatus {
	if !hasOpening {
		return nil
	}
	if concluded {
		return &v1alpha1.PinnedMessageStatus{
			Badge: v1alpha1.OpeningBadge(outcome),
			Body:  body,
			Link:  link,
		}
	}
	return &v1alpha1.PinnedMessageStatus{Badge: v1alpha1.OpeningBadgeInProgress, Body: body}
}
