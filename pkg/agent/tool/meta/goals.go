package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

const GoalResourceType = "agent_goal_domain"

type GoalsCaller func(context.Context, goals.Request) (goals.Response, error)

func IsGoalTool(name string) bool {
	switch name {
	case "list_goals", "get_goal", "list_goal_runs", "create_goal", "update_goal", "request_goal_execution", "request_goal_discovery", "get_goal_discovery_policy", "get_goal_discovery_proposal", "stop_goal_discovery":
		return true
	}
	return false
}

func IsGoalWrite(name string) bool {
	return name == "create_goal" || name == "update_goal" || name == "request_goal_execution" || name == "request_goal_discovery" || name == "stop_goal_discovery"
}

func NewGoalTools(call GoalsCaller) []tool.Tool {
	out := []tool.Tool{}
	for _, op := range []string{"list", "get", "runs", "create", "update", "request_execution", "request_discovery", "get_discovery_policy", "get_discovery_proposal", "stop_discovery"} {
		out = append(out, &goalTool{op: op, call: call})
	}
	return out
}

type goalTool struct {
	op   string
	call GoalsCaller
}

func (t *goalTool) Name() string {
	switch t.op {
	case "request_discovery":
		return "request_goal_discovery"
	case "get_discovery_policy":
		return "get_goal_discovery_policy"
	case "get_discovery_proposal":
		return "get_goal_discovery_proposal"
	case "stop_discovery":
		return "stop_goal_discovery"
	}
	if t.op == "request_execution" {
		return "request_goal_execution"
	}
	if t.op == "runs" {
		return "list_goal_runs"
	}
	if t.op == "list" {
		return "list_goals"
	}
	return t.op + "_goal"
}

func (t *goalTool) Kind() tool.Kind      { return tool.KindMeta }
func (t *goalTool) PipelineRouted() bool { return true }

// groupScopeWhen matches a call addressing the conversation's group goals.
const groupScopeWhen = `has(args.scope) && args.scope == "group"`

// PermissionVariants relaxes WHO dispatch admits for a group-scope write,
// never WHAT the call is: it stays Readwrite, so the trifecta's leg C and the
// pre-dispatch snapshot see a write exactly as for private scope.
//
// The check is pause (any member of the conversation's group, or a platform
// admin) rather than manage, because the goal server is the authority for
// group writes: it checks the author of the turn's opening message — dispatch
// would ask about whoever spoke last — with manage, pause or membership as the
// operation needs, plus the goal's own manager list. A creator manages
// nothing before the goal exists, so manage here would refuse every create.
// The resource arg comes from list_goals with scope group, whose resolve
// writes the domain's member_group tuple, so a member passes. The plan gate
// still governs the call by the tool's static handle.
func (t *goalTool) PermissionVariants() []authz.PermissionVariant {
	if !IsGoalWrite(t.Name()) {
		return nil
	}
	check := *t.Permission().Check
	check.Permission = "pause"
	return []authz.PermissionVariant{{When: groupScopeWhen, Check: authz.Permission{StateImpact: authz.Readwrite, Check: &check}}}
}

func (t *goalTool) Permission() authz.Permission {
	if !IsGoalWrite(t.Name()) {
		return authz.Permission{StateImpact: authz.Stateless}
	}
	return authz.Permission{StateImpact: authz.Readwrite, Check: &authz.PermissionCheck{ResourceType: GoalResourceType, Permission: "manage", ResourceIDExpr: "args.resource.split(':')[1]"}}
}

