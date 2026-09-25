// pkg/channels/channelkinds/slack/blocks_degrade.go
//
// The shared "Slack refused these blocks — fall back to ones it has always
// accepted" seam.
//
// Two renderers depend on Block Kit primitives new enough that a workspace or
// SDK may refuse them — plan/task_card in sender_plan.go, and the notice
// container in interaction_notice.go. Both need the same question answered:
// "did Slack reject the SHAPE, or did something else go wrong?" One allowlist
// answers it for both, so adding a rejection code is a one-line change that
// cannot leave one renderer behind.
package slack

import (
	"errors"

	slackapi "github.com/slack-go/slack"
)

// isUnsupportedBlocksErr reports whether err is Slack refusing a block type or
// block payload — as opposed to a network fault, an auth failure, or a
// rate limit, none of which a different rendering would fix.
//
// Retrying with a simpler rendering is only correct for THIS class. Widening
// it would turn a transient outage into a permanent silent downgrade, where
// every user quietly gets the degraded rendering forever and nothing says so.
func isUnsupportedBlocksErr(err error) bool {
	var sre slackapi.SlackErrorResponse
	if !errors.As(err, &sre) {
		return false
	}
	switch sre.Err {
	case "invalid_blocks", "invalid_blocks_format", "block_unknown_type":
		return true
	default:
		return false
	}
}
