package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// seqFromCtx derives the logical Seq and session UID for a status publish from
// the per-tool-call IDs context the runner sets before dispatch. Returns (0, "")
// when absent — kubectl-driven sessions and tests — so those envelopes carry a
// zero sequence rather than a fabricated one.
func seqFromCtx(ctx context.Context) (uint64, string) {
	ids, ok := sandbox.IDsFromCtx(ctx)
	if !ok {
		return 0, ""
	}
	return channelevents.PackSeq(ids.MemTurnIndex, ids.BlockIndex), ids.SessionUID
}

// UpdatePlanConfig wires update_plan to the runner's NATS publisher.
// AppendSystemNote is NOT a field here — persistence happens inside
// plans.Store via the state.Deps it received at session bootstrap.
//
// NATSPublish may be nil (kubectl-driven sessions, tests). When nil,
// the registry is updated and persistence still happens, but no
// envelope is emitted to channels.
type UpdatePlanConfig struct {
	NATSPublish func(ctx context.Context, subject string, payload []byte) error

	// EnvelopeSigner signs every envelope update_plan publishes with the
	// session's identity key. Nil-safe: a nil signer leaves the envelope
	// unsigned (test fixtures without a signer still work).
	EnvelopeSigner *channelevents.EnvelopeSigner

	// OnPhasesDeclared fires after a plan carrying phases is stored.
	//
	// This is the moment the working plan becomes a candidate for authority:
	// the runtime freezes the ordered list, prices it, and records it. The
	// freeze has to happen HERE, once, at a recorded moment — deriving it
	// lazily from the store on each call would mean the "frozen" plan silently
	// tracked the agent's document, which is the whole thing the design
	// prevents.
	//
	// Nil for sessions with no plan gate. An error is surfaced to the agent:
	// a plan whose ceiling was not recorded is not one the gate can honour.
	// Returns human-readable notices for anything the freeze DROPPED, so the
	// agent hears about it on this call's result. A dropped handle silently
	// narrows the phase — possibly to an empty ceiling — and the agent is the
	// only party that can fix the plan.
	OnPhasesDeclared func(ctx context.Context, phases []plans.Phase) ([]string, error)
}

// NewUpdatePlan constructs the update_plan meta tool.
func NewUpdatePlan(cfg UpdatePlanConfig) tool.Tool {
	return &updatePlanTool{cfg: cfg}
}

type updatePlanTool struct{ cfg UpdatePlanConfig }

func (*updatePlanTool) Name() string    { return "update_plan" }
func (*updatePlanTool) Kind() tool.Kind { return tool.KindMeta }
func (*updatePlanTool) Permission() authz.Permission {
	// update_plan mutates in-process session state (plans registry) and
	// publishes a plan-update envelope to the channel. Both targets are
	// session-scoped and already gated by AgentSession#interact; treated
	// as Passthrough until slice 2 provenance.
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*updatePlanTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*updatePlanTool) Description() string {
	return "Declare or update a multi-step plan. Pass the FULL current state of the plan on every call — added, " +
		"changed, and removed items are diffed by id. Use a stable, slug-like id per item; the diff is keyed by id, " +
		"so renaming a label keeps the same id. At most one item may have status \"in_progress\" per plan. " +
		"Transitioning an item from pending→in_progress causes the runtime to auto-open an operation; the returned " +
		"operation_id MUST be passed on every sandbox tool call performing that item's work. Marking an item " +
		"\"in_progress\" also sets the user-facing status caption to that item's label (it persists until the next " +
		"status or plan change), so a plan transition doubles as an update_status — no need to repeat the same line. Both \"done\" and " +
		"\"error\" are terminal: don't transition out of them — add a new item with a new id if you need to retry. " +
		"Use the optional `details` field to give the user a one-line why for the step. Use the optional `output` " +
		"field to surface the concrete result once status is `done` or `error`. Pass items=[] to delete a plan " +
		"entirely. Optional parent_plan/parent_item soft-anchor this plan as a sub-plan of an item in another plan " +
		"(rendered hierarchically by channels that support it; otherwise rendered as a peer). " +
		"\n\nOptionally declare `phases`: the stages of the work, each naming the permissions that stage needs. " +
		"Declare the narrowest set each phase actually requires — and a COMPLETE one: every permission its own " +
		"steps will need. Narrow is about SCOPE, not about discovering permissions as you go. Over-asking costs " +
		"the user a careful decision; under-asking raises an amendment mid-run and interrupts them again. " +
		"Every permission needs a `why`; it is shown to the approver verbatim as your justification. " +
		"A phase runs ONCE by default: set `max` only when a step genuinely repeats, and say why in `max.why`. " +
		"Use `requires` when a phase must not start before another has run. Set each item's `phase` to group it " +
		"under the stage it belongs to. For an existing active goal, attach `reminders` to its phase " +
		"using the request_goal_execution argument shape (resource, id, revision, requestID, terms). " +
		"The plan approval authorizes those exact bounded reminders; do not also call request_goal_execution. " +
		"Use stable requestIDs on repeated full-plan updates. Each future session still requires its own fresh action plan."
}