func (t *goalTool) Description() string {
	if t.op == "request_discovery" || t.op == "get_discovery_policy" || t.op == "get_discovery_proposal" || t.op == "stop_discovery" {
		return "Manage finite private goal suggestions. request_goal_discovery asks the verified human to " +
			"enable one exact accessible source/predicate, finite expiry, total question limit and " +
			"pending limit. It creates no goal or execution authority. Each matching observation must " +
			"contain a top-level string in subjectField; that literal entity becomes the proposed " +
			"future watch subject, while terms.event sets its exact kind, string predicates and finite " +
			"private-report limits. Each proposal needs its own exact human consent before one " +
			"goal/watch is created. An unrelated chat yes or an agent's account is not consent. " +
			"list_goals supplies the resource handle for writes. get_goal_discovery_policy/proposal " +
			"read retained evidence only with current access; stop_goal_discovery stops new proposals " +
			"and acceptance, without cancelling already accepted goals. Never infer discovery consent " +
			"from read access or scheduling. No external capabilities are granted."
	}
	return groupGoalsGuidance + "Manage private durable goals for this session's verified human and AgentClass. Call " +
		"list_goals first to obtain the resource for writes and discover eventFeeds, which contain " +
		"exact accessible source references and supported eventKinds. Use a configured or " +
		"goal-produced feed matching the request; never invent a source kind, source ID or event " +
		"kind. If no suitable feed is listed, explain that monitoring is not enabled and ask for " +
		"the integration to be connected; do not keep retrying or substitute a schedule. Goals " +
		"survive sessions. list_goal_runs returns durable execution history; session_ended is not " +
		"proof of goal success, and effects marked unknown are not delivery receipts. A due time is " +
		"schedule intent only. request_goal_execution asks the human to authorize a bounded future " +
		"private reporting session or finite recurring series; it does not authorize external " +
		"actions. Activate the goal before requesting execution. list_goals also returns " +
		"executionPolicy with serverTime, defaultBounds and class maxBounds. Use UTC dueAt in the " +
		"future with enough lead for approval, and expiresAt after dueAt within 30 days. Prefer " +
		"omitting bounds to use conservative class-capped defaults, which are included in the exact " +
		"human consent. If supplying bounds, all four fields are required, each must respect class " +
		"maxBounds, and approvalSeconds must not exceed durationSeconds. Repair rejected fields " +
		"using the error and a new requestID; never repeat unchanged invalid terms or replace an " +
		"event watch with a calendar schedule to work around an error. If setup cannot finish, tell " +
		"the user monitoring is not enabled and conclude the session. A scheduled collector can " +
		"publish private observations by requesting allowedOperations=[report_goal_event] with " +
		"terms.report={kind,subject}. In its run, include observation={kind,subject,data} in " +
		"report_goal_result; the platform publishes the recorded result with source dependencies " +
		"and a stable source. Goal-produced feeds appear in get_goal/list_goals after the report " +
		"policy is proposed. Use them to connect a collector to an event-driven notification goal. " +
		"Simulation data must be explicitly labeled. Reporting consent grants no external tools or " +
		"delegation. Optional schedule supports once, interval, daily or weekly in an explicit IANA " +
		"timezone, at most 100 nominal runs within 30 days, with per-run windows and optional local " +
		"quiet hours. Quiet hours defer runs and coalesce collisions; missed windows are skipped. " +
		"For an event watch, dueAt is when listening starts (usually soon), not the future time of " +
		"the flight or other expected event. Instead of schedule, terms.event watches one exact " +
		"accessible source and literal event kind/subject with optional top-level string " +
		"predicates, finite maxRuns, original event deadline, timezone/quiet hours and " +
		"burst=skip_pending. The server pins the source UID. Event data grants no capabilities. " +
		"Each created session must declare a fresh action plan. By default the human approves each " +
		"run. terms.actionApproval=standing_private explicitly asks for unattended private delivery " +
		"within the approved finite schedule, recipient and limits; never infer that consent from " +
		"scheduling alone. Updates require the current revision and a unique requestID; reuse the " +
		"same requestID only when retrying identical arguments. Completion is reported success with " +
		"evidence, not independent verification. A different employee must use their own session."
}

