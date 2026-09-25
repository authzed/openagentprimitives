package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// ShowAttachmentConfig carries the session-scoped pin function. Pin is
// supplied by the runner and validates the handle against the session's own
// turns; a nil Pin means the tool is unusable and it should not have been
// offered (see attachmentsCapability.Offer).
type ShowAttachmentConfig struct {
	Pin func(ctx context.Context, handle string) error
}

// NewShowAttachment returns the show_attachment tool.
func NewShowAttachment(cfg ShowAttachmentConfig) tool.Tool {
	return &showAttachment{pin: cfg.Pin}
}

type showAttachment struct {
	pin func(ctx context.Context, handle string) error
}

func (*showAttachment) Name() string    { return "show_attachment" }
func (*showAttachment) Kind() tool.Kind { return tool.KindMeta }

// Permission is Stateless: the tool changes only which of THIS session's own
// attachments are rendered into its next request. It reads no external
// resource and mutates nothing outside the session the AgentSession#interact
// check already gates.
func (*showAttachment) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}

func (*showAttachment) PermissionVariants() []authz.PermissionVariant { return nil }

func (*showAttachment) Description() string {
	return "Bring an attached file back into view when it is no longer visible to you. " +
		"Use this ONLY after a note tells you a file has dropped out of view — the most recent attachments are already visible, " +
		"and calling this for one of those does nothing useful. Pass the file's handle exactly as the note gave it. " +
		"The file becomes visible on your NEXT message, not in this tool's result: the result only confirms it will be there, " +
		"so continue your work and look at the file on the following turn. " +
		"This does not work for files that were never readable in the first place — if you were told a file's type cannot be read, " +
		"this cannot change that."
}

func (*showAttachment) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"handle": {
				"type": "string",
				"description": "The attachment handle, copied exactly from the note that said the file is no longer visible."
			}
		},
		"required": ["handle"]
	}`)
}

func (t *showAttachment) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args struct {
		Handle string `json:"handle"`
	}
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"handle": "mem://…"}`); !ok {
		return res, nil
	}
	if args.Handle == "" {
		return tool.Result{
			Content: "show_attachment: `handle` is required. Copy it exactly from the note that told you the file is no longer visible.",
			IsError: true, Trusted: true,
		}, nil
	}
	// A nil pin means the runner offered a tool it cannot back. That is a
	// wiring bug, not bad input, so it returns a Go error (fatal to the turn)
	// rather than an IsError result the model would try to work around.
	if t.pin == nil {
		return tool.Result{Trusted: true}, fmt.Errorf("show_attachment: no pin function wired")
	}
	if err := t.pin(ctx, args.Handle); err != nil {
		return tool.Result{
			Content: fmt.Sprintf("show_attachment: %s could not be brought back into view: %v. "+
				"Check the handle against the note that named it; if it is right, ask the user to re-send the file.", args.Handle, err),
			IsError: true, Trusted: true,
		}, nil
	}
	return tool.Result{
		Content: fmt.Sprintf("%s will be visible again starting with your next message. "+
			"It is NOT in this result — continue, and look at it on the following turn.", args.Handle),
		Trusted: true,
	}, nil
}
