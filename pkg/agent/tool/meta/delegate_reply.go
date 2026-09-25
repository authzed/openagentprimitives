// pkg/agent/tool/meta/delegate_reply.go
//
// The parent's half of a resumable delegation: answering a child that asked a
// question and parked, then waiting for whatever it says next.
//
// delegate and reply_to_subagent are the same shape — do a thing, then wait
// for the delegation's next boundary — and they share DelegateConfig and
// waitForNext for exactly that reason. What differs is the thing: delegate
// CREATES the request, reply_to_subagent DELIVERS a message to an existing
// child.
//
// Delivery is the agent-to-agent inbound path already built for this: a
// channelevents.KindAgentMessageSend envelope published onto the PARENT's own
// inbound bus subject, naming the destination child in the payload. It is the
// sender's own subject and not the child's because that is the only prefix a
// runner's per-session NATS grant authorizes publishing on — and the inversion
// is the security property, not a workaround: the SUBJECT authorizes the
// sender, and the destination is the claim, which channelsd corroborates
// against the one Channel joining the pair before authorizing anything on
// agentsession#converse. Nothing here is a second delivery mechanism; see
// channelevents.KindAgentMessageSend for the full argument.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxReplyRunes bounds one reply to a child. Sized to match the task a
// delegation opens with (maxTaskRunes): an answer mid-conversation can
// legitimately carry as much detail as the instruction that started it.
// Refused rather than truncated, for the same reason a task is.
const maxReplyRunes = maxTaskRunes

// NewSubagentReplyTool constructs the reply_to_subagent meta tool from the
// same config delegate uses.
func NewSubagentReplyTool(cfg DelegateConfig) tool.Tool { return &subagentReplyTool{cfg: cfg} }

type subagentReplyTool struct {
	cfg DelegateConfig
}

func (*subagentReplyTool) Name() string    { return "reply_to_subagent" }
func (*subagentReplyTool) Kind() tool.Kind { return tool.KindMeta }
func (*subagentReplyTool) Permission() authz.Permission {
	// Same reasoning as delegate's: there is no resource-level permission
	// here modelling "may this agent answer that child". Whether the message
	// may be carried at all is re-decided on arrival, by channelsd, against a
	// K8s-witnessed Channel joining the pair and an agentsession#converse
	// check from this session to the child — neither of which this side can
	// influence.
	return authz.Permission{StateImpact: authz.Stateless}
}

// PlanGateGoverned marks re-instructing a live child as an action the plan
// gate governs, for the same reason delegate is governed: this hands the child
// up to 8192 runes of parent-authored instruction, so a parent under injection
// that has advanced out of the phase carrying tool:delegate would otherwise
// keep directing a child it could no longer legally spawn. The gate sees
// tool:reply_to_subagent; dispatch stays Stateless -- no SpiceDB check, no
// per-call approval.
func (*subagentReplyTool) PlanGateGoverned() bool                        { return true }
func (*subagentReplyTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*subagentReplyTool) Description() string {
	return "Answer an agent you delegated to that came back with a question, and wait for what it says next. Pass the " +
		"`delegation` handle the delegate call gave you. Like delegate, this returns either the agent's next question " +
		"(answer it with another reply_to_subagent call) or its final result. The other agent sees ONLY the `message` " +
		"text -- not this conversation, not your reasoning, not your other tool results -- so answer in full rather " +
		"than by reference."
}