func (*updatePlanTool) InputSchema() json.RawMessage {
	schema := json.RawMessage(`{
		"type":"object",
		"additionalProperties":false,
		"properties":{
			"name":{"type":"string","minLength":1,"maxLength":64},
			"items":{
				"type":"array",
				"items":{
					"type":"object",
					"additionalProperties":false,
					"properties":{
						"id":{"type":"string","minLength":1,"maxLength":64},
						"label":{"type":"string","minLength":1,"maxLength":200},
						"status":{"enum":["pending","in_progress","done","error"]},
						"details":{"type":"string","maxLength":1000},
						"output":{"type":"string","maxLength":2000}
					},
					"required":["id","label","status"]
				}
			},
			"phases":{
				"type":"array",
				"items":{
					"type":"object",
					"additionalProperties":false,
					"properties":{
						"id":{"type":"string","minLength":1,"maxLength":64},
						"label":{"type":"string","minLength":1,"maxLength":200},
						"why":{"type":"string","minLength":1,"maxLength":300},
						"requires":{
							"type":"array",
							"items":{
								"type":"object",
								"additionalProperties":false,
								"properties":{
									"phase":{"type":"string","minLength":1,"maxLength":64},
									"why":{"type":"string","minLength":1,"maxLength":300}
								},
								"required":["phase","why"]
							}
						},
						"max":{
							"type":"object",
							"additionalProperties":false,
							"properties":{
								"count":{"type":"integer","minimum":1},
								"why":{"type":"string","minLength":1,"maxLength":300}
							},
							"required":["count","why"]
						},
						"budget":{
							"type":"object",
							"additionalProperties":false,
							"properties":{
								"calls":{"type":"integer","minimum":1},
								"why":{"type":"string","minLength":1,"maxLength":300}
							},
							"required":["calls","why"]
						},
						"permissions":{
							"type":"array",
							"items":{
								"type":"object",
								"additionalProperties":false,
								"properties":{
									"handle":{"type":"string","maxLength":200,"pattern":"^(perm:[a-z][a-z0-9_]*:[a-z][a-z0-9_/]*|tool:[a-zA-Z0-9_-]{1,64})$","description":"A permission handle from this session's permission surface — NOT a tool name. Either \"perm:<permission>:<resourceType>\" (e.g. perm:read:github_repo) or \"tool:<toolName>\" (e.g. tool:apply_workspace). A handle that is not on the surface is dropped, narrowing this phase; the result says which and lists what is declarable. Tools that are stateless or passthrough carry no authorization and have no handle."},
									"why":{"type":"string","minLength":1,"maxLength":300,"description":"Why this phase needs this capability. A person reads it on the approval card, quoted and attributed to you. It is your claim, not a system statement — the card describes what is actually being granted separately, so justify the need rather than restating the permission."}
								},
								"required":["handle","why"]
							}
						},
						"slots":{
							"type":"array",
							"items":{
								"type":"object",
								"additionalProperties":false,
								"properties":{
									"type":{"type":"string","minLength":1,"maxLength":128},
									"id":{"type":"string","maxLength":512,"description":"The SPECIFIC resource this phase will act on, when you already know it \u2014 the repository, the issue, the record. Supply it whenever you can: naming it lets the user approve this phase now, and the work then runs without interrupting them again. Omit it only when you genuinely cannot know the target until you have looked; the phase is then marked as needing a second approval later."},
									"why":{"type":"string","minLength":1,"maxLength":300,"description":"Why this phase needs THIS resource. A person reads it on the approval card, quoted and attributed to you, next to the resource itself \u2014 so write the sentence that answers \"why does it want that one?\". It is your claim, not a system statement: it cannot widen what you are asking for, and what is being granted is described separately."}
								},
								"required":["type","why"]
							}
						}
					},
					"required":["id","label","why"]
				}
			},
			"parent_plan":{"type":"string","maxLength":64},
			"parent_item":{"type":"string","maxLength":64}
		},
		"required":["name","items"]
	}`)
	var parsed map[string]any
	if err := json.Unmarshal(schema, &parsed); err != nil {
		panic(err)
	}
	var reminder map[string]any
	if err := json.Unmarshal((&goalTool{op: "request_execution"}).InputSchema(), &reminder); err != nil {
		panic(err)
	}
	props := parsed["properties"].(map[string]any)
	phaseProps := props["phases"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	phaseProps["reminders"] = map[string]any{"type": "array", "maxItems": 8, "items": reminder}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(parsed); err != nil {
		panic(err)
	}
	return encoded.Bytes()
}

// Plan-shape limits. Deliberately expansive: they exist to stop a runaway or
// hostile plan from DoS-ing an approver and producing an unrenderable card,
// NOT to shape normal authoring. Exceeding one is a tool error naming the
// limit so the agent can re-plan smaller — never a silent truncation, which
// would drop declared reach while leaving the plan looking complete.
const (
	defaultMaxPhases        = 50
	defaultMaxPermsPerPhase = 64
	defaultMaxSlotsPerPhase = 128
)

type updatePlanArgs struct {
	Name       string               `json:"name"`
	Items      []updatePlanArgItem  `json:"items"`
	Phases     []updatePlanArgPhase `json:"phases,omitempty"`
	ParentPlan string               `json:"parent_plan,omitempty"`
	ParentItem string               `json:"parent_item,omitempty"`
}

type updatePlanArgItem struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
	Output  string `json:"output,omitempty"`
	Phase   string `json:"phase,omitempty"`
}

