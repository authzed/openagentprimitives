package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// ReadHistoryConfig configures the read_thread_history tool. The runner
// builds one per channel-attached, mention_only (adopted-thread) session.
type ReadHistoryConfig struct {
	// RequestSubject is this session's history request subject
	// (channelevents.HistoryRequestSubject of the session's NATS prefix).
	// It identifies the session to the responder — the responder reads
	// the session from the subject, not from request payload fields.
	RequestSubject string
	// NATSRequest performs a NATS request/reply: it publishes payload on
	// subject and returns the reply bytes. Backed by the runner's NATS
	// connection.
	NATSRequest func(ctx context.Context, subject string, payload []byte) ([]byte, error)
}

type readHistoryTool struct {
	cfg    ReadHistoryConfig
	schema json.RawMessage
}

// NewReadHistory builds the read_thread_history tool.
func NewReadHistory(cfg ReadHistoryConfig) tool.Tool {
	return &readHistoryTool{
		cfg: cfg,
		schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "description": "Maximum number of older messages to fetch (default 25)."
    },
    "before_cursor": {
      "type": "string",
      "description": "Opaque cursor from a previous call's oldest_cursor. Omit on the first call to fetch the messages immediately older than the session's initial backfill."
    }
  }
}`),
	}
}

func (t *readHistoryTool) Name() string    { return "read_thread_history" }
func (t *readHistoryTool) Kind() tool.Kind { return tool.KindMeta }
func (t *readHistoryTool) Description() string {
	return "Retrieve older messages from the conversation that predate this session. " +
		"Use it when the context you were given references something you cannot see. " +
		"Paginate by passing the returned oldest_cursor as before_cursor."
}
func (t *readHistoryTool) InputSchema() json.RawMessage { return t.schema }

// Permission: a pure read with no side effects.
func (t *readHistoryTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (t *readHistoryTool) PermissionVariants() []authz.PermissionVariant { return nil }

type readHistoryArgs struct {
	Limit        int    `json:"limit"`
	BeforeCursor string `json:"before_cursor"`
}

func (t *readHistoryTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in readHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return tool.Result{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
		}
	}
	if in.Limit <= 0 {
		in.Limit = 25
	}
	if t.cfg.NATSRequest == nil {
		return tool.Result{Content: "read_thread_history is not available for this session", IsError: true}, nil
	}

	reqBytes, err := json.Marshal(channelevents.HistoryRequest{
		BeforeCursor: in.BeforeCursor,
		Limit:        in.Limit,
	})
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("encode request: %v", err), IsError: true}, nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	replyBytes, err := t.cfg.NATSRequest(reqCtx, t.cfg.RequestSubject, reqBytes)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("history request failed: %v", err), IsError: true}, nil
	}

	var resp channelevents.HistoryResponse
	if err := json.Unmarshal(replyBytes, &resp); err != nil {
		return tool.Result{Content: fmt.Sprintf("decode response: %v", err), IsError: true}, nil
	}
	if resp.Error != "" {
		return tool.Result{Content: "history unavailable: " + resp.Error, IsError: true}, nil
	}
	if len(resp.Messages) == 0 {
		return tool.Result{Content: "No older messages found."}, nil
	}
	return renderHistoryResponse(resp), nil
}

// renderHistoryResponse turns a non-empty HistoryResponse into the transcript
// the model reads: one "author: text" line per message, then the pagination
// footer.
//
// Shared by read_thread_history and read_channel_history, which differ only in
// the request sent and the empty-result sentence. This rendering is what lands
// in the model's context, so the two must not drift apart.
//
// Trusted is deliberately left false for both callers: history is third-party
// content and must run through the content guards before entering context.
func renderHistoryResponse(resp channelevents.HistoryResponse) tool.Result {
	var b strings.Builder
	for _, m := range resp.Messages {
		name := m.AuthorDisplayName
		if name == "" {
			name = "unknown"
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("\noldest_cursor: %s", resp.OldestCursor))
	if resp.HasMore {
		b.WriteString("\n(more older messages exist — call again with before_cursor set to oldest_cursor)")
	}
	return tool.Result{Content: strings.TrimRight(b.String(), "\n")}
}
