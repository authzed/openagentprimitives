package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxRequestWhyRunes bounds the child's stated reason. Refused rather than
// truncated, for the same reason ask_parent refuses an over-long question: a
// truncated reason can read as a different reason, and the approver deciding
// on it has no way to know it read half of one.
const maxRequestWhyRunes = 2048

// maxRequestSlotRunes matches DataSlotRequest.Slot's own CRD bound, so Execute
// never records something the apiserver would later reject.
const maxRequestSlotRunes = 64

// RequestInputConfig wires request_input.
type RequestInputConfig struct {
	// Record routes the request to the DELEGATING AGENT as a plan amendment.
	// Required; a nil Record refuses every call rather than panicking.
	//
	// A plan amendment rather than a new approval kind, deliberately: asking
	// for more data mid-flight is a request to widen what this delegation
	// covers, which is what that category already means. A second flow would
	// need its own card, its own audience resolution and its own audit shape,
	// all to say the same thing.
	Record func(ctx context.Context, slot, why string) error

	// Await is the SAME yield configuration ask_parent gets, because the park
	// is the same park: the child stops, its parent gets control, and the
	// reply re-hydrates this session.
	//
	// It has to wait. Recording the request without yielding leaves the child
	// running while the only party who can answer is blocked inside its own
	// delegate call — so the ask reaches nobody who can act on it until the
	// child finishes, by which time there is nothing to send it to.
	Await AwaitConfig
}

// NewRequestInput builds the child's mid-flight data request.
func NewRequestInput(cfg RequestInputConfig) tool.Tool { return &requestInputTool{cfg: cfg} }

type requestInputTool struct{ cfg RequestInputConfig }

func (*requestInputTool) Name() string    { return "request_input" }
func (*requestInputTool) Kind() tool.Kind { return tool.KindMeta }

func (*requestInputTool) Permission() authz.Permission {
	// This tool GRANTS NOTHING and reaches no resource. It records an ask that
	// the delegating agent may act on, and the bound on what that agent can
	// then bind is enforced where every other slot bound is — the attenuation
	// check at handoff, which refuses any tag the parent cannot itself read.
	//
	// Putting a permission here instead would be the wrong place for it: it
	// would gate the ASKING rather than the granting, and a child that may not
	// ask is not the property anyone wants — the property is that asking
	// cannot widen anything.
	return authz.Permission{StateImpact: authz.Stateless}
}

func (*requestInputTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*requestInputTool) Description() string {
	return "Ask the agent that delegated this task to you for data you were not given, by naming one of your " +
		"declared input slots. Use it when the task refers to something you cannot see — a diff, a log, a document — " +
		"and you have no slot holding it. " +
		"This PAUSES you until that agent responds, so only ask when you cannot finish without the data; if you can " +
		"finish without it, finish without it. The data may still not arrive: the other agent decides whether to " +
		"send it, can only send what it is itself allowed to read, and a person may have to approve the sharing. " +
		"Say plainly why you need it — that reason is what the other agent, and any person approving, actually reads."
}

func (*requestInputTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"slot": {
				"type": "string",
				"description": "The name of one of your declared input slots -- the one you want filled. Not a description of the data and not a file path: the slot name your class declared."
			},
			"why": {
				"type": "string",
				"description": "Why you need it, in your own words. The delegating agent sees only this -- not your reasoning or your tool results -- and an approver may see it too, so make it stand alone."
			}
		},
		"required": ["slot", "why"]
	}`)
}

type requestInputArgs struct {
	Slot string `json:"slot"`
	Why  string `json:"why"`
}

func (t *requestInputTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	if t.cfg.Record == nil {
		return tool.Result{
			Content: "request_input: this session has no delegating agent to ask, so the request cannot be sent.",
			IsError: true, Trusted: true,
		}, nil
	}
	var args requestInputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return tool.Result{Content: "request_input: " + err.Error(), IsError: true, Trusted: true}, nil
	}
	slot := strings.TrimSpace(args.Slot)
	why := strings.TrimSpace(args.Why)
	if slot == "" || why == "" {
		return tool.Result{
			Content: "request_input: both slot and why are required — name the slot you want filled and say why you need it.",
			IsError: true, Trusted: true,
		}, nil
	}
	if utf8.RuneCountInString(slot) > maxRequestSlotRunes {
		return tool.Result{
			Content: fmt.Sprintf("request_input: slot name is longer than %d characters, which is not a slot any class declares.", maxRequestSlotRunes),
			IsError: true, Trusted: true,
		}, nil
	}
	if utf8.RuneCountInString(why) > maxRequestWhyRunes {
		return tool.Result{
			Content: fmt.Sprintf("request_input: the reason is longer than %d characters. Refused rather than shortened, because a truncated reason reads as a different one to whoever decides on it.", maxRequestWhyRunes),
			IsError: true, Trusted: true,
		}, nil
	}
	if err := t.cfg.Record(ctx, slot, why); err != nil {
		// Surfaced, never swallowed: a child that believes it asked and did
		// not would wait on data nobody was ever told about.
		return tool.Result{
			Content: "request_input: the request could not be sent to the delegating agent: " + err.Error(),
			IsError: true, Trusted: true,
		}, nil
	}
	// Record BEFORE yielding, the same order ask_parent takes and for the same
	// reason: the parent learns of the request only through that write, so a
	// yield ahead of a failed one would park the child on a request nobody can
	// see, to wait out the whole idle TTL in silence.
	switch yieldAndWait(ctx, t.cfg.Await) {
	case awaitDisabled:
		return tool.Result{
			Content: fmt.Sprintf("Requested %q; parking until the delegating agent responds "+
				"(this session exits to phase=Idle and resumes when it does).", slot),
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	case awaitTTL:
		return tool.Result{
			Content: fmt.Sprintf("No response yet on %q; parking until the delegating agent responds "+
				"(this session exits to phase=Idle and resumes when it does).", slot),
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	}
	// awaitResumed. The bare acknowledgment await_user_message returns, for the
	// same reason: the parent's reply is already spliced in as the next turn,
	// and the DATA — if it cleared — arrives as its own turn beside it. Pointing
	// the model at memory instead would make a missed delivery catastrophic
	// rather than merely quiet.
	return tool.Result{
		Content:      "acknowledged",
		AwaitResumed: true,
		Trusted:      true,
	}, nil
}
