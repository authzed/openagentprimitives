package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// UpdateStatusConfig wires update_status to the runner's NATS publisher.
// Mirrors RespondConfig but uses the notification sub-channel kind so
// channelsd renders status updates as in-place edits of the working
// message instead of new threaded replies.
type UpdateStatusConfig struct {
	NATSPublish       func(ctx context.Context, subject string, payload []byte) error
	NATSSubjectPrefix string

	// EnvelopeSigner signs every envelope update_status publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner
}

// NewUpdateStatus constructs the update_status meta-tool. Only registered
// when the session is channel-attached; the channel kind's Sender
// interprets notification envelopes as edit-the-current-status semantics.
func NewUpdateStatus(cfg UpdateStatusConfig) tool.Tool {
	return &updateStatusTool{cfg: cfg}
}

type updateStatusTool struct{ cfg UpdateStatusConfig }

func (*updateStatusTool) Name() string    { return "update_status" }
func (*updateStatusTool) Kind() tool.Kind { return tool.KindMeta }
func (*updateStatusTool) Permission() authz.Permission {
	// update_status posts a transient progress notification to the
	// channel. Like respond_to_user it is session-scoped and gated by
	// AgentSession#interact; treated as Passthrough until slice 2.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*updateStatusTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*updateStatusTool) Description() string {
	return "Post a short progress update to the channel by editing the current working message in place. " +
		"Use this during long-running work (multiple tool calls, fetching/processing data) so the user sees " +
		"that something is happening rather than silence. Each call REPLACES the previous status. Examples: " +
		"\"Fetching commits from authzed/spicedb…\", \"Found 47 commits, summarizing…\", " +
		"\"Cross-referencing with releases…\". Keep these short (one line, no markdown formatting) — they're " +
		"transient progress markers, not the final answer. The final answer goes via respond_to_user. " +
		"BOTH `text` and `short` are REQUIRED on every call: `text` is the full status (keep to ~70 chars — " +
		"the channel may append a live token/time progress indicator, and over-long captions are replaced " +
		"with a generic one) shown in the thread-top indicator; `short` is the ≤50-character abbreviated variant shown in " +
		"surfaces that reject longer strings (e.g. Slack's bottom-of-channel typing indicator). Pick a " +
		"meaningful 50-character abbreviation rather than letting the system truncate. " +
		"Optionally include `expected_duration_seconds` when the next operation is expected to take more " +
		"than ~30 seconds (large data fetch, slow MCP call, big diff/grep, generating a large artifact). The " +
		"runtime watchdog uses this hint to extend its silence window so you don't get a spurious \"taking " +
		"longer than expected\" warning during a known-slow op. Pass however many seconds you genuinely " +
		"expect — there is no upper limit you need to stay under; large values are fine and the runtime " +
		"bounds them internally for safety. Omit (or pass 0) for routine sub-30-second steps."
}

func (*updateStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"text": {
				"type": "string",
				"description": "The full status text. Keep to ~70 characters: the channel may append a live progress indicator (tokens + elapsed), and over-long captions are replaced with a generic one. Replaces the previous status in the thread-top indicator."
			},
			"short": {
				"type": "string",
				"description": "≤50-character abbreviated variant of text. Used in surfaces that reject longer strings (Slack's bottom-of-channel typing indicator). Required — pick a meaningful abbreviation rather than letting the system truncate."
			},
			"expected_duration_seconds": {
				"type": "integer",
				"minimum": 0,
				"description": "Optional. Hint that the next operation is expected to take roughly this many seconds. The watchdog uses it to extend its silence window so a known-slow op doesn't trigger a spurious 'taking longer than expected' warning. No upper limit — pass whatever you genuinely expect (large values for a big artifact generation are fine; the runtime bounds them internally for safety). Omit (or 0) for routine sub-30-second steps."
			}
		},
		"required": ["text", "short"]
	}`)
}

func (t *updateStatusTool) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.cfg.NATSPublish == nil {
		return tool.Result{Trusted: true}, errors.New("update_status: NATSPublish is unset")
	}
	var in struct {
		// Text is the full status line; Short is the abbreviated form a
		// space-constrained channel surface renders instead.
		Text  string `json:"text"`
		Short string `json:"short"`
		// ExpectedDurationSeconds is the agent's own estimate; zero means it
		// offered none, not "instant".
		ExpectedDurationSeconds int `json:"expected_duration_seconds,omitempty"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"text": "full status", "short": "≤50-char abbrev"}`); !ok {
		return res, nil
	}
	if strings.TrimSpace(in.Text) == "" {
		return tool.Result{Content: "update_status: `text` is required and must be non-empty. Pass the full status string (≤70 chars) you want the user to see, e.g. {\"text\": \"Fetching commits…\", \"short\": \"Fetching\"}.", IsError: true, Trusted: true}, nil
	}
	if strings.TrimSpace(in.Short) == "" {
		return tool.Result{Content: "update_status: `short` is required and must be non-empty. Provide a ≤50-character abbreviation of `text` (Slack's loading_messages indicator caps at 50 chars). Pick a meaningful abbreviation rather than letting it get truncated.", IsError: true, Trusted: true}, nil
	}
	if in.ExpectedDurationSeconds < 0 {
		return tool.Result{Content: fmt.Sprintf("update_status: `expected_duration_seconds` must be >= 0 (got %d). Omit the field for routine sub-30-second steps; pass a positive integer when announcing a known-slow operation (no upper limit).", in.ExpectedDurationSeconds), IsError: true, Trusted: true}, nil
	}

	// Stamp the logical Seq + session UID from this tool call's IDs context so
	// the consumer can order status captions and drop stale ones.
	seq, uid := seqFromCtx(ctx)
	if err := publishStatusNotification(ctx, t.cfg.NATSPublish, t.cfg.EnvelopeSigner, sess.Namespace, sess.Name,
		channelevents.NotificationPayload{
			Text:                    in.Text,
			Short:                   in.Short,
			ExpectedDurationSeconds: in.ExpectedDurationSeconds,
		}, seq, uid,
	); err != nil {
		return tool.Result{Content: fmt.Sprintf("publish failed: %v", err), IsError: true, Trusted: true}, nil
	}
	return tool.Result{Content: "status updated", Trusted: true}, nil
}

// publishStatusNotification emits a KindNotification — the in-place status
// caption channels render as "what the agent is doing right now". Shared by
// update_status (the explicit tool) and update_plan (which mirrors a plan's
// in_progress step into the same caption). Text/Short are sent verbatim: any
// per-channel size limits (e.g. Slack's loading_messages rune cap, or a
// too-long caption falling back to a generic one) are the channel kind's
// concern, not this channel-agnostic layer's. seq/uid stamp the envelope's
// logical order + session instance for the consumer; pass (0, "") when no IDs
// context is available.
func publishStatusNotification(
	ctx context.Context,
	natsPublish func(ctx context.Context, subject string, payload []byte) error,
	signer *channelevents.EnvelopeSigner,
	ns, name string,
	pl channelevents.NotificationPayload,
	seq uint64,
	uid string,
) error {
	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, natsPublish, subject, data)
	}
	return signer.PublishOutSeq(publish, ns, name, channelevents.KindNotification, pl, seq, uid)
}
