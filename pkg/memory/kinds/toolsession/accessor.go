package toolsession

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Event is the stored Content of a tool_session Entry — one parsed
// stream event. Mirrors channelevents.ToolSessionEventPayload so the
// persisted form and the NATS form carry the same fields.
type Event struct {
	// ToolCallRef correlates every event of one dispatched tool call.
	ToolCallRef string `json:"toolCallRef"`
	// EventType is one of text_delta, tool_use_start, tool_use_stop, result.
	EventType string `json:"eventType"`
	// Text is the streamed delta on a text_delta event; empty otherwise.
	Text string `json:"text,omitempty"`
	// ToolName is the streaming agent's own internal tool on tool_use events.
	ToolName string `json:"toolName,omitempty"`
	// ToolID is that internal tool call's id, pairing its start with its stop.
	ToolID string `json:"toolId,omitempty"`
	// OuterTool is the agent-facing tool that produced this stream (e.g.
	// "claude") — the dispatched tool, set by the runner. Distinct from
	// ToolName, which carries a streaming agent's own internal tool.
	OuterTool string `json:"outerTool,omitempty"`
	// Reason is the agent's `_reason` for the dispatching call — its stated
	// intent. Carried on every event so the Slack renderer's header is
	// stateless.
	Reason string `json:"reason,omitempty"`
	// Summary, OK, DurationMs and CostUSD are the result event's fields and
	// are set only on EventType "result"; zero on every streaming event.
	Summary    string  `json:"summary,omitempty"`
	OK         bool    `json:"ok,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
}

// Record persists one event into the tool_session Kind. The Entry ID is
// random (events are append-only — no deterministic key needed); order
// is recovered from CreatedAt.
func Record(ctx context.Context, mem memory.Memory, scope memory.Scope, ev Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("toolsession.Record: marshal: %w", err)
	}
	_, err = mem.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        memory.NewID(Kind{}),
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	if err != nil {
		return fmt.Errorf("toolsession.Record: put: %w", err)
	}
	return nil
}

// EntryToEvent decodes a tool_session Entry's Content. Exported so the
// oap CLI can render entries fetched over HTTP.
func EntryToEvent(e memory.Entry) (Event, error) {
	var ev Event
	if err := json.Unmarshal(e.Content, &ev); err != nil {
		return Event{}, fmt.Errorf("toolsession.EntryToEvent: %q: %w", e.ID, err)
	}
	return ev, nil
}

// ReadAll returns every tool_session event in scope, ascending CreatedAt. An
// entry that will not decode is skipped and logged.
//
// Skipping, not failing: tool_session is append-only, so an unparseable entry
// can never be removed (per-entry Delete is refused for an append-only Kind,
// DeleteScope skips it), and Memory.Put validates only Kind, write authority
// and ID prefix — never content. A hard error would wedge every replay of that
// session's tool stream forever. These events only RENDER a stream that already
// happened and gate nothing, so degraded rendering is the right trade — but
// never silently, hence the log naming the entry and its publisher.
func ReadAll(ctx context.Context, mem memory.Memory, scope memory.Scope) ([]Event, error) {
	res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, err
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		return res.Entries[i].CreatedAt.Before(res.Entries[j].CreatedAt)
	})
	out := make([]Event, 0, len(res.Entries))
	for _, e := range res.Entries {
		ev, cerr := EntryToEvent(e)
		if cerr != nil {
			undecodable.Skipped("toolsession.ReadAll", scope, e, cerr)
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}
