package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxTaskRunes mirrors SubagentRequestSpec.Task's own
// +kubebuilder:validation:MaxLength=8192 -- enforced here too so Execute never
// attempts a Create the apiserver would reject. Refused outright rather than
// truncated (contrast credential_update's capWhyRunes): a truncated delegation
// instruction can silently change what the child is asked to do, where a
// truncated "why" explanation cannot.
const maxTaskRunes = 8192

// DefaultTimeout is the poll ceiling used both as NewDelegateTool's own
// zero-value fallback and by both RunnerEnv wiring sites
// (internal/cmd/runner/main.go, the e2e in-process factory) for
// RunnerEnv.SubagentTimeout. A single named constant, rather than the literal
// 30*time.Minute repeated at three sites, so the three cannot silently drift
// apart under a future edit to one of them.
const DefaultTimeout = 30 * time.Minute

// DelegateOwnerReference builds the controller owner reference a
// SubagentRequest's creator must stamp on it, naming the delegating (parent)
// AgentSession.
//
// Without it, deleting the parent mid-delegation leaves the SubagentRequest
// -- and, transitively, any child AgentSession it already spawned -- orphaned
// rather than reaped. The child's own owner reference points at the REQUEST,
// not the parent, and pkg/controllers/subagentrequest's Reconcile short-
// circuits at the top once status.childRef is set, so nothing ever revisits
// the parent's existence again. A running child whose spec.parent names a
// session that no longer exists resolves standing to NOBODY under the
// lineage model: unclaimed, unapprovable, unstoppable by any accountable
// human -- the same unclaimed-session shape the compensating delete on a
// failed lineage write already guards against elsewhere in this controller.
// Kubernetes' built-in cascading GC on this owner reference is what closes
// that gap: deleting the parent reaps the request, and transitively the
// child.
//
// Exported (rather than inlined at each wiring site) so both
// internal/cmd/runner/main.go and the e2e in-process factory build it
// identically, and so the mapping from parent to owner reference is
// unit-testable without envtest.
func DelegateOwnerReference(parent *v1.AgentSession) metav1.OwnerReference {
	return *metav1.NewControllerRef(parent, v1.SchemeGroupVersion.WithKind("AgentSession"))
}

// DelegateConfig wires the delegate tool. Create and Poll are funcs rather
// than a client -- like request_credential_update's Client field, but one
// level further removed, since delegate's round trip needs no other operator
// interaction (no equivalent of credential_update's findOpenRequest List) --
// so the tool is unit-testable without envtest.
type DelegateConfig struct {
	// Namespace and SessionName identify the delegating (parent) session;
	// stamped onto spec.parent on every SubagentRequest this tool creates.
	Namespace   string
	SessionName string

	// Create writes a new SubagentRequest CR. Required; a nil Create refuses
	// every call rather than panicking.
	Create func(ctx context.Context, sr *v1.SubagentRequest) error
	// Poll reads back the named SubagentRequest CR by name.
	Poll func(ctx context.Context, name string) (*v1.SubagentRequest, error)

	// Send delivers one message from this session to a child session, on the
	// agent-to-agent inbound path (a channelevents.KindAgentMessageSend
	// envelope on THIS session's own inbound bus subject naming the child in
	// the payload, corroborated by channelsd against the Channel that joins the
	// pair and authorized there on agentsession#converse). It backs
	// reply_to_subagent; a nil Send means this session cannot answer a child —
	// there is no bus, so it is a kubectl-driven parent. The subagents
	// capability offers reply_to_subagent anyway and lets Execute refuse the
	// call by name, which puts the reason in front of the model in the turn it
	// asked, rather than leaving a parent to infer a missing tool.
	Send func(ctx context.Context, childNS, childName, text string) error

	// ResolveDataTag maps a tool_use_id from THIS session to the pt-tag minted
	// for that call, so a parent can fill a child's data slot without ever
	// handling a tag id.
	//
	// Server-side on purpose. The tag is the authorization object — its reader
	// set is what the controller attenuates against — so letting a model name
	// one directly would hand it the vocabulary to ask for tags it never
	// produced. Resolving from a tool_use_id bounds the request to this
	// session's own calls by construction.
	//
	// Returns an empty id (no error) when the call minted no tag: a call whose
	// tool declared no read has no provenance to hand over, which is a
	// legitimate answer and a refusal, not a failure.
	//
	// A nil ResolveDataTag means this session cannot hand over data at all.
	// Execute then REFUSES a call that asked to, rather than dropping the
	// slots and delegating anyway — a child waiting on data that will never
	// arrive is worse than one told it did not get it, which is the same
	// reasoning DataSlots' own CRD contract gives.
	ResolveDataTag func(ctx context.Context, toolUseID string) (tagID string, err error)

	// PollInterval paces Poll calls while the request is non-terminal.
	PollInterval time.Duration
	// Timeout bounds ONE call's wait -- delegate's wait for the first
	// question or the final answer, and each reply_to_subagent's wait for the
	// next one. It is deliberately per-call rather than a budget shared by a
	// whole conversation: a conversation whose every exchange is answered
	// promptly is working exactly as intended, and a shared budget would kill
	// it for lasting a while. What bounds the OTHER side -- a child parked on
	// a parent that never answers -- is the SubagentRequest controller's own
	// per-exchange timeout, because only the operator outlives both parties.
	//
	// A zero value falls back to a generous internal default rather than
	// polling forever -- the operator's SubagentRequest controller is a
	// separate process and a stalled reconcile must not hang this tool call
	// indefinitely.
	Timeout time.Duration
}

