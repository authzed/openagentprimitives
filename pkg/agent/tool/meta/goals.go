package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

const GoalResourceType = "agent_goal_domain"

type GoalsCaller func(context.Context, goals.Request) (goals.Response, error)

func IsGoalTool(name string) bool {
	switch name {
	case "list_goals", "get_goal", "create_goal", "update_goal", "request_goal_execution":
		return true
	}
	return false
}
func IsGoalWrite(name string) bool {
	return name == "create_goal" || name == "update_goal" || name == "request_goal_execution"
}
func NewGoalTools(call GoalsCaller) []tool.Tool {
	out := []tool.Tool{}
	for _, op := range []string{"list", "get", "create", "update", "request_execution"} {
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
	return "Manage private durable goals for this session's verified human and AgentClass. Call list_goals first to obtain the resource for writes. Goals survive sessions. A due time is schedule intent only. request_goal_execution asks the human to authorize one bounded future private reporting session; it does not authorize external actions. Use UTC dueAt and expiresAt and finite duration, turn, token and approval ceilings. Updates require the current revision and a unique requestID; reuse the same requestID only when retrying identical arguments. Completion is reported success with evidence, not independent verification. A different employee must use their own session."
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
			"dueAt": map[string]any{"type": "string", "format": "date-time"}, "expiresAt": map[string]any{"type": "string", "format": "date-time"},
			"bounds":            map[string]any{"type": "object", "additionalProperties": false, "required": []string{"durationSeconds", "turns", "tokens", "approvalSeconds"}, "properties": map[string]any{"durationSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "turns": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000000}, "approvalSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}}},
			"allowedOperations": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "string", "enum": []string{"respond_to_user"}}}, "evidence": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": str()}}}
		required = []string{"resource", "requestID", "id", "revision", "terms"}
	case "list":
		props["state"] = map[string]any{"type": "string", "enum": []string{"draft", "active", "paused", "completed", "cancelled"}}
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
	case "list":
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
