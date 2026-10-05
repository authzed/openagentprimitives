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
	case "list_goals", "get_goal", "list_goal_runs", "create_goal", "update_goal", "request_goal_execution":
		return true
	}
	return false
}
func IsGoalWrite(name string) bool {
	return name == "create_goal" || name == "update_goal" || name == "request_goal_execution"
}
func NewGoalTools(call GoalsCaller) []tool.Tool {
	out := []tool.Tool{}
	for _, op := range []string{"list", "get", "runs", "create", "update", "request_execution"} {
		out = append(out, &goalTool{op: op, call: call})
	}
	return out
}

type goalTool struct {
	op   string
	call GoalsCaller
}

func (t *goalTool) Name() string {
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
	return "Manage private durable goals for this session's verified human and AgentClass. Call list_goals first to obtain the resource for writes. Goals survive sessions. list_goal_runs returns durable execution history; session_ended is not proof of goal success, and effects marked unknown are not delivery receipts. A due time is schedule intent only. request_goal_execution asks the human to authorize a bounded future private reporting session or finite recurring series; it does not authorize external actions. Use UTC dueAt and expiresAt and finite per-run duration, turn, token and approval ceilings. Optional schedule supports once, interval, daily or weekly in an explicit IANA timezone, at most 100 nominal runs within 30 days, with per-run windows and optional local quiet hours. Quiet hours defer runs and coalesce collisions; missed windows are skipped. Each created session still requires its own fresh action plan approval. Updates require the current revision and a unique requestID; reuse the same requestID only when retrying identical arguments. Completion is reported success with evidence, not independent verification. A different employee must use their own session."
}
func (t *goalTool) InputSchema() json.RawMessage {
	props := map[string]any{}
	required := []string{}
	str := func() map[string]any { return map[string]any{"type": "string"} }
	switch t.op {
	case "request_execution":
		props["resource"] = str()
		props["requestID"] = str()
		props["id"] = str()
		props["revision"] = map[string]any{"type": "integer", "minimum": 1}
		props["terms"] = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"dueAt", "expiresAt", "bounds", "allowedOperations", "evidence"}, "properties": map[string]any{
			"schedule": sessionScheduleSchema(),
			"dueAt":    map[string]any{"type": "string", "format": "date-time"}, "expiresAt": map[string]any{"type": "string", "format": "date-time"},
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