// groupGoalsGuidance leads every goal tool's description.
const groupGoalsGuidance = "scope selects whose goals: private (the default, below) or group. " +
	"scope=group addresses goals owned by everyone in this shared conversation; the platform " +
	"resolves the group from the conversation, never from arguments. Group goals act as the " +
	"agent, not as any one person. Any member may list, read, create and pause them; the " +
	"creator becomes the first manager, and only a manager may revise, resume, cancel or " +
	"complete one, add or remove managers (update_goal action=add_manager/remove_manager with " +
	"manager), or ask for it to run, which needs a manager's approval. A group goal always keeps " +
	"at least one manager in the group. A group request acts for the person whose message " +
	"started this turn; if it is refused because someone else spoke since, ask them to repeat " +
	"the request. Pass the same scope on every call for a goal, and use the resource that " +
	"list_goals returns for that scope. Before a group goal's approval runs out, its managers " +
	"get a renewal card in one of its runs; resuming a paused group goal posts a card here to " +
	"re-approve the terms it last ran under. If a group goal shows blockedReason \"approval " +
	"expired\", or a resumed one has no pending execution, a manager asks again with " +
	"request_goal_execution, omitting terms to re-request the last approved ones. On a channel " +
	"without delivery receipts, a group run's reply shows in list_goal_runs with reply state " +
	"\"unconfirmed\": it was handed to the channel for posting; delivery was not confirmed, so " +
	"never report it as delivered. Reply state \"publish_failed\" means it was handed off but " +
	"never reached the channel, so nothing was posted. Of the reply states, only \"accepted\" " +
	"carries a delivery receipt. "

func (t *goalTool) InputSchema() json.RawMessage {
	props := map[string]any{}
	required := []string{}
	str := func() map[string]any { return map[string]any{"type": "string"} }
	switch t.op {
	case "request_discovery":
		var execution map[string]any
		if err := json.Unmarshal((&goalTool{op: "request_execution"}).InputSchema(), &execution); err != nil {
			panic(err)
		}
		ep := execution["properties"].(map[string]any)
		props["resource"] = str()
		props["requestID"] = str()
		props["title"] = str()
		props["outcome"] = str()
		props["terms"] = ep["terms"]
		event := goalEventSchema()["properties"].(map[string]any)
		props["predicate"] = event["predicate"]
		props["subjectField"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
		props["maxProposals"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		props["maxPending"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		props["proposalSeconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}
		required = []string{"resource", "requestID", "title", "outcome", "terms", "predicate", "subjectField", "maxProposals", "maxPending", "proposalSeconds"}
	case "get_discovery_policy", "get_discovery_proposal", "stop_discovery":
		props["id"] = str()
		required = []string{"id"}
		if t.op == "stop_discovery" {
			props["resource"] = str()
			required = append(required, "resource")
		}
	case "request_execution":
		props["resource"] = str()
		props["requestID"] = str()
		props["id"] = str()
		props["revision"] = map[string]any{"type": "integer", "minimum": 1}
		props["terms"] = map[string]any{"type": "object", "additionalProperties": false, "description": "Required, except for a group goal approved before: omit terms entirely to renew its approval in force (it keeps running until a manager approves the renewal), or, once that has run out, to ask its managers to re-approve the terms it last ran under, with a fresh window.", "required": []string{"dueAt", "expiresAt", "allowedOperations", "evidence"}, "properties": map[string]any{
			"actionApproval": map[string]any{"type": "string", "enum": []string{"manual", "standing_private", "standing_plan"}, "description": "manual requires fresh human plan approval for each run (default). standing_private asks the human to authorize unattended private delivery within this exact finite schedule and destination. standing_plan (group goals only, with standingPlan) asks the goal's managers to approve standingPlan once: each run whose own plan stays within it runs without a card; a run that asks for more gets a card for a manager."},
			"standingPlan":   standingPlanSchema(),
			"schedule":       sessionScheduleSchema(),
			"event":          goalEventSchema(),
			"dueAt":          map[string]any{"type": "string", "format": "date-time"}, "expiresAt": map[string]any{"type": "string", "format": "date-time"},
			"report":            map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "subject"}, "properties": map[string]any{"kind": map[string]any{"type": "string", "minLength": 1, "maxLength": 1024}, "subject": map[string]any{"type": "string", "minLength": 1, "maxLength": 1024}}},
			"bounds":            map[string]any{"type": "object", "description": "Optional: omit entirely for conservative class-capped defaults. If supplied, all four fields are required; approvalSeconds must not exceed durationSeconds and values must respect executionPolicy.maxBounds from list_goals.", "additionalProperties": false, "required": []string{"durationSeconds", "turns", "tokens", "approvalSeconds"}, "properties": map[string]any{"durationSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "turns": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000000}, "approvalSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}}},
			"allowedOperations": map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"respond_to_user", "report_goal_event"}}}, "evidence": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": str()}}}
		required = []string{"resource", "requestID", "id", "revision"}
	case "list", "runs":
		if t.op == "runs" {
			props["id"] = str()
			required = append(required, "id")
		}
		if t.op == "list" {
			props["state"] = map[string]any{"type": "string", "enum": []string{"draft", "active", "paused", "completed", "cancelled"}}
		}
		props["after"] = str()
		props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	case "get":
		props["id"] = str()
		required = append(required, "id")
	case "create", "update":
		props["resource"] = str()
		props["requestID"] = str()
		props["title"] = str()
		props["outcome"] = str()
		props["dueAt"] = map[string]any{"type": "string", "format": "date-time"}
		props["timezone"] = str()
		required = append(required, "resource", "requestID")
		if t.op == "create" {
			required = append(required, "title", "outcome")
		} else {
			props["id"] = str()
			props["revision"] = map[string]any{"type": "integer", "minimum": 1}
			props["action"] = map[string]any{"type": "string", "enum": []string{"revise", "activate", "pause", "resume", "cancel", "complete", "add_manager", "remove_manager"}}
			props["manager"] = map[string]any{"type": "string", "description": "add_manager/remove_manager only (group goals): the person's canonical user id, as goal managers lists them."}
			props["clearDue"] = map[string]any{"type": "boolean"}
			props["result"] = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"summary", "evidence"}, "properties": map[string]any{"summary": str(), "evidence": map[string]any{"type": "array", "items": str(), "minItems": 1}}}
			required = append(required, "id", "revision", "action")
		}
	}
	props["scope"] = map[string]any{"type": "string", "enum": []string{"private", "group"}, "description": "private (default): this session's verified human's goals. group: goals owned by everyone in this shared conversation."}
	b, err := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required})
	if err != nil {
		panic(fmt.Sprintf("goal schema: %v", err))
	}
	return b
}