type updatePlanArgPhase struct {
	ID          string                  `json:"id"`
	Label       string                  `json:"label"`
	Why         string                  `json:"why"`
	Requires    []updatePlanArgRequires `json:"requires,omitempty"`
	Max         *updatePlanArgMax       `json:"max,omitempty"`
	Budget      *updatePlanArgBudget    `json:"budget,omitempty"`
	Permissions []updatePlanArgPerm     `json:"permissions,omitempty"`
	Slots       []updatePlanArgSlot     `json:"slots,omitempty"`
	Reminders   []plans.ReminderRequest `json:"reminders,omitempty"`
}

type updatePlanArgRequires struct {
	Phase string `json:"phase"`
	Why   string `json:"why"`
}

type updatePlanArgMax struct {
	Count int    `json:"count"`
	Why   string `json:"why"`
}

// updatePlanArgBudget is the phase's governed-call budget as the agent wrote it.
type updatePlanArgBudget struct {
	Calls int    `json:"calls"`
	Why   string `json:"why"`
}

type updatePlanArgPerm struct {
	Handle string `json:"handle"`
	Why    string `json:"why"`
}

type updatePlanArgSlot struct {
	Type string `json:"type"`
	// ID is the concrete resource, when the agent can name one. Optional by
	// design: forcing it would make the agent invent a target rather than admit
	// it does not know one yet.
	ID  string `json:"id,omitempty"`
	Why string `json:"why"`
}

