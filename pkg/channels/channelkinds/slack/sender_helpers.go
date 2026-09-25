// pkg/channels/channelkinds/slack/sender_helpers.go
//
// Kept Slack render/delivery helpers relocated out of the now-removed
// tool_approval / info_leakage sender files because surviving files depend on
// them. Bodies unchanged.
package slack

import (
	"errors"
	"fmt"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

const (
	// awaitingTickInterval is how often the awaiting-indicator ticker
	// chat.update's the thread message with "Nm elapsed". Set
	// conservatively: every 30s × 1 in-flight approval is 2 updates/min,
	// well under Slack's Tier-2 chat.update budget. Supplied to the authz
	// driver via toolApprovalPending.Interval().
	awaitingTickInterval = 30 * time.Second
	// awaitingMaxDuration caps the ticker so a stuck/abandoned request
	// doesn't update the thread message forever. The runner's per-tool
	// approval timeout is configurable but typically 5 min; 10 min is
	// a safe upper bound.
	awaitingMaxDuration = 10 * time.Minute
	// namedApproverThreshold is the cutoff above which the public thread
	// message references the raw approver subject (or display label) instead of
	// @-mentioning each approver individually.
	namedApproverThreshold = 3
)

// resolveChannelAndThread reads the AgentSession's channel binding for
// the Slack-side routing tuple (channel_id, thread_ts). thread_ts is
// resolved via effectiveOutboundThreadTS (the per-turn LastInboundTS
// annotation, falling back to External["thread_ts"]); it may still be
// empty for a fresh DM/cron session with neither set.
func resolveChannelAndThread(sess channelkinds.SessionInfo) (channelID, threadTS string) {
	if sess.Channel == nil {
		return "", ""
	}
	return sess.Channel.External["channel_id"], effectiveOutboundThreadTS(sess)
}

// isUserNotInChannelErr reports whether err is the Slack API rejection
// we use to trigger the DM fallback. slack-go wraps this as a typed
// SlackErrorResponse; the raw API error string is "user_not_in_channel".
func isUserNotInChannelErr(err error) bool {
	var sre slackapi.SlackErrorResponse
	if errors.As(err, &sre) {
		return sre.Err == "user_not_in_channel"
	}
	// Belt-and-suspenders: some slack-go code paths surface the bare
	// string via fmt.Errorf("%s", ...). Match on text as well so the
	// fallback fires whether the error is typed or stringified.
	return err != nil && err.Error() == "user_not_in_channel"
}

// truncateRunes hard-clips s to maxRunes runes (NOT bytes — emoji and
// non-ASCII are multi-byte, and every Slack limit is a rune count). Appends an
// ellipsis when it cut, so the reader knows there was more.
//
// It lives here beside the other summarizers rather than in app_home_view.go,
// where it started: it is now what caps the Show-Details modal's short slots
// too, and a helper two unrelated renderers depend on belongs with the shared
// ones rather than inside one of them.
//
// summarizeForSlackSection is the right tool for PROSE (it takes the first line
// as a summary); this one is for a value that has no first line to take — a
// resource id, a permission, a hash — where dropping everything after a newline
// would silently hide part of what the reader is gating on.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}

// summarizeForSlackSection trims a long description down to its first
// line and caps the result at maxRunes. The full text is preserved in
// the agent's own context; this is just what fits in a Slack section
// block (whose mrkdwn limit is 3000 chars — an opaque "invalid_blocks"
// error fires above that). Returns "" if the input is "".
//
// Its one caller (the Show-Details modal) hands it text the sweep has already
// been over, so the cap is capInertRunes rather than a plain rune clip: the
// sweep wraps each URL in a PAIR of backticks, and a cut that lands between
// them drops the closing one and re-livens the link. On text that carries no
// delimiter at all capInertRunes is a plain clip.
//
// The FIRST-LINE STRIP is a character-removing transform of the same shape, and
// the reasoning that it could do no damage held for only one of the two pairs
// the sweep leaves behind. It is true for a SPAN — defuseBareLinks matches a
// whitespace-free run, so a span never straddles a newline. It is false for a
// FENCE, which is exactly what straddles newlines: an upstream tool description
// of "Creates an issue. ```URL\nx```" is fence-SKIPPED by the sweep and then cut
// at that newline, leaving an unterminated fence around a URL nothing ever
// defused. capInertRunes closes the orphaned fence, which is why this call must
// stay after the strip and must not be short-circuited when the text is already
// inside the budget.
func summarizeForSlackSection(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	// First-line summary: strip after the first newline. Many MCP tool
	// descriptions embed long <capabilities>/<returns> XML-ish blocks
	// after the lede, all of which is noise for an approver.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return s
	}
	return capInertRunes(s, maxRunes)
}

// summarizeArgsForContext trims a tool-call args JSON blob down to maxRunes
// for rendering inside a Slack section/code block, appending a truncation
// marker when it overflows. An empty or "{}" args blob renders nothing. Used
// by the generic Show-Details modal (interaction_details.go).
func summarizeArgsForContext(argsJSON string, maxRunes int) string {
	s := strings.TrimSpace(argsJSON)
	if s == "" || s == "{}" {
		return ""
	}
	if maxRunes <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	const suffix = "\n…(args truncated)"
	return string(rs[:maxRunes-len([]rune(suffix))]) + suffix
}

// formatElapsedSuffix renders the "_Nm elapsed._" italic suffix the
// awaiting-ticker appends to the thread message every 30s. Sub-minute
// elapsed renders as "_just now._" so a tick that fires immediately
// after the initial post doesn't show a misleading "0m elapsed".
func formatElapsedSuffix(elapsed time.Duration) string {
	if elapsed < time.Minute {
		return "_just now._"
	}
	return fmt.Sprintf("_%dm elapsed._", int(elapsed/time.Minute))
}
