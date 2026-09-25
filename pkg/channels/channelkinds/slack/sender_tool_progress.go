// pkg/channels/channelkinds/slack/sender_tool_progress.go
//
// Renders KindToolProgress ticks onto the Slack assistant status line by
// composing a tool clause onto the agent's cached caption: 1 in-flight tool
// renders its own detail, N summarizes with a count naming the
// longest-running. Done removes the tool from the session's in-flight set;
// when the set drains, the plain agent caption is restored.
//
// Design: like sendTurnProgress, this reads the agent's caption read-only
// via recallStatus (never rememberStatus) so the tool clause augments rather
// than overwrites it. The in-flight set itself (toolProgress) is separate
// per-session state, guarded by toolProgMu.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// toolProgressReservedRunes reserves rune budget for the tool clause appended
// to the caption, mirroring progressReservedRunes for turn-progress. Worst
// case ~"3 tools (git clone 41s/30m · 100% · ~59m left)".
const toolProgressReservedRunes = 44

// toolProgEntry is one running tool in a session's in-flight set.
type toolProgEntry struct {
	name           string
	budgetSeconds  int
	elapsedSeconds int
	percent        *int32
	etaSeconds     *int32
}

// sendToolProgress folds one KindToolProgress tick into the session's in-flight
// set and re-renders the status caption. It augments the agent's cached caption
// (read-only via recallStatus, exactly like sendTurnProgress) with a tool
// clause: 1 tool → detail, N → a count naming the longest-running. Done removes
// the tool; when the set drains, the plain agent caption is restored.
func (s *slackSender) sendToolProgress(ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string, env channelevents.Envelope) error {
	var pl channelevents.ToolProgressPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("slack sender: unmarshal tool_progress payload: %w", err)
	}
	key := sess.Namespace + "/" + sess.Name

	s.toolProgMu.Lock()
	set := s.toolProgress[key]
	if set == nil {
		set = map[string]toolProgEntry{}
		s.toolProgress[key] = set
	}
	if pl.Done {
		delete(set, pl.CallID)
	} else {
		set[pl.CallID] = toolProgEntry{
			name: pl.Name, budgetSeconds: pl.BudgetSeconds, elapsedSeconds: pl.ElapsedSeconds,
			percent: pl.Percent, etaSeconds: pl.EtaSeconds,
		}
	}
	entries := make([]toolProgEntry, 0, len(set))
	for _, e := range set {
		entries = append(entries, e)
	}
	if len(set) == 0 {
		delete(s.toolProgress, key) // drop empty session buckets
	}
	s.toolProgMu.Unlock()

	// A tool_progress envelope IS forward progress — the runner emitted it. Tell
	// the silence watchdog before touching Slack, and regardless of what Slack
	// says: assistant.threads.setStatus rejects a non-assistant thread with
	// invalid_thread_ts, and gating the liveness signal behind that decorative
	// call let a healthy agent look silent and trip the "appears to have
	// stalled" notice on every user message.
	if s.deps.TouchSetStatus != nil {
		s.deps.TouchSetStatus(sess.Namespace, sess.Name)
	}

	// Resolve the agent's underlying caption read-only (never overwrite it).
	// The base caption is the agent's own update_status text when cached, else
	// the generic fallback — the same base-selection sendTurnProgress uses when
	// nothing is cached, so both progress surfaces stay consistent.
	cached, hasCached := s.recallStatus(sess)
	resolvedChannelID := channelID
	if hasCached && cached.channelID != "" {
		resolvedChannelID = cached.channelID
	}
	effectiveThreadTS := s.resolveThreadTS(ctx, sess, threadTS)

	caption := genericStatusCaption
	if hasCached && cached.status != "" {
		caption = cached.status
	}

	if len(entries) == 0 {
		// Set drained: ALWAYS re-render the plain base caption with no tool
		// clause so the finished tool's clause never lingers — even when the
		// agent never called update_status before the first tick (base is then
		// the generic caption, not a no-op that would leave the stale clause up).
		if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
			ChannelID: resolvedChannelID, ThreadTS: effectiveThreadTS,
			Status: caption, LoadingMessages: animatedLoadingMessages(caption),
		}); err != nil {
			log.FromContext(ctx).Info("slack: tool_progress restore setStatus failed (best-effort, continuing)",
				"error", err, "session", sess.Name)
		}
		return nil
	}

	clause := composeToolClause(entries)
	capFit := fitRunes(caption, statusMaxRunes-toolProgressReservedRunes)
	status := fitRunes(capFit+"  "+clause, statusMaxRunes)

	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID: resolvedChannelID, ThreadTS: effectiveThreadTS,
		Status: status, LoadingMessages: animatedLoadingMessages(capFit + "  " + clause),
	}); err != nil {
		log.FromContext(ctx).Info("slack: tool_progress setStatus failed (best-effort, continuing)",
			"error", err, "session", sess.Name, "channelID", resolvedChannelID)
		return nil
	}
	return nil
}

// composeToolClause renders the in-flight set deterministically (sorted by
// elapsed desc, then name): 1 tool → its detail; N → "N tools (<longest>, …)".
func composeToolClause(entries []toolProgEntry) string {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].elapsedSeconds != entries[j].elapsedSeconds {
			return entries[i].elapsedSeconds > entries[j].elapsedSeconds
		}
		return entries[i].name < entries[j].name
	})
	if len(entries) == 1 {
		return formatToolEntry(entries[0])
	}
	return fmt.Sprintf("%d tools (%s, …)", len(entries), formatToolEntry(entries[0]))
}

// formatToolEntry renders one tool: "git clone 14s/5m · 47% · ~30s left".
// Budget/percent/eta are appended only when present.
func formatToolEntry(e toolProgEntry) string {
	var b strings.Builder
	b.WriteString(e.name)
	b.WriteString(" ")
	b.WriteString(humanizeElapsed(e.elapsedSeconds))
	if e.budgetSeconds > 0 {
		b.WriteString("/")
		b.WriteString(humanizeElapsed(e.budgetSeconds))
	}
	if e.percent != nil {
		fmt.Fprintf(&b, " · %d%%", *e.percent)
	}
	if e.etaSeconds != nil {
		fmt.Fprintf(&b, " · ~%s left", humanizeElapsed(int(*e.etaSeconds)))
	}
	return b.String()
}