// NewDelegateTool constructs the delegate meta tool.
func NewDelegateTool(cfg DelegateConfig) tool.Tool {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Timeout <= 0 {
		// Guards a zero value reaching Execute directly (e.g. a test, or a
		// caller that forgot to size it) against polling forever. The
		// subagents capability normally supplies RunnerEnv.SubagentTimeout
		// explicitly.
		cfg.Timeout = DefaultTimeout
	}
	return &delegateTool{cfg: cfg}
}

type delegateTool struct {
	cfg DelegateConfig
}

func (*delegateTool) Name() string    { return "delegate" }
func (*delegateTool) Kind() tool.Kind { return tool.KindMeta }
func (*delegateTool) Permission() authz.Permission {
	// Stateless for DISPATCH: delegate reaches no SpiceDB resource of its own,
	// and the delegation decision (roster membership, graph validity, identity
	// monotonicity, budget) is re-verified operator-side by the SubagentRequest
	// controller. A Readonly/Readwrite here would be denied fail-closed by the
	// tool checker ("stateImpact requires a check but none was supplied"), and
	// External would force per-call human approval in EVERY mode -- gating
	// delegation even where no plan gate runs. Neither is wanted.
	//
	// Plan-gate governance is separate from dispatch: see PlanGateGoverned
	// below. Spawning a child IS a consequential action the plan gate must see
	// (without it a parent under injection escapes its own plan by delegating a
	// write it cannot itself perform), but that governance belongs to the plan
	// gate alone, only under `enforcing` mode -- not to the dispatch checker.
	return authz.Permission{StateImpact: authz.Stateless}
}

// PlanGateGoverned marks delegate as an action the plan gate governs even
// though its dispatch permission is Stateless. The plan-gate handle resolver
// hands the gate a `tool:delegate` handle for it, so under `enforcing` mode a
// spawn the plan did not pre-authorize raises a plan_amendment ("the human
// approves the run itself"); under disabled/logging the gate does not fire and
// delegation is unchanged. Dispatch stays Stateless -- no per-call SpiceDB
// check, no per-call approval, zero blast radius on classes that run no gate.
func (*delegateTool) PlanGateGoverned() bool                        { return true }
func (*delegateTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*delegateTool) Description() string {
	return "Delegate a task to another agent from your subagents roster, and block until it either finishes or comes " +
		"back with a question. The other agent runs as a fresh, independent session: it starts with ONLY the `task` " +
		"text you give it -- no conversation history, no tool results, no open files, nothing else you know. Write " +
		"`task` as a complete, self-contained instruction. If it answers with a QUESTION, this call returns that " +
		"question and the delegation handle to answer it with; reply using reply_to_subagent, which returns the next " +
		"question or the final result the same way. A DENIAL (the class is not on your roster, or the delegation was " +
		"otherwise refused before any child ran) is final -- do not retry the same task through another agent or " +
		"resplit it; report the refusal instead. A FAILURE (the child ran but did not complete successfully) may be " +
		"retried."
}