func (*subagentReplyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"delegation": {
				"type": "string",
				"description": "The delegation handle from the delegate call that returned the question. Copy it exactly; there is no way to look it up afterwards."
			},
			"message": {
				"type": "string",
				"description": "Your answer, written to stand alone. The other agent sees these words and nothing else of yours."
			}
		},
		"required": ["delegation", "message"]
	}`)
}

type subagentReplyArgs struct {
	Delegation string `json:"delegation"`
	Message    string `json:"message"`
}

func (t *subagentReplyTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args subagentReplyArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"delegation": "subreq-lead-agent-abc12", "message": "use the second repo"}`); !ok {
		return res, nil
	}
	name := strings.TrimSpace(args.Delegation)
	if name == "" {
		return t.refuse("delegation must not be empty; it is the handle the delegate call returned with the question")
	}
	message := strings.TrimSpace(args.Message)
	if message == "" {
		return t.refuse("message must not be empty")
	}
	if len([]rune(message)) > maxReplyRunes {
		return t.refuse(fmt.Sprintf("message is too long (%d runes, max %d) -- shorten it rather than relying on truncation, which would silently change what you answered",
			len([]rune(message)), maxReplyRunes))
	}
	if t.cfg.Poll == nil || t.cfg.Send == nil {
		return t.refuse("not available in this session (no way to reach a delegated agent from here)")
	}

	// Re-read the request rather than trusting anything remembered from the
	// delegate call. Two things depend on it and neither survives a pod
	// restart in memory: which child this handle names, and whether the
	// question is still outstanding. A parent that parked between turns and
	// was re-hydrated holds no in-process state at all — the handle in its
	// transcript is the whole durable record.
	sr, err := t.cfg.Poll(ctx, name)
	if err != nil {
		return t.refuse(fmt.Sprintf("could not read delegation %q: %v. Check the handle from the delegate call that asked the question.", name, err))
	}
	// The handle names a request in this namespace, which is not the same as
	// naming one of THIS session's delegations. Refused here, locally and
	// loudly: the message would in any case be refused on arrival (no Channel
	// joins this session to a stranger's child), but as a channelsd log line
	// the model would never see, after a wait that could only end in a
	// timeout.
	if sr.Spec.Parent.Namespace != t.cfg.Namespace || sr.Spec.Parent.Name != t.cfg.SessionName {
		return t.refuse(fmt.Sprintf("delegation %q is not yours to answer", name))
	}
	if sr.Status.Phase != v1.SubagentRequestPhaseAwaitingParent {
		return t.refuse(fmt.Sprintf("delegation %q is not waiting on you (it is %s). Only a delegation that came back with a question can be answered; start a new one with delegate if you need more work done.",
			name, phaseText(sr.Status.Phase)))
	}
	if sr.Status.ChildRef == nil {
		// Unreachable through the controller, which only ever reaches
		// AwaitingParent by way of a child it created — but a status written
		// by anything else must not become a nil dereference here.
		return t.refuse(fmt.Sprintf("delegation %q reports a question but names no agent to answer", name))
	}
	// Captured BEFORE the send: waiting for the exchange to advance past this
	// number is what stops the poll below re-reading the very question this
	// call is answering and handing it straight back to the model.
	answering := sr.Status.Exchange

	if err := t.cfg.Send(ctx, sr.Status.ChildRef.Namespace, sr.Status.ChildRef.Name, message); err != nil {
		return t.refuse(fmt.Sprintf("could not deliver your answer to delegation %q: %v. This may be retried.", name, err))
	}

	return t.cfg.waitForNext(ctx, t.Name(), name, answering)
}

// refuse builds the tool's own error result. Trusted, because every one of
// these sentences is platform-authored — no child's words reach the model
// through this path, which is why the results carrying a child's words
// (waitForNext's Succeeded and AwaitingParent arms) are the only untrusted
// ones in the delegation surface.
func (t *subagentReplyTool) refuse(msg string) (tool.Result, error) {
	return tool.Result{Content: fmt.Sprintf("%s: %s", t.Name(), msg), IsError: true, Trusted: true}, nil
}

// phaseText renders a SubagentRequest phase for a model. The empty phase is
// the pre-first-reconcile state, which reads as nothing at all when
// interpolated straight into a sentence.
func phaseText(phase string) string {
	if phase == "" {
		return "not yet started"
	}
	return phase
}