// validatePhases enforces what the JSON schema cannot: cross-phase invariants.
//
// The schema catches shape (required fields, lengths, unknown properties);
// these are the rules that need the whole list in view. Each returns an error
// naming what is wrong, because the agent is the one who has to fix it.
func validatePhases(phases []updatePlanArgPhase) error {
	if len(phases) > defaultMaxPhases {
		return fmt.Errorf("plan declares %d phases, over the maxPhases limit of %d; re-plan with fewer, broader phases",
			len(phases), defaultMaxPhases)
	}

	seen := make(map[string]struct{}, len(phases))
	for _, p := range phases {
		if p.ID == "" {
			return errors.New("every phase needs a non-empty id")
		}
		if p.Why == "" {
			return fmt.Errorf("phase %q needs a why: it is shown to the approver as your justification", p.ID)
		}
		if _, dup := seen[p.ID]; dup {
			// Requires edges name phases by id, so a duplicate would make an
			// edge ambiguous — and freezing resolves ids to indices exactly
			// once, so the ambiguity would silently pick one.
			return fmt.Errorf("duplicate phase id %q; ids must be unique because requires edges name them", p.ID)
		}
		seen[p.ID] = struct{}{}

		if len(p.Permissions) > defaultMaxPermsPerPhase {
			return fmt.Errorf("phase %q declares %d permissions, over the maxPermissionsPerPhase limit of %d; re-plan with fewer",
				p.ID, len(p.Permissions), defaultMaxPermsPerPhase)
		}
		if len(p.Reminders) > 8 {
			return fmt.Errorf("phase %q has too many reminders (maximum 8)", p.ID)
		}
		if len(p.Slots) > defaultMaxSlotsPerPhase {
			return fmt.Errorf("phase %q declares %d slots, over the maxSlotsPerPhase limit of %d; re-plan with fewer",
				p.ID, len(p.Slots), defaultMaxSlotsPerPhase)
		}
		for _, perm := range p.Permissions {
			if perm.Why == "" {
				return fmt.Errorf("phase %q declares permission %q with no why; every permission needs a justification, because that is what the approver reads",
					p.ID, perm.Handle)
			}
		}
		if p.Max != nil {
			if p.Max.Count < 1 {
				return fmt.Errorf("phase %q has max.count %d; a phase that can never be entered is not a plan step", p.ID, p.Max.Count)
			}
			if p.Max.Why == "" {
				return fmt.Errorf("phase %q raises max.count above the default of 1 and needs a why for it", p.ID)
			}
		}
	}

	// Resolved after the id set is complete, so forward references are legal:
	// a phase may require one declared later in the list.
	for _, p := range phases {
		for _, r := range p.Requires {
			if r.Why == "" {
				return fmt.Errorf("phase %q requires %q with no why", p.ID, r.Phase)
			}
			if _, ok := seen[r.Phase]; !ok {
				return fmt.Errorf("phase %q requires %q, which is not a declared phase", p.ID, r.Phase)
			}
		}
	}
	return nil
}

func toPlanPhases(in []updatePlanArgPhase) []plans.Phase {
	if len(in) == 0 {
		return nil
	}
	out := make([]plans.Phase, 0, len(in))
	for _, p := range in {
		ph := plans.Phase{ID: p.ID, Label: p.Label, Why: p.Why, Reminders: p.Reminders}
		for _, r := range p.Requires {
			ph.Requires = append(ph.Requires, plans.PhaseRequirement{Phase: r.Phase, Why: r.Why})
		}
		if p.Max != nil {
			ph.Max = &plans.PhaseMax{Count: p.Max.Count, Why: p.Max.Why}
		}
		if p.Budget != nil {
			ph.Budget = &plans.PhaseBudget{Calls: p.Budget.Calls, Why: p.Budget.Why}
		}
		for _, perm := range p.Permissions {
			ph.Permissions = append(ph.Permissions, plans.PhasePermission{Handle: perm.Handle, Why: perm.Why})
		}
		for _, s := range p.Slots {
			ph.Slots = append(ph.Slots, plans.PhaseSlot{Type: s.Type, ID: s.ID, Why: s.Why})
		}
		out = append(out, ph)
	}
	return out
}

