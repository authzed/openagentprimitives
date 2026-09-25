package claude

import (
	"bytes"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
)

// maxLineBytes bounds the partial-line buffer. A single malformed
// multi-MB line would otherwise eat heap until OOM; on overflow we
// clear the buffer and emit a synthetic text_delta so the truncation
// is user-visible.
const maxLineBytes = 1 << 20

type parser struct {
	buf     bytes.Buffer
	outcome toolkitstream.Outcome
}

func newParser() *parser { return &parser{} }

func (p *parser) Parse(chunk []byte) ([]toolkitstream.Event, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	var out []toolkitstream.Event

	if p.buf.Len()+len(chunk) > maxLineBytes && !bytes.Contains(chunk, []byte{'\n'}) {
		p.buf.Reset()
		out = append(out, toolkitstream.Event{
			Type: toolkitstream.EventTextDelta,
			Text: "[stream truncated]",
		})
		return out, nil
	}
	p.buf.Write(chunk)

	for {
		idx := bytes.IndexByte(p.buf.Bytes(), '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), p.buf.Bytes()[:idx]...)
		rest := p.buf.Bytes()[idx+1:]
		p.buf.Reset()
		p.buf.Write(rest)
		out = append(out, p.decodeLine(line)...)
	}
	return out, nil
}

func (p *parser) Done() ([]toolkitstream.Event, error) {
	if p.buf.Len() == 0 {
		return nil, nil
	}
	line := append([]byte(nil), p.buf.Bytes()...)
	p.buf.Reset()
	return p.decodeLine(line), nil
}

type claudeEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Result  string `json:"result,omitempty"`
	Message struct {
		Content []claudeContentBlock `json:"content,omitempty"`
	} `json:"message"`
	// TotalCostUSD is a POINTER so absence is distinguishable from zero. The
	// two are different statements: an explicit 0 is the toolkit reporting that
	// the provider metered nothing, which is the credential-halt signal, while
	// an absent field is output that says nothing about billing at all — an
	// unrecognized `result` subtype, or a release that stopped emitting it.
	// Decoded into a plain float64 they are the same value, and the second
	// silently becomes the first. See Outcome.Unbilled below.
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
	DurationMs   int64    `json:"duration_ms,omitempty"`
}

// cost returns the run's billing tally, or 0 when the event carried none.
// Outcome.CostUSD and Event.CostUSD are both plain float64s and both mean "no
// cost recorded" by 0 — only Unbilled needs the presence bit.
func (e claudeEvent) cost() float64 {
	if e.TotalCostUSD == nil {
		return 0
	}
	return *e.TotalCostUSD
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (p *parser) decodeLine(line []byte) []toolkitstream.Event {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	switch ev.Type {
	case "assistant":
		return decodeAssistantBlocks(ev.Message.Content)
	case "user":
		return decodeUserBlocks(ev.Message.Content)
	case "result":
		p.outcome = toolkitstream.Outcome{
			Text:       ev.Result,
			HasResult:  true,
			OK:         ev.Subtype == "success",
			DurationMs: ev.DurationMs,
			CostUSD:    ev.cost(),
			// Only a result that ACTUALLY reported a tally licenses the claim
			// Outcome.Unbilled describes. A `result` event carrying no
			// total_cost_usd has said nothing about billing, so it cannot state
			// that the provider metered nothing — and the field's own doc is
			// explicit that a toolkit whose terminal event carries no tally
			// leaves Unbilled false. Reading absence as zero would let an
			// unrecognized subtype, or a claude release that dropped the field,
			// halt the session and tell the operator to rotate a healthy
			// credential.
			Unbilled: ev.TotalCostUSD != nil && *ev.TotalCostUSD == 0,
		}
		return []toolkitstream.Event{{
			Type:       toolkitstream.EventResult,
			OK:         ev.Subtype == "success",
			DurationMs: ev.DurationMs,
			CostUSD:    ev.cost(),
		}}
	case "system":
		return nil
	}
	return nil
}

func decodeAssistantBlocks(blocks []claudeContentBlock) []toolkitstream.Event {
	var out []toolkitstream.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, toolkitstream.Event{
					Type: toolkitstream.EventTextDelta,
					Text: b.Text,
				})
			}
		case "tool_use":
			out = append(out, toolkitstream.Event{
				Type:     toolkitstream.EventToolUseStart,
				ToolName: b.Name,
				ToolID:   b.ID,
				Summary:  summarizeToolInput(b.Name, b.Input),
			})
		}
	}
	return out
}

func decodeUserBlocks(blocks []claudeContentBlock) []toolkitstream.Event {
	var out []toolkitstream.Event
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		out = append(out, toolkitstream.Event{
			Type:    toolkitstream.EventToolUseStop,
			ToolID:  b.ToolUseID,
			OK:      !b.IsError,
			Summary: summarizeToolOutput(b.Content),
		})
	}
	return out
}

// fileToolVerbs glosses Claude's built-in file tools in the present
// progressive. A new file tool is a row here, not another switch arm.
//
// Every one of them names its target `file_path` — NOT `path`. That
// distinction is the whole point of this map: a lookup on the wrong key
// matches nothing, so each line silently degrades to the raw-JSON fallback
// below, and the chat renderers show a bare tool name with no target.
var fileToolVerbs = map[string]string{
	"Read":  "Reading",
	"Edit":  "Editing",
	"Write": "Writing",
}

func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return name + "(...)"
	}
	var fields map[string]any
	_ = json.Unmarshal(input, &fields)
	// Claude's built-in tool names and input schema are stable contract;
	// unknown names hit the generic fallback below.
	if verb, ok := fileToolVerbs[name]; ok {
		if p, ok := fields["file_path"].(string); ok {
			return verb + " " + p
		}
	}
	if name == "Bash" {
		if c, ok := fields["command"].(string); ok {
			return truncate("$ "+c, 80)
		}
	}
	return name + "(" + truncate(string(input), 60) + ")"
}

// summarizeToolOutput pulls a one-line gloss from a tool_result content
// payload. Claude emits content either as a bare string ("ok\n") or as
// an array of typed content blocks ([{"type":"text","text":"..."}]); we
// try the string shape first and fall back to the array's first text block.
func summarizeToolOutput(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return truncate(firstLine(s), 80)
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return truncate(firstLine(b.Text), 80)
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (p *parser) Outcome() toolkitstream.Outcome { return p.outcome }
