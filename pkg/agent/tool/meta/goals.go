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
func (t *goalTool) Kind() tool.Kind                               { return tool.KindMeta }
func (t *goalTool) PipelineRouted() bool                          { return true }
func (t *goalTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (t *goalTool) Permission() authz.Permission {
	if !IsGoalWrite(t.Name()) {
		return authz.Permission{StateImpact: authz.Stateless}
	}
	return authz.Permission{StateImpact: authz.Readwrite, Check: &authz.PermissionCheck{ResourceType: GoalResourceType, Permission: "manage", ResourceIDExpr: "args.resource.split(':')[1]"}}
}
func (t *goalTool) Description() string {
	if t.op == "request_discovery" || t.op == "get_discovery_policy" || t.op == "get_discovery_proposal" || t.op == "stop_discovery" {
		return "Manage finite private goal suggestions. request_goal_discovery asks the verified human to enable one exact accessible source/predicate, finite expiry, total question limit and pending limit. It creates no goal or execution authority. Each matching observation must contain a top-level string in subjectField; that literal entity becomes the proposed future watch subject, while terms.event sets its exact kind, string predicates and finite private-report limits. Each proposal needs its own exact human consent before one goal/watch is created. An unrelated chat yes or an agent's account is not consent. list_goals supplies the resource handle for writes. get_goal_discovery_policy/proposal read retained evidence only with current access; stop_goal_discovery stops new proposals and acceptance, without cancelling already accepted goals. Never infer discovery consent from read access or scheduling. No external capabilities are granted."
	}
	return "Manage private durable goals for this session's verified human and AgentClass. Call list_goals first to obtain the resource for writes. Goals survive sessions. list_goal_runs returns durable execution history; session_ended is not proof of goal success, and effects marked unknown are not delivery receipts. A due time is schedule intent only. request_goal_execution asks the human to authorize a bounded future private reporting session or finite recurring series; it does not authorize external actions. Use UTC dueAt and expiresAt and finite per-run duration, turn, token and approval ceilings. Optional schedule supports once, interval, daily or weekly in an explicit IANA timezone, at most 100 nominal runs within 30 days, with per-run windows and optional local quiet hours. Quiet hours defer runs and coalesce collisions; missed windows are skipped. Instead of schedule, terms.event watches one exact accessible source and literal event kind/subject with optional top-level string predicates, finite maxRuns, original event deadline, timezone/quiet hours and burst=skip_pending. The server pins the source UID. Event data grants no capabilities. Each created session must declare a fresh action plan. By default the human approves each run. terms.actionApproval=standing_private explicitly asks for unattended private delivery within the approved finite schedule, recipient and limits; never infer that consent from scheduling alone. Updates require the current revision and a unique requestID; reuse the same requestID only when retrying identical arguments. Completion is reported success with evidence, not independent verification. A different employee must use their own session."
}
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
		props["terms"] = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"dueAt", "expiresAt", "bounds", "allowedOperations", "evidence"}, "properties": map[string]any{
			"actionApproval": map[string]any{"type": "string", "enum": []string{"manual", "standing_private"}, "description": "manual requires fresh human plan approval for each run (default). standing_private asks the human to authorize unattended private delivery within this exact finite schedule and destination."},
			"schedule":       sessionScheduleSchema(),
			"event":          goalEventSchema(),
			"dueAt":          map[string]any{"type": "string", "format": "date-time"}, "expiresAt": map[string]any{"type": "string", "format": "date-time"},
			"bounds":            map[string]any{"type": "object", "additionalProperties": false, "required": []string{"durationSeconds", "turns", "tokens", "approvalSeconds"}, "properties": map[string]any{"durationSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "turns": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000000}, "approvalSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}}},
			"allowedOperations": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "string", "enum": []string{"respond_to_user"}}}, "evidence": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": str()}}}
		required = []string{"resource", "requestID", "id", "revision", "terms"}
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
			props["action"] = map[string]any{"type": "string", "enum": []string{"revise", "activate", "pause", "resume", "cancel", "complete"}}
			props["clearDue"] = map[string]any{"type": "boolean"}
			props["result"] = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"summary", "evidence"}, "properties": map[string]any{"summary": str(), "evidence": map[string]any{"type": "array", "items": str(), "minItems": 1}}}
			required = append(required, "id", "revision", "action")
		}
	}
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
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	req := goals.Request{Operation: t.op, Resource: base.Resource, ID: base.ID}
	var err error
	switch t.op {
	case "request_discovery":
		err = json.Unmarshal(raw, &req.Discovery)
	case "request_execution":
		err = json.Unmarshal(raw, &req.Execution)
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
