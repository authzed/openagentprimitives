package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// UpdateOpeningSummaryConfig carries the write-side leakage gate, shared with
// respond_to_user. The tool writes only session-state: no NATS, no envelope.
type UpdateOpeningSummaryConfig struct {
	// LeakageGate, when non-nil, is called before the body is written to the
	// openingsummary store. Mirrors RespondConfig.LeakageGate exactly — same
	// signature, same "return nil to allow" contract — so Task 4's capability
	// can wire respond_to_user's gate straight through. A blocked body is
	// never recorded: the store keeps whatever it held before this call.
	LeakageGate func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error
}

// NewUpdateOpeningSummary constructs the update_opening_summary meta-tool.
// Offered only for triggered inputs (wired by Task 4's capability); it sets
// the enrichment body a triggered session's pinned opening message renders
// underneath the original delivery text.
func NewUpdateOpeningSummary(cfg UpdateOpeningSummaryConfig) tool.Tool {
	return &updateOpeningSummaryTool{cfg: cfg}
}

type updateOpeningSummaryTool struct{ cfg UpdateOpeningSummaryConfig }

func (*updateOpeningSummaryTool) Name() string    { return "update_opening_summary" }
func (*updateOpeningSummaryTool) Kind() tool.Kind { return tool.KindMeta }
func (*updateOpeningSummaryTool) Permission() authz.Permission {
	// Writes only session-state (no channel publish of its own); the
	// channel-facing write happens later when the pinned message is
	// re-rendered, which is gated the same way respond_to_user's publish is.
	// Treated as Passthrough here, matching update_status/respond_to_user.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional variants
// today (only MCP-tooled AgentClasses use them).
func (*updateOpeningSummaryTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*updateOpeningSummaryTool) Description() string {
	return "Set the short summary pinned to the top of this thread's opening message (e.g. findings so far, " +
		"files reviewed). Each call REPLACES the previous summary — this is not a running log, it's the " +
		"current state. Pass an empty string to clear it. This is not a reply to the person — use " +
		"respond_to_user for that."
}

func (*updateOpeningSummaryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"body": {
				"type": "string",
				"description": "A short summary pinned to the top of the thread (findings so far, files reviewed). Replaces the previous summary each call; pass an empty string to clear it. This is not a reply — use respond_to_user for messages to the person."
			}
		},
		"required": ["body"]
	}`)
}

func (t *updateOpeningSummaryTool) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var in struct {
		Body string `json:"body"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"body": "2 findings: cache TTL race, nil-check"}`); !ok {
		return res, nil
	}

	// Same write-side info-leakage gate as respond_to_user (model text is
	// otherwise trusted, but this text lands on a channel-visible pinned
	// message, so it is gated identically before anything is recorded).
	if t.cfg.LeakageGate != nil {
		if err := t.cfg.LeakageGate(ctx, sess, in.Body, nil); err != nil {
			return tool.Result{Content: fmt.Sprintf("information-leakage gate: %v", err), IsError: true, Trusted: true}, nil
		}
	}

	body := in.Body
	if stripped, had := toolenvelope.StripPt(body); had {
		body = stripped
	}

	store, ok := openingsummary.TryFrom(sess)
	if !ok {
		// Not fatal to the turn: log with context, tell the model it didn't
		// stick, so it can decide whether to fall back to respond_to_user.
		log.FromContext(ctx).Info("update_opening_summary: no opening-summary state on this session; the pinned message will not be updated",
			"session", sess.Namespace+"/"+sess.Name)
		return tool.Result{Content: "this session has no pinned opening message to update", IsError: true, Trusted: true}, nil
	}
	if err := store.SetBody(ctx, body); err != nil {
		return tool.Result{Content: fmt.Sprintf("recording the summary failed: %v", err), IsError: true, Trusted: true}, nil
	}
	return tool.Result{Content: "pinned summary updated", Trusted: true}, nil
}
