// pkg/channels/channelkinds/slack/sender_turn_progress.go
//
// Renders KindTurnProgress envelopes onto the Slack assistant status line:
// "<caption>  <in> in · <out> out · <elapsed>". The spinner lives solely on
// the loading_messages surface, animated by Slack's own client-side rotation
// (see animatedLoadingMessages) — the status line itself stays frame-free.
//
// Design: read-only against the status cache (recallStatus without
// rememberStatus) so successive ticks wrap the agent's plain caption rather
// than compounding the suffix. Best-effort: setStatus errors are logged and
// not returned. The silence watchdog is NOT touched here — token deltas tick
// it via the stream-delta path.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// progressReservedRunes is the rune budget reserved for everything the
// renderer wraps around the caption: the "  " (2) separator plus a
// worst-case "<in> in · <out> out · <elapsed>" suffix (~32 runes). The
// caption is truncated to statusMaxRunes-progressReservedRunes so the
// suffix is never clipped, even at the widest counts.
const progressReservedRunes = 34

// sendTurnProgress renders a KindTurnProgress envelope onto the Slack
// assistant status line: "<caption>  <in> in · <out> out · <elapsed>".
// It reuses the caption the agent stashed via its last update_status (or a
// generic fallback) WITHOUT overwriting the cache, so successive ticks keep
// wrapping the agent's plain caption rather than compounding the suffix.
// Render-only: it does not touch the silence watchdog (token deltas already
// tick it via the stream-delta path) and is best-effort (errors logged).
func (s *slackSender) sendTurnProgress(ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string, env channelevents.Envelope) error {
	var pl channelevents.TurnProgressPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("slack sender: unmarshal turn_progress payload: %w", err)
	}

	caption := genericStatusCaption
	resolvedChannelID := channelID
	if c, ok := s.recallStatus(sess); ok && c.status != "" {
		caption = c.status
		if c.channelID != "" {
			resolvedChannelID = c.channelID
		}
	}

	suffix := fmt.Sprintf("%s in · %s out · %s",
		humanizeTokens(pl.InputTokens), humanizeTokens(pl.OutputTokens), humanizeElapsed(pl.ElapsedSeconds))

	caption = fitRunes(caption, statusMaxRunes-progressReservedRunes)
	status := fitRunes(caption+"  "+suffix, statusMaxRunes)

	effectiveThreadTS := s.resolveThreadTS(ctx, sess, threadTS)
	// The spinner appears only on the loading surface: animatedLoadingMessages
	// prepends a frame per entry and Slack's client-side rotation animates it
	// between ticks, so the status line carries no frame of its own.
	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       resolvedChannelID,
		ThreadTS:        effectiveThreadTS,
		Status:          status,
		LoadingMessages: animatedLoadingMessages(caption + "  " + suffix),
	}); err != nil {
		log.FromContext(ctx).Info("slack: turn_progress setStatus failed (best-effort, continuing)",
			"error", err, "session", sess.Name, "channelID", resolvedChannelID)
	}
	return nil
}

// humanizeTokens renders a token count compactly via SI prefixes, normalized
// to "999" / "1.2K" / "47.2K" / "1.5M". go-humanize emits "1.2 k" (space +
// lowercase prefix); we drop the space and uppercase so the unit reads the
// way users expect for token counts. ToUpper is safe on the digits/decimal.
func humanizeTokens(n int64) string {
	return strings.ToUpper(strings.ReplaceAll(humanize.SIWithDigits(float64(n), 1, ""), " ", ""))
}

// humanizeElapsed renders a non-negative second count using Go's duration
// formatting: "34s", "1m20s", "2m5s".
func humanizeElapsed(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return (time.Duration(sec) * time.Second).String()
}

// fitRunes truncates s to at most max runes, appending "…" when it had to
// cut. Rune-aware so multi-byte glyphs aren't split.
func fitRunes(s string, max int) string {
	if max < 0 {
		max = 0
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}
