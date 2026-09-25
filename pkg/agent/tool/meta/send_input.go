package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// SendInputConfig wires the parent's answer to a child's request_input.
type SendInputConfig struct {
	// Bind appends one data slot to a live delegation's spec. Required; a nil
	// Bind refuses every call rather than panicking.
	//
	// The SPEC, not a grant: this records what the parent is OFFERING, and the
	// SubagentRequest controller decides whether it lands — attenuation
	// (can the parent read this tag at all?) then grading (would giving it to
	// THIS child disclose?), with a disclosing one routed to a human. The
	// parent's judgment selects within an envelope it cannot widen.
	Bind func(ctx context.Context, requestName, slot, tagID string) error

	// ResolveDataTag maps a tool_use_id from THIS session to the tag minted
	// for it — the same server-side resolution delegate's `inputs` uses, and
	// for the same reason: a model never handles a tag id, so it can only
	// offer data it actually produced here.
	ResolveDataTag func(ctx context.Context, toolUseID string) (tagID string, err error)
}

// NewSendInputTool builds the parent's mid-flight data hand-over.
func NewSendInputTool(cfg SendInputConfig) tool.Tool { return &sendInputTool{cfg: cfg} }

type sendInputTool struct{ cfg SendInputConfig }

func (*sendInputTool) Name() string    { return "send_input" }
func (*sendInputTool) Kind() tool.Kind { return tool.KindMeta }

func (*sendInputTool) Permission() authz.Permission {
	// Stateless for the same reason delegate is: this GRANTS nothing. It
	// records an offer, and every bound on what can actually reach the child
	// is enforced operator-side against live state. A permission here would
	// gate the offering rather than the granting.
	return authz.Permission{StateImpact: authz.Stateless}
}

// PlanGateGoverned marks handing a live child data as an action the plan gate
// governs, alongside delegate and reply_to_subagent. The three are one
// capability -- spawn, re-instruct, feed -- and governing only the first lets a
// parent that has advanced out of the delegating phase keep directing the child
// it spawned there. The gate sees tool:send_input; dispatch stays Stateless.
func (*sendInputTool) PlanGateGoverned() bool                        { return true }
func (*sendInputTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*sendInputTool) Description() string {
	return "Send data to an agent you delegated to, filling one of its input slots — normally in answer to a " +
		"request it made. You name the tool call in THIS conversation whose result holds the data; the data itself " +
		"is passed by reference, never pasted, so it keeps the permissions it had when you read it. You can only " +
		"send results of calls you made, and only data you are allowed to read. Sending may require a person's " +
		"approval if the other agent's readers are not already entitled to see it, in which case it arrives once " +
		"they approve, or not at all. Do not paste the data into a message instead: that strips its permissions and " +
		"the other agent may act on something it was never authorized to see."
}

func (*sendInputTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"delegation": {
				"type": "string",
				"description": "The delegation handle you were given when you delegated to this agent."
			},
			"slot": {
				"type": "string",
				"description": "The input slot to fill — the name the other agent used when it asked."
			},
			"tool_use_id": {
				"type": "string",
				"description": "The id of the tool call in this conversation whose result holds the data."
			}
		},
		"required": ["delegation", "slot", "tool_use_id"]
	}`)
}

type sendInputArgs struct {
	Delegation string `json:"delegation"`
	Slot       string `json:"slot"`
	ToolUseID  string `json:"tool_use_id"`
}

func (t *sendInputTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args sendInputArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(),
		`{"delegation": "subreq-lead-abc12", "slot": "diff", "tool_use_id": "toolu_7"}`); !ok {
		return res, nil
	}
	refuse := func(format string, a ...any) (tool.Result, error) {
		return tool.Result{
			Content: t.Name() + ": " + fmt.Sprintf(format, a...),
			IsError: true, Trusted: true,
		}, nil
	}
	if strings.TrimSpace(args.Delegation) == "" {
		return refuse("delegation must not be empty; use the handle from the delegate call")
	}
	if strings.TrimSpace(args.Slot) == "" {
		return refuse("slot must not be empty; use the name the other agent asked for")
	}
	id := strings.TrimSpace(args.ToolUseID)
	if id == "" {
		return refuse("tool_use_id must name the call whose result holds the data")
	}
	if t.cfg.Bind == nil || t.cfg.ResolveDataTag == nil {
		return refuse("this session cannot send data to another agent")
	}

	tagID, err := t.cfg.ResolveDataTag(ctx, id)
	if err != nil {
		return refuse("could not look up the data from call %q: %v", id, err)
	}
	if tagID == "" {
		// Distinct from an error, and the distinction matters to the model:
		// nothing is broken, this particular call simply produced no data that
		// can be handed over, and the useful next move is a different call.
		return refuse("the call %q produced no data that can be sent — only results from tools that read a tracked resource can fill a slot", id)
	}
	if err := t.cfg.Bind(ctx, strings.TrimSpace(args.Delegation), strings.TrimSpace(args.Slot), tagID); err != nil {
		return refuse("could not offer the data to %q: %v", args.Delegation, err)
	}
	// Deliberately does NOT claim the data arrived. Attenuation and grading
	// run operator-side after this returns, and a disclosing datum waits on a
	// person — so reporting delivery here would have the parent tell the child
	// it has something it may never get.
	return tool.Result{
		Content: fmt.Sprintf("Offered the result of %s for the %q slot. "+
			"It reaches the other agent once the platform clears it, which may need a person's approval.",
			id, args.Slot),
		Trusted: true,
	}, nil
}
