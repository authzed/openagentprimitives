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

// titleMaxRunes bounds the title to a width that renders cleanly across Slack
// surfaces; longer titles are ellipsis-trimmed rather than rejected.
const titleMaxRunes = 100

// emojiMaxRunes bounds the optional glyph so a stray sentence can't land in
// the glyph slot; an over-long value is treated as unset.
const emojiMaxRunes = 16

// SetThreadTitleConfig wires set_thread_title to the runner's NATS publisher.
// Mirrors UpdateStatusConfig.
type SetThreadTitleConfig struct {
	NATSPublish       func(ctx context.Context, subject string, payload []byte) error
	NATSSubjectPrefix string

	// EnvelopeSigner signs every envelope set_thread_title publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner
}

// NewSetThreadTitle constructs the set_thread_title meta-tool. Only registered
// when the session is channel-attached; the channel kind's thread_title
// sub-channel sender renders the title.
func NewSetThreadTitle(cfg SetThreadTitleConfig) tool.Tool {
	return &setThreadTitleTool{cfg: cfg}
}

type setThreadTitleTool struct{ cfg SetThreadTitleConfig }

func (*setThreadTitleTool) Name() string    { return "set_thread_title" }
func (*setThreadTitleTool) Kind() tool.Kind { return tool.KindMeta }
func (*setThreadTitleTool) Permission() authz.Permission {
	// set_thread_title posts a conversation-title update to the channel. Like
	// update_status it is session-scoped and gated by AgentSession#interact;
	// treated as Passthrough until slice 2.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*setThreadTitleTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*setThreadTitleTool) Description() string {
	return "Set a short, human-readable title for the current conversation — e.g. " +
		"\"Refactoring the auth layer\" or \"Q3 revenue analysis\". This is a label, NOT the " +
		"answer; the answer goes via respond_to_user. You may call this again later to change " +
		"the title. `title` is REQUIRED. `emoji` is OPTIONAL: omit it to keep the default 🤖 " +
		"glyph, or pass a single literal emoji (e.g. \"🔧\", \"📊\") to mark the conversation's " +
		"nature. Use a literal emoji character rather than a :shortcode: — a shortcode may not " +
		"render in direct-message titles."
}

func (*setThreadTitleTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"title": {
				"type": "string",
				"description": "The conversation title. A short human-readable label (~100 chars max; longer is trimmed)."
			},
			"emoji": {
				"type": "string",
				"description": "Optional single literal emoji to use as the leading glyph, replacing the default 🤖. Omit to keep 🤖."
			}
		},
		"required": ["title"]
	}`)
}

func (t *setThreadTitleTool) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.cfg.NATSPublish == nil {
		return tool.Result{Trusted: true}, errors.New("set_thread_title: NATSPublish is unset")
	}
	var in struct {
		Title string `json:"title"`
		Emoji string `json:"emoji,omitempty"`
	}
	if res, ok := tool.ParseArgs(args, &in, t.Name(), `{"title": "Refactoring the auth layer", "emoji": "🔧"}`); !ok {
		return res, nil
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return tool.Result{Content: "set_thread_title: `title` is required and must be non-empty. Pass a short label for the conversation, e.g. {\"title\": \"Refactoring the auth layer\"}.", IsError: true, Trusted: true}, nil
	}
	title = clampRunes(title, titleMaxRunes)

	emoji := strings.TrimSpace(in.Emoji)
	if len([]rune(emoji)) > emojiMaxRunes {
		emoji = "" // a stray sentence in the emoji slot is treated as unset
	}

	seq, uid := seqFromCtx(ctx)
	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, t.cfg.NATSPublish, subject, data)
	}
	if err := t.cfg.EnvelopeSigner.PublishOutSeq(publish, sess.Namespace, sess.Name,
		channelevents.KindThreadTitle,
		channelevents.ThreadTitlePayload{Title: title, Emoji: emoji}, seq, uid,
	); err != nil {
		return tool.Result{Content: fmt.Sprintf("publish failed: %v", err), IsError: true, Trusted: true}, nil
	}
	return tool.Result{Content: "title updated", Trusted: true}, nil
}

// clampRunes trims s to at most n runes, appending an ellipsis when trimmed.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