func (t *goalTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if sess == nil || sess.IsDelegatedChild || t.call == nil {
		return tool.Result{IsError: true, Content: "Goals are unavailable in this session."}, nil
	}
	var base struct {
		Resource string `json:"resource"`
		ID       string `json:"id"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	req := goals.Request{Operation: t.op, Resource: base.Resource, ID: base.ID, Scope: base.Scope}
	var err error
	switch t.op {
	case "request_discovery":
		err = json.Unmarshal(raw, &req.Discovery)
	case "request_execution":
		req.Execution, err = decodeExecutionRequest(raw)
	case "create":
		err = json.Unmarshal(raw, &req.Create)
	case "update":
		err = json.Unmarshal(raw, &req.Change)
	case "list", "runs":
		err = json.Unmarshal(raw, &req.List)
	}
	if err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	out, err := t.call(ctx, req)
	if err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: string(b)}, nil
}

// standingPlanSchema is a group goal's standing plan: phases in exactly the
// shape update_plan takes, which the goal server freezes as the runner
// freezes a run's plan.
func standingPlanSchema() map[string]any {
	var phase map[string]any
	if err := json.Unmarshal([]byte(authoredPhaseItemSchema), &phase); err != nil {
		panic(fmt.Sprintf("standing plan schema: %v", err))
	}
	return map[string]any{"type": "array", "minItems": 1, "maxItems": defaultMaxPhases, "items": phase,
		"description": "Group goals with actionApproval standing_plan only: the plan every run is held to, as update_plan phases. Name every resource concretely (slots[].id); a run holds one resource of each slot type, so name at most one per type. Include a phase with perm:execute:agent_goal_execution for the run's reply and result."}
}

// decodeExecutionRequest reads request_goal_execution's arguments. A standing
// plan is authored inside terms (terms.standingPlan, update_plan phases) but
// travels beside them (ExecutionRequest.StandingPlan): Terms holds only the
// plan the goal server froze, never one a caller wrote.
func decodeExecutionRequest(raw json.RawMessage) (goals.ExecutionRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return goals.ExecutionRequest{}, err
	}
	// Only terms.standingPlan is read: the one place the schema offers.
	delete(top, "standingPlan")
	var plan json.RawMessage
	if terms, ok := top["terms"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(terms, &fields); err != nil {
			return goals.ExecutionRequest{}, err
		}
		if p, ok := fields["standingPlan"]; ok {
			plan = p
			delete(fields, "standingPlan")
			rest, err := json.Marshal(fields)
			if err != nil {
				return goals.ExecutionRequest{}, err
			}
			top["terms"] = rest
		}
	}
	rest, err := json.Marshal(top)
	if err != nil {
		return goals.ExecutionRequest{}, err
	}
	var r goals.ExecutionRequest
	if err := json.Unmarshal(rest, &r); err != nil {
		return goals.ExecutionRequest{}, err
	}
	if len(plan) > 0 {
		if err := json.Unmarshal(plan, &r.StandingPlan); err != nil {
			return goals.ExecutionRequest{}, fmt.Errorf("terms.standingPlan: %w", err)
		}
	}
	return r, nil
}

func sessionScheduleSchema() map[string]any {
	days := func() map[string]any {
		return map[string]any{"type": "array", "minItems": 1, "maxItems": 7, "uniqueItems": true, "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 6}, "description": "Sunday=0 through Saturday=6. Weekly schedule days, or days a quiet period starts."}
	}
	clock := func() map[string]any {
		return map[string]any{"type": "string", "pattern": "^([01][0-9]|2[0-3]):[0-5][0-9]$"}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "timezone", "maxRuns", "runWindowSeconds"}, "properties": map[string]any{
		"kind":            map[string]any{"type": "string", "enum": sessionschedule.Kinds()},
		"timezone":        map[string]any{"type": "string", "description": "Explicit IANA timezone, e.g. America/New_York. Daily/weekly use the local clock time of dueAt; nonexistent times skip, repeated times run once."},
		"intervalSeconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 2592000, "description": "Required only for interval."},
		"weekdays":        days(), "maxRuns": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		"runWindowSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400, "description": "Each run expires this many seconds after its effective due time, shortened at quiet hours or expiresAt. expiresAt ends the entire series."},
		"quietHours":       map[string]any{"type": "array", "maxItems": 14, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"start", "end"}, "properties": map[string]any{"start": clock(), "end": clock(), "weekdays": days()}}},
	}}
}

func goalEventSchema() map[string]any {
	schedule := sessionScheduleSchema()["properties"].(map[string]any)
	str := func() map[string]any { return map[string]any{"type": "string", "minLength": 1, "maxLength": 1024} }
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"source", "predicate", "maxRuns", "runWindowSeconds", "timezone", "burst"}, "properties": map[string]any{
		"source":    map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "namespace", "id"}, "properties": map[string]any{"kind": str(), "namespace": str(), "id": str(), "uid": str()}},
		"predicate": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "subject"}, "properties": map[string]any{"kind": str(), "subject": str(), "equals": map[string]any{"type": "object", "maxProperties": 16, "additionalProperties": map[string]any{"type": "string", "maxLength": 1024}}}},
		"maxRuns":   schedule["maxRuns"], "runWindowSeconds": schedule["runWindowSeconds"], "timezone": schedule["timezone"], "quietHours": schedule["quietHours"], "burst": map[string]any{"type": "string", "enum": []string{"skip_pending"}},
	}}
}
