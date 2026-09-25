package audit

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func base(e memory.Entry, kind string) Event {
	ns, name := splitScopeID(e.Scope.ID)
	return Event{
		Time: e.CreatedAt, SessionNamespace: ns, SessionName: name,
		Kind: kind, EntryID: e.ID, Raw: e.Content,
	}
}

// --- approval (pkg/memory/kinds/approval: Request | Outcome entries) ---

type approvalMapper struct{}

func (approvalMapper) MemoryKind() string   { return "approval" }
func (approvalMapper) EventKinds() []string { return []string{"approval"} }
func (approvalMapper) Map(e memory.Entry) ([]Event, error) {
	var c struct {
		ToolName string `json:"toolName"`
		Approver string `json:"approver"`
		Reason   string `json:"reason"`
		Decision string `json:"decision"` // set on Outcome entries only
	}
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return nil, fmt.Errorf("approval %s: %w", e.ID, err)
	}
	ev := base(e, "approval")
	ev.Tool = c.ToolName
	ev.Actor = c.Approver
	if c.Decision != "" {
		ev.Outcome = c.Decision
		ev.Summary = fmt.Sprintf("tool %q %s by %s", c.ToolName, c.Decision, c.Approver)
	} else {
		ev.Outcome = "pending"
		ev.Summary = fmt.Sprintf("approval requested for tool %q (approver %s)", c.ToolName, c.Approver)
	}
	return []Event{ev}, nil
}

// --- authz_decision (pkg/memory/kinds/authzdecision.Decision) ---

type authzDecisionMapper struct{}

func (authzDecisionMapper) MemoryKind() string   { return "authz_decision" }
func (authzDecisionMapper) EventKinds() []string { return []string{"authz_decision"} }
func (authzDecisionMapper) Map(e memory.Entry) ([]Event, error) {
	var c struct {
		Outcome      string `json:"outcome"`
		Subject      string `json:"subject"`
		ResourceType string `json:"resourceType"`
		ResourceID   string `json:"resourceId"`
		Permission   string `json:"permission"`
	}
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return nil, fmt.Errorf("authz_decision %s: %w", e.ID, err)
	}
	ev := base(e, "authz_decision")
	ev.Actor = c.Subject
	ev.Outcome = c.Outcome
	ev.Summary = fmt.Sprintf("%s %s on %s:%s for %s", c.Outcome, c.Permission, c.ResourceType, c.ResourceID, c.Subject)
	return []Event{ev}, nil
}

// --- scope_audit (pkg/memory/kinds/scopeaudit.Content) ---

type scopeAuditMapper struct{}

func (scopeAuditMapper) MemoryKind() string   { return "scope_audit" }
func (scopeAuditMapper) EventKinds() []string { return []string{"scope_change"} }
func (scopeAuditMapper) Map(e memory.Entry) ([]Event, error) {
	var c struct {
		AppliedAt time.Time `json:"appliedAt"`
		Source    string    `json:"source"`
		Approver  string    `json:"approver"`
		Requester string    `json:"requester"`
	}
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return nil, fmt.Errorf("scope_audit %s: %w", e.ID, err)
	}
	ev := base(e, "scope_change")
	if !c.AppliedAt.IsZero() {
		ev.Time = c.AppliedAt
	}
	ev.Actor = c.Approver
	if ev.Actor == "" {
		ev.Actor = c.Requester
	}
	ev.Outcome = "applied"
	ev.Summary = fmt.Sprintf("scope delta applied (source=%s)", c.Source)
	return []Event{ev}, nil
}

// --- infoleakage_audit (pkg/memory/kinds/infoleakageaudit.AuditRecord) ---

type leakageMapper struct{}