func (t *updatePlanTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var a updatePlanArgs
	if res, ok := tool.ParseArgs(raw, &a, t.Name(), `{"name":"main","items":[{"id":"step1","label":"...","status":"pending"}]}`); !ok {
		return res, nil
	}
	if sess == nil || sess.State == nil {
		return tool.Result{Trusted: true}, errors.New("update_plan: SessionContext.State is nil")
	}

	// Cross-phase invariants the JSON schema cannot express (uniqueness,
	// requires resolvability, the size limits). Rejected as a tool error the
	// agent can act on, never as a Go error.
	if err := validatePhases(a.Phases); err != nil {
		return tool.Result{Content: "update_plan: " + err.Error(), IsError: true, Trusted: true}, nil
	}

	items := make([]plans.Item, 0, len(a.Items))
	for _, it := range a.Items {
		items = append(items, plans.Item{
			ID:      it.ID,
			Label:   it.Label,
			Status:  plans.Status(it.Status),
			Details: it.Details,
			Output:  it.Output,
			Phase:   it.Phase,
		})
	}

	for _, phase := range a.Phases {
		if len(phase.Reminders) > 0 && t.cfg.OnPhasesDeclared == nil {
			return tool.Result{Content: "update_plan: reminders require an enforcing plan approval gate", IsError: true, Trusted: true}, nil
		}
	}
	store := plans.From(sess)
	res, err := store.Update(ctx, a.Name, plans.ParentRef{Plan: a.ParentPlan, Item: a.ParentItem},
		plans.Content{Items: items, Phases: toPlanPhases(a.Phases)})
	if err != nil {
		return tool.Result{Content: "update_plan: " + err.Error(), IsError: true, Trusted: true}, nil
	}

	// Freeze and record the declared phases. Unlike the channel publish below
	// this is NOT best-effort: a plan whose ceiling was never recorded is not
	// one the gate can honour, so telling the agent the update succeeded would
	// leave it planning against authority the log does not know about.
	var phaseNotices []string
	if len(a.Phases) > 0 && t.cfg.OnPhasesDeclared != nil {
		notices, err := t.cfg.OnPhasesDeclared(ctx, toPlanPhases(a.Phases))
		if err != nil {
			return tool.Result{
				Content: "update_plan: the plan was stored but its phases could not be recorded, " +
					"so they do not yet govern anything: " + err.Error(),
				IsError: true, Trusted: true,
			}, nil
		}
		phaseNotices = notices
	}

	// Publish a snapshot envelope (best-effort). Read the
	// post-update plan to capture operation_ids assigned by Update.
	if t.cfg.NATSPublish != nil {
		// Stamp the logical Seq + session UID derived from this tool call's
		// IDs context onto both outbound envelopes so the channelsd status
		// state machine can order them and drop stale captions.
		seq, uid := seqFromCtx(ctx)
		stored, ok := store.Get(a.Name)
		if !ok {
			// Deletion path: Get returns empty; preserve PlanName + parent refs
			// so the channel renders the "cancelled" stub for the right message.
			stored = plans.Plan{Name: a.Name, ParentPlan: a.ParentPlan, ParentItem: a.ParentItem}
		}
		if perr := PublishPlanSnapshot(ctx, t.cfg.NATSPublish, t.cfg.EnvelopeSigner, sess.Namespace, sess.Name,
			stored, toEnvelopeDiff(res.Diff), false, "", seq, uid); perr != nil {
			return tool.Result{Content: fmt.Sprintf("update_plan: publish failed: %v", perr), IsError: true, Trusted: true}, nil
		}

		// Mirror the plan's current step into the in-place status caption: the
		// in_progress item IS "what the agent is doing right now", which is
		// exactly what the status indicator conveys. It rides the same
		// KindNotification pipeline as update_status, so it persists until the
		// next status or plan change overwrites it. The full label is sent
		// verbatim; clamping an over-long one to a generic caption is the
		// channel kind's job, not this layer's. No in_progress item (all
		// pending/done, or a deletion) ⇒ nothing is "happening now", so the
		// caption is left untouched.
		if label, ok := inProgressLabel(stored); ok {
			if perr := publishStatusNotification(ctx, t.cfg.NATSPublish, t.cfg.EnvelopeSigner, sess.Namespace, sess.Name,
				channelevents.NotificationPayload{Text: label}, seq, uid); perr != nil {
				return tool.Result{Content: fmt.Sprintf("update_plan: status publish failed: %v", perr), IsError: true, Trusted: true}, nil
			}
		}
	}

	return marshalUpdatePlanResultWithNotices(res, phaseNotices)
}