func (*delegateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"agent": {
				"type": "string",
				"description": "The agent to delegate to. Must be one this agent is allowed to use (its subagents roster); asking for another is refused."
			},
			"task": {
				"type": "string",
				"description": "A complete, self-contained instruction. The other agent sees NOTHING of this conversation -- no history, no tool results, no files you have open. State everything it needs."
			},
			"mode": {
				"type": "string",
				"enum": ["single_turn", "task", "chat"],
				"description": "How much surface the other agent gets. Omit for single_turn, which is the default and the right answer unless you genuinely need to talk to it. single_turn: it gets your instruction, does one bounded piece of work, and hands back an answer -- you cannot reach it while it runs. task: it may come back to you with ONE bounded clarifying question and then continue. chat: a full back-and-forth with you. Which modes you may use is set per-agent by your roster and defaults to single_turn only; asking for one you do not have is refused outright and names what you are allowed, so do not guess widely."
			},
			"inputs": {
				"type": "object",
				"description": "Data to hand over, BY REFERENCE, filling the other agent's declared input slots. The key is the slot name it declared (\"diff\", \"logs\"); the value names the tool call whose result holds that data. Use this instead of pasting the data into the task text: pasted text is your words and carries none of the original's permissions, so the other agent may be refused access to it or may act on something it was never authorized to see. You may only hand over results from calls YOU made in this conversation, and only the other agent's declared slots can be filled.",
				"additionalProperties": {
					"type": "object",
					"additionalProperties": false,
					"properties": {
						"tool_use_id": {
							"type": "string",
							"description": "The id of the tool call in this conversation whose result holds this data."
						}
					},
					"required": ["tool_use_id"]
				}
			}
		},
		"required": ["agent", "task"]
	}`)
}

// delegateInput names ONE piece of data the parent is handing over, by the
// tool call that produced it.
//
// By the CALL, not by a tag id, and that is the security shape rather than a
// convenience. Tag ids are minted server-side and never surfaced to a model;
// keeping them out means a parent can only hand over data it actually
// produced in this session, because a tool_use_id it did not make has no tag
// to resolve to. There is also no field here for content — a slot holds a
// reference, so a parent structurally cannot launder text into one.
type delegateInput struct {
	ToolUseID string `json:"tool_use_id"`
}

type delegateArgs struct {
	Agent  string                   `json:"agent"`
	Task   string                   `json:"task"`
	Mode   string                   `json:"mode"`
	Inputs map[string]delegateInput `json:"inputs"`
}

// resolveInputs turns the model's slot→tool_use_id map into the DataSlots the
// SubagentRequest carries, refusing rather than dropping anything it cannot
// resolve.
//
// EVERY failure here is a refusal of the whole delegation, never a partial
// handover. A child spawned with three of the four slots it was promised looks
// to itself like a child whose parent chose to withhold one, and it will
// proceed on that reading — so a silently dropped slot does not degrade the
// delegation, it changes what the child believes it was told. The CRD's own
// DataSlots contract makes the same argument for the controller's half.
//
// The returned Result is meaningful only when ok is false.
func (t *delegateTool) resolveInputs(ctx context.Context, inputs map[string]delegateInput) ([]v1.DataSlotRequest, tool.Result, bool) {
	if len(inputs) == 0 {
		return nil, tool.Result{}, true
	}
	refuse := func(format string, a ...any) ([]v1.DataSlotRequest, tool.Result, bool) {
		return nil, tool.Result{
			Content: t.Name() + ": " + fmt.Sprintf(format, a...),
			IsError: true, Trusted: true,
		}, false
	}
	if t.cfg.ResolveDataTag == nil {
		return refuse("this session cannot hand over data, so the delegation was not sent. Put what the other agent needs into the task text instead, or delegate without inputs")
	}

	slots := make([]v1.DataSlotRequest, 0, len(inputs))
	for _, slot := range slices.Sorted(maps.Keys(inputs)) {
		in := inputs[slot]
		if strings.TrimSpace(slot) == "" {
			return refuse("an input was given with no slot name; name the slot the other agent declared")
		}
		id := strings.TrimSpace(in.ToolUseID)
		if id == "" {
			return refuse("input %q names no tool call; give the tool_use_id of the call whose result holds that data", slot)
		}
		tagID, err := t.cfg.ResolveDataTag(ctx, id)
		if err != nil {
			return refuse("could not look up the data for input %q: %v", slot, err)
		}
		if tagID == "" {
			return refuse("the call %q produced no data that can be handed over — only results from tools that read a tracked resource can fill a slot. Summarize it in the task text instead", id)
		}
		slots = append(slots, v1.DataSlotRequest{Slot: slot, TagID: tagID})
	}
	return slots, tool.Result{}, true
}

func (t *delegateTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args delegateArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"agent": "demo-coder", "task": "review the open PRs and summarize them"}`); !ok {
		return res, nil
	}
	if strings.TrimSpace(args.Agent) == "" {
		return tool.Result{Content: fmt.Sprintf("%s: agent must not be empty", t.Name()), IsError: true, Trusted: true}, nil
	}
	if strings.TrimSpace(args.Task) == "" {
		return tool.Result{Content: fmt.Sprintf("%s: task must not be empty", t.Name()), IsError: true, Trusted: true}, nil
	}
	if len([]rune(args.Task)) > maxTaskRunes {
		return tool.Result{
			Content: fmt.Sprintf("%s: task is too long (%d runes, max %d) -- shorten it rather than relying on truncation, which would silently change what the child is asked to do",
				t.Name(), len([]rune(args.Task)), maxTaskRunes),
			IsError: true, Trusted: true,
		}, nil
	}
	// A shape check on the argument, NOT the gate. Whether this session may
	// delegate in the mode it names is the parent roster's answer and the
	// SubagentRequest controller's to give -- this runs in the runner, the very
	// party the delegation constrains, so it can only ever be advisory. What it
	// does buy is that a misspelling never becomes a Create the apiserver's
	// spec.mode Enum rejects with a schema error the model cannot act on, and
	// it is refused rather than dropped: silently blanking an unrecognized mode
	// would run a single_turn delegation the agent did not ask for.
	//
	// Trimmed once, here, and carried to the Create below: checking one string
	// and sending another is how a value that passed validation stops being the
	// value that ships.
	mode := strings.TrimSpace(args.Mode)
	if mode != "" && !v1.IsSubagentMode(mode) {
		return tool.Result{
			Content: fmt.Sprintf("%s: %q is not a delegation mode; use one of %v, or omit it for %s",
				t.Name(), args.Mode, v1.SubagentModesAll(), v1.SubagentModeSingleTurn),
			IsError: true, Trusted: true,
		}, nil
	}
	if t.cfg.Create == nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: not available in this session (no subagent request client wired)", t.Name()),
			IsError: true, Trusted: true,
		}, nil
	}

	slots, res, ok := t.resolveInputs(ctx, args.Inputs)
	if !ok {
		return res, nil
	}

	sr := &v1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{
			// The apiserver names it, same reasoning as credential_update's
			// GenerateName use: a name picked here can collide, and the agent has
			// no way to retry differently against a name it never chose.
			GenerateName: fmt.Sprintf("subreq-%s-", t.cfg.SessionName),
			Namespace:    t.cfg.Namespace,
		},
		Spec: v1.SubagentRequestSpec{
			Parent: v1.NamespacedRef{Namespace: t.cfg.Namespace, Name: t.cfg.SessionName},
			Class:  args.Agent,
			Task:   args.Task,
			// Passed through as the agent wrote it, including the empty string,
			// which spec.mode's own contract resolves to single_turn. Nothing
			// here substitutes a mode: what the agent asked for is what the
			// controller gets to authorize, so a denial names the mode the
			// agent actually wrote.
			Mode: mode,
			// A REQUEST, exactly like Mode. Resolving a tag here says only
			// "this session produced that datum"; whether the parent may
			// DELEGATE it is `pt_tag:<T>#access@agentsession:<parent>`, which
			// the controller checks. This runs in the runner, the party the
			// delegation constrains, so it can never be the gate.
			DataSlots: slots,
		},
	}
	if err := t.cfg.Create(ctx, sr); err != nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: creating the delegation request: %v", t.Name(), err),
			IsError: true, Trusted: true,
		}, nil
	}

	// afterExchange 0: nothing has been asked yet, so ANY question the child
	// raises is new to this caller.
	return t.cfg.waitForNext(ctx, t.Name(), sr.Name, 0)
}