func (leakageMapper) MemoryKind() string   { return "infoleakage_audit" }
func (leakageMapper) EventKinds() []string { return []string{"leakage"} }
func (leakageMapper) Map(e memory.Entry) ([]Event, error) {
	var c struct {
		At        time.Time `json:"at"`
		Kind      string    `json:"kind"`
		Tool      string    `json:"tool"`
		Requester string    `json:"requester"`
	}
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return nil, fmt.Errorf("infoleakage_audit %s: %w", e.ID, err)
	}
	ev := base(e, "leakage")
	if !c.At.IsZero() {
		ev.Time = c.At
	}
	ev.Tool = c.Tool
	ev.Actor = c.Requester
	ev.Outcome = c.Kind
	ev.Summary = fmt.Sprintf("information-leakage gate: %s (tool=%s)", c.Kind, c.Tool)
	return []Event{ev}, nil
}

// --- relwrites_audit (pkg/memory/kinds/relwritesaudit.Audit) ---

type relWritesMapper struct{}

func (relWritesMapper) MemoryKind() string   { return "relwrites_audit" }
func (relWritesMapper) EventKinds() []string { return []string{"rel_writes"} }
func (relWritesMapper) Map(e memory.Entry) ([]Event, error) {
	var c struct {
		Source string            `json:"source"`
		Tuples []json.RawMessage `json:"tuples"`
	}
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return nil, fmt.Errorf("relwrites_audit %s: %w", e.ID, err)
	}
	ev := base(e, "rel_writes")
	ev.Outcome = "applied"
	ev.Summary = fmt.Sprintf("%d tuple(s) written to SpiceDB (source=%s)", len(c.Tuples), c.Source)
	return []Event{ev}, nil
}

// --- lifecycle (signal-kind tag is the content; payload is opaque) ---

type lifecycleMapper struct{}

func (lifecycleMapper) MemoryKind() string   { return "lifecycle" }
func (lifecycleMapper) EventKinds() []string { return []string{"lifecycle"} }
func (lifecycleMapper) Map(e memory.Entry) ([]Event, error) {
	ev := base(e, "lifecycle")
	ev.Outcome = "info"
	if len(e.Tags) > 0 {
		ev.Summary = e.Tags[0] // the signal kind, e.g. "lifecycle/turn.completed"
	} else {
		ev.Summary = e.ID
	}
	return []Event{ev}, nil
}

// --- tool_session (interactive-tool parsed events; shape is per-toolkit) ---

type toolSessionMapper struct{}

func (toolSessionMapper) MemoryKind() string   { return "tool_session" }
func (toolSessionMapper) EventKinds() []string { return []string{"tool_session"} }
func (toolSessionMapper) Map(e memory.Entry) ([]Event, error) {
	ev := base(e, "tool_session")
	ev.Outcome = "info"
	s := string(e.Content)
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	ev.Summary = s
	return []Event{ev}, nil
}

// --- turn (memory.Turn; tool_use blocks only) ---

type turnMapper struct{}

func (turnMapper) MemoryKind() string   { return "turn" }
func (turnMapper) EventKinds() []string { return []string{"tool_call"} }
func (turnMapper) Map(e memory.Entry) ([]Event, error) {
	var turn memory.Turn
	if err := json.Unmarshal(e.Content, &turn); err != nil {
		return nil, fmt.Errorf("turn %s: %w", e.ID, err)
	}
	var out []Event
	for _, blk := range turn.Content {
		if blk.Type != "tool_use" || blk.ToolUse == nil {
			continue
		}
		ev := base(e, "tool_call")
		if !turn.CreatedAt.IsZero() {
			ev.Time = turn.CreatedAt
		}
		ev.Tool = blk.ToolUse.Name
		ev.Outcome = "dispatched"
		ev.Summary = fmt.Sprintf("tool call %q", blk.ToolUse.Name)
		out = append(out, ev)
	}
	return out, nil
}

func init() {
	Register(approvalMapper{})
	Register(authzDecisionMapper{})
	Register(scopeAuditMapper{})
	Register(leakageMapper{})
	Register(relWritesMapper{})
	Register(lifecycleMapper{})
	Register(toolSessionMapper{})
	Register(turnMapper{})
}
