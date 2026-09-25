package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// ReadChannelHistoryConfig configures the read_channel_history tool. The runner
// builds one per channel-attached session whose Channel opted in AND whose
// gating (info-leakage on, or output==input) permits it.
type ReadChannelHistoryConfig struct {
	RequestSubject    string
	NATSRequest       func(ctx context.Context, subject string, payload []byte) ([]byte, error)
	SupportsDateRange bool
}

type readChannelHistoryTool struct {
	cfg    ReadChannelHistoryConfig
	schema json.RawMessage
}

func NewReadChannelHistory(cfg ReadChannelHistoryConfig) tool.Tool {
	props := `"limit":{"type":"integer","description":"Max messages to fetch (default 50)."},
	"lookback_days":{"type":"integer","description":"Reach back this many days (kind clamps to its maximum, ~30 for Slack)."},
	"before_cursor":{"type":"string","description":"Opaque cursor from a previous call's oldest_cursor; omit on the first call."}`
	if cfg.SupportsDateRange {
		props += `,
	"since":{"type":"string","description":"RFC3339 timestamp; only messages at or after this time."},
	"until":{"type":"string","description":"RFC3339 timestamp; only messages at or before this time."}`
	}
	return &readChannelHistoryTool{
		cfg:    cfg,
		schema: json.RawMessage(`{"type":"object","properties":{` + props + `}}`),
	}
}

func (t *readChannelHistoryTool) Name() string                 { return "read_channel_history" }
func (t *readChannelHistoryTool) Kind() tool.Kind              { return tool.KindMeta }
func (t *readChannelHistoryTool) InputSchema() json.RawMessage { return t.schema }
func (t *readChannelHistoryTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (t *readChannelHistoryTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (t *readChannelHistoryTool) Description() string {
	return "Retrieve older messages from the ENTIRE channel this session is in (not just this thread). " +
		"Use it to answer questions about channel activity outside the current conversation. " +
		"Paginate by passing the returned oldest_cursor as before_cursor."
}

type readChannelHistoryArgs struct {
	// Limit caps returned messages; zero takes the schema default.
	Limit int `json:"limit"`
	// LookbackDays bounds how far back to scan; zero takes the schema default.
	LookbackDays int `json:"lookback_days"`
	// BeforeCursor pages backwards from a prior result's oldest_cursor.
	BeforeCursor string `json:"before_cursor"`
	// Since and Until bound the window; empty means unbounded on that side.
	Since string `json:"since"`
	Until string `json:"until"`
}

func (t *readChannelHistoryTool) Execute(ctx context.Context, args json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var in readChannelHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return tool.Result{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
		}
	}
	if in.Limit <= 0 {
		in.Limit = 50
	}
	if t.cfg.NATSRequest == nil {
		return tool.Result{Content: "read_channel_history is not available for this session", IsError: true}, nil
	}
	reqBytes, err := json.Marshal(channelevents.ChannelHistoryRequest{
		Limit: in.Limit, LookbackDays: in.LookbackDays, BeforeCursor: in.BeforeCursor,
		Since: in.Since, Until: in.Until,
	})
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("encode request: %v", err), IsError: true}, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	replyBytes, err := t.cfg.NATSRequest(reqCtx, t.cfg.RequestSubject, reqBytes)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("channel history request failed: %v", err), IsError: true}, nil
	}
	var resp channelevents.HistoryResponse
	if err := json.Unmarshal(replyBytes, &resp); err != nil {
		return tool.Result{Content: fmt.Sprintf("decode response: %v", err), IsError: true}, nil
	}
	if resp.Error != "" {
		return tool.Result{Content: "channel history unavailable: " + resp.Error, IsError: true}, nil
	}
	if len(resp.Messages) == 0 {
		// Trusted is left false (untrusted-by-default): channel content is
		// third-party and must run through content guards.
		return tool.Result{Content: "No channel messages found in that window."}, nil
	}
	return renderHistoryResponse(resp), nil
}