// waitForNext waits for the named SubagentRequest to reach either a terminal
// phase or a question the caller has not already answered, and returns the
// tool result for whichever it reaches. cfg.Timeout or ctx.Done() bounds the
// wait; both are reported as retryable.
//
// afterExchange is the highest exchange number the caller has already
// answered — 0 from delegate, which has answered nothing. An AwaitingParent
// request at or below it is the caller's OWN outstanding question, observed
// before the child has woken to read the reply; treating that as a new
// question would hand the model the same words it just answered and loop it
// against itself. Waiting past it is what makes the exchange counter
// load-bearing rather than decorative.
//
// Every one of SubagentRequestStatus's six phases (Pending, Running,
// AwaitingParent, Succeeded, Failed, Denied) is handled explicitly below --
// deliberately NOT via (*v1.SubagentRequest).IsTerminal(), which reads any
// phase it does not recognize as non-terminal. Relying on it here would mean a
// phase value this tool doesn't know about (a future addition, a typo in a
// status write) makes the loop wait forever; instead an unrecognized phase
// falls into the same "keep polling" branch as Pending/Running, and it is
// cfg.Timeout -- not IsTerminal -- that ultimately bounds every one of those
// branches.
//
// toolName is the CALLING tool's name (delegate or reply_to_subagent), so the
// error text a model reads names the call it actually made.
func (cfg DelegateConfig) waitForNext(ctx context.Context, toolName, name string, afterExchange int64) (tool.Result, error) {
	deadline := time.Now().Add(cfg.Timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tool.Result{Content: fmt.Sprintf("%s: %v", toolName, err), IsError: true, Trusted: true}, nil
		}
		sr, err := cfg.Poll(ctx, name)
		if err != nil {
			slog.Info("delegation: polling the subagent request failed; retrying until timeout",
				"tool", toolName, "namespace", cfg.Namespace, "name", name, "err", err.Error())
			time.Sleep(cfg.PollInterval)
			continue
		}
		switch sr.Status.Phase {
		case v1.SubagentRequestPhaseSucceeded:
			// The child's own answer -- NOT platform-authored -- so it is left
			// Trusted: false (the zero value) and runs through the same
			// content-guard inspection any other untrusted meta-tool result gets.
			//
			// Any artifact handles the child returned ride the same result, and
			// therefore the same inspection. A handle is not a licence: it names
			// bytes this agent may choose to deliver, and respond_to_user
			// re-verifies every one against this delegation's own status before
			// attaching anything.
			return tool.Result{Content: sr.Status.Result + describeReturnedArtifacts(name, sr.Status.Artifacts)}, nil
		case v1.SubagentRequestPhaseAwaitingParent:
			if sr.Status.Exchange <= afterExchange {
				break // already answered; keep waiting for the child to move on
			}
			// The child's own words again, and left untrusted for exactly the
			// same reason the Succeeded arm is: this is the message a delegated
			// agent composed, and it must reach this model through the same
			// content-guard inspection its final answer does. The framing
			// sentence is inside the untrusted content deliberately -- widening
			// what gets inspected is harmless, while narrowing it is the
			// laundering path the whole mode design exists to bound.
			return tool.Result{Content: fmt.Sprintf(
				"The agent you delegated to has a question and is waiting. Answer it with reply_to_subagent(delegation=%q), or leave it unanswered and the delegation ends. Its question follows.\n\n%s",
				name, sr.Status.Message)}, nil
		case v1.SubagentRequestPhaseDenied:
			// A denial BINDS: the parent must not re-attempt the same work through
			// another child. Saying so explicitly is what stops the model treating
			// a refusal as a transient obstacle to route around -- denial-laundering
			// is exactly the attack this tool's split between Denied and Failed
			// exists to prevent.
			return tool.Result{
				IsError: true, Trusted: true,
				Content: fmt.Sprintf("Delegation denied (%s): %s. Do not retry this task through another agent; report the refusal instead.",
					sr.Status.FailureReason, sr.Status.Determination),
			}, nil
		case v1.SubagentRequestPhaseFailed:
			return tool.Result{
				IsError: true, Trusted: true,
				Content: fmt.Sprintf("Delegation failed (%s): %s. This may be retried.",
					sr.Status.FailureReason, sr.Status.Determination),
			}, nil
		case v1.SubagentRequestPhasePending, v1.SubagentRequestPhaseRunning, "":
			// Not yet terminal; poll again below. "" (the Phase field's zero
			// value, not one of the six named phases) is the
			// pre-first-reconcile state -- the controller never writes
			// Pending, so a fresh request goes ""->Running/Denied, and the
			// very first poll (issued immediately after Create) almost
			// always observes it. Without this case it fell into default
			// below and logged as an unrecognized phase on essentially every
			// delegation. default must stay a real guard against a
			// genuinely unrecognized (future/mistyped) phase value.
		default:
			// Unrecognized phase -- see the doc comment above. Logged so a
			// genuinely new/mistyped phase value is visible rather than silently
			// spinning until timeout.
			slog.Info("delegation: unrecognized SubagentRequest phase; continuing to poll until timeout",
				"tool", toolName, "namespace", cfg.Namespace, "name", name, "phase", sr.Status.Phase)
		}
		time.Sleep(cfg.PollInterval)
	}
	return tool.Result{
		Content: fmt.Sprintf("%s: timed out waiting for %q; it may still be running and complete later. This may be retried.",
			toolName, name),
		IsError: true, Trusted: true,
	}, nil
}