// PublishPlanSnapshot publishes a KindPlanUpdate envelope for one plan
// snapshot with the given activity flags. Shared by update_plan (active
// snapshots on mutation, paused=false) and the runner loop (paused/active
// echoes at turn transitions). natsPublish may be the same func update_plan
// holds. seq/uid stamp the envelope's logical order + session instance so the
// consumer can order snapshots and reset its baseline on a fork/recreate; pass
// (0, "") from callers that have no IDs context. Returns the publish error for
// the caller to log; best-effort.
func PublishPlanSnapshot(
	ctx context.Context,
	natsPublish func(ctx context.Context, subject string, payload []byte) error,
	signer *channelevents.EnvelopeSigner,
	ns, name string,
	plan plans.Plan,
	diff channelevents.PlanDiff,
	paused bool,
	pauseCause string,
	seq uint64,
	uid string,
) error {
	payload := plan.ToEnvelopePayload()
	payload.Diff = diff
	payload.Paused = paused
	payload.PauseCause = pauseCause
	publish := func(subject string, data []byte) error {
		return publishWithRetry(ctx, natsPublish, subject, data)
	}
	return signer.PublishOutSeq(publish, ns, name, channelevents.KindPlanUpdate, payload, seq, uid)
}

// inProgressLabel returns the label of the plan's single in_progress item, if
// any. The plans store enforces at most one in_progress item per plan, so the
// first match is authoritative.
func inProgressLabel(p plans.Plan) (string, bool) {
	for _, it := range p.Items {
		if it.Status == plans.StatusInProgress {
			return it.Label, true
		}
	}
	return "", false
}

func marshalUpdatePlanResult(res plans.UpdateResult) (tool.Result, error) {
	return marshalUpdatePlanResultWithNotices(res, nil)
}

// marshalUpdatePlanResultWithNotices renders the result, appending anything the
// phase freeze dropped.
//
// Notices go AFTER the JSON body, on their own lines, so the structured result
// stays the first line and every existing reader is unaffected. They are not an
// error: the plan was stored and the phase is real, just narrower than
// authored. Returning IsError would tell the agent to retry the whole call,
// when what it needs to do is re-declare one phase with handles that exist.
//
// Silent on a clean freeze, deliberately. A notice appended to every update
// becomes noise the model learns to skip, which would cost this the one moment
// it has to be heard.
func marshalUpdatePlanResultWithNotices(res plans.UpdateResult, notices []string) (tool.Result, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("update_plan: marshal result: %w", err)
	}
	content := string(body)
	if len(notices) > 0 {
		content += "\n\nSome of what you declared was DROPPED and does not govern anything:\n  - " +
			strings.Join(notices, "\n  - ")
	}
	return tool.Result{Content: content, Trusted: true}, nil
}

func toEnvelopeDiff(d plans.Diff) channelevents.PlanDiff {
	out := channelevents.PlanDiff{Added: d.Added, Removed: d.Removed}
	for _, r := range d.Renamed {
		out.Renamed = append(out.Renamed, channelevents.PlanRenameDiff{ID: r.ID, From: r.From, To: r.To})
	}
	for _, sc := range d.StatusChanged {
		out.StatusChanged = append(out.StatusChanged, channelevents.PlanStatusChangeDiff{
			ID: sc.ID, From: string(sc.From), To: string(sc.To),
		})
	}
	return out
}
