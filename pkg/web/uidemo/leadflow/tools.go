package leadflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tool names, exported so a CR author — and this package's own wiring
// test — spell them identically to what the server announces, rather than
// re-typing the literal in a second place.
const (
	ToolListLeads        = "list_leads"
	ToolStageBreakdown   = "stage_breakdown"
	ToolAdvanceLeadStage = "advance_lead_stage"
	// ToolSearchAccounts is a READONLY tool answering with an ENVELOPE —
	// {"results":[…],"total":…,"paging":{…}}, each record's fields under
	// "properties" — rather than the bare array ToolListLeads returns.
	// Envelope-wrapped responses are the REST norm, so this one is shaped like
	// a third-party CRM search to give the selector path (pkg/web/uiselect)
	// something real to extract from end to end.
	ToolSearchAccounts = "search_accounts"
)

// defaultMaxRows is the fallback for Options.MaxRows.
const defaultMaxRows = 25

// StageCount is one entry of stage_breakdown's result: one per Stages
// element, always — see stageBreakdown.
type StageCount struct {
	Stage string `json:"stage"`
	Count int    `json:"count"`
}

// AccountProperties is one search_accounts record's fields, nested one
// level down under "properties" — the shape a component's selector
// ("results[].properties") must reach through, rather than the flat Lead
// shape ToolListLeads returns directly.
type AccountProperties struct {
	Name  string `json:"name"`
	Stage string `json:"stage"`
}

// AccountRecord is one element of search_accounts' "results" array.
type AccountRecord struct {
	Properties AccountProperties `json:"properties"`
}

// AccountPaging is search_accounts' paging cursor. Next is always empty in
// this fixture — there is only ever one page of the seeded book — but the
// key is present so a selector written against the envelope's shape (not
// just its populated fields) has something real to skip past.
type AccountPaging struct {
	Next string `json:"next,omitempty"`
}

// SearchAccountsEnvelope is search_accounts' response shape: a wrapper object,
// a results array, and a paging cursor — the REST norm ToolSearchAccounts
// exists to exercise.
type SearchAccountsEnvelope struct {
	Results []AccountRecord `json:"results"`
	Total   int             `json:"total"` // match count across all pages
	Paging  AccountPaging   `json:"paging"`
}

// stringProp builds a "type": "string" JSON Schema property. Every argument on
// every tool here is string-typed: uibindings.SubstituteParams marshals each
// substituted binding parameter as a Go string, and the app-tool call path has
// no InputSchema validation to catch a string where a number was declared.
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// toolDef is one row of the tool table registerTools iterates. Handler
// takes the server plus the raw argument bytes (nil when the caller sent
// no arguments) and returns a CallToolResult; it never returns a
// protocol-level error for a bad ARGUMENT — see errResult — reserving the
// error return for a genuine encoding failure.
type toolDef struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	ReadOnlyHint bool
	Handler      func(s *Server, args json.RawMessage) (*mcp.CallToolResult, error)
}

var toolTable = []toolDef{
	{
		Name:        ToolListLeads,
		Description: "List leads in the pipeline, optionally filtered by an update-date range and/or stage.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from":  stringProp("RFC3339 date-time lower bound on the lead's Updated field (inclusive); empty means unbounded."),
				"to":    stringProp("RFC3339 date-time upper bound on the lead's Updated field (inclusive); empty means unbounded."),
				"stage": stringProp("one of new/qualified/proposal/won; empty means all stages."),
			},
		},
		ReadOnlyHint: true,
		Handler:      handleListLeads,
	},
	{
		Name:        ToolStageBreakdown,
		Description: "Count leads per pipeline stage within an optional update-date range. Always reports every stage, even at zero.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": stringProp("RFC3339 date-time lower bound on the lead's Updated field (inclusive); empty means unbounded."),
				"to":   stringProp("RFC3339 date-time upper bound on the lead's Updated field (inclusive); empty means unbounded."),
			},
		},
		ReadOnlyHint: true,
		Handler:      handleStageBreakdown,
	},
	{
		Name:        ToolAdvanceLeadStage,
		Description: "Move one lead to the next pipeline stage.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"leadId": stringProp("the lead's id (Lead.Value)."),
				"note":   stringProp("an optional note echoed back with the result; not persisted."),
			},
			"required": []any{"leadId"},
		},
		ReadOnlyHint: false,
		Handler:      handleAdvanceLeadStage,
	},
	{
		Name:        ToolSearchAccounts,
		Description: "Search CRM accounts, optionally filtered by an update-date range and/or stage. Answers with a paged envelope, not a bare list.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from":  stringProp("RFC3339 date-time lower bound on the account's Updated field (inclusive); empty means unbounded."),
				"to":    stringProp("RFC3339 date-time upper bound on the account's Updated field (inclusive); empty means unbounded."),
				"stage": stringProp("one of new/qualified/proposal/won; empty means all stages."),
			},
		},
		ReadOnlyHint: true,
		Handler:      handleSearchAccounts,
	},
}

// registerTools iterates toolTable exactly once and registers each entry against
// srv — no switch on tool name anywhere in this package. Visibility rides in
// _meta, the shape probe.toolFromSDK recovers into Annotations.Visibility: every
// tool here is app-only and is never offered to the model.
func registerTools(srv *mcp.Server, s *Server) {
	for _, td := range toolTable {
		td := td
		srv.AddTool(&mcp.Tool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: td.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: td.ReadOnlyHint},
			Meta:        mcp.Meta{"ui": map[string]any{"visibility": []string{"app"}}},
		}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var argsForLog map[string]any
			if len(req.Params.Arguments) > 0 {
				// Best-effort: a decode failure here leaves the call log
				// entry's Args nil rather than dropping the entry or
				// panicking — the handler below still gets the raw bytes
				// and reports its own malformed-arguments error through the
				// normal tool-result path.
				_ = json.Unmarshal(req.Params.Arguments, &argsForLog)
			}
			s.recordCall(td.Name, argsForLog)
			res, err := td.Handler(s, req.Params.Arguments)
			// Never silently drop the outcome: every call is visible in the log,
			// success or failure, even in this demo binary.
			switch {
			case err != nil:
				s.logger.Info("leadflow: tool call failed", "tool", td.Name, "err", err.Error())
			case res != nil && res.IsError:
				s.logger.Info("leadflow: tool call returned an error result", "tool", td.Name)
			default:
				s.logger.Info("leadflow: tool call", "tool", td.Name)
			}
			return res, err
		})
	}
}

// validateRange rejects a from/to that isn't a well-formed RFC3339 date-time.
// The bounds are then compared as plain strings against Lead.Updated, so this
// exists to make a malformed value a reported MCP error rather than a silent
// fall-through to an unbounded full-table scan — not to convert to time.Time.
func validateRange(from, to string) error {
	if from != "" {
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			return fmt.Errorf("from: not an RFC3339 date-time: %w", err)
		}
	}
	if to != "" {
		if _, err := time.Parse(time.RFC3339, to); err != nil {
			return fmt.Errorf("to: not an RFC3339 date-time: %w", err)
		}
	}
	return nil
}

// stageBreakdown is stage_breakdown's core computation, factored out of the
// MCP handler so leadflow_test.go can exercise it directly without going
// through the wire. It always returns one entry per Stages, in Stages
// order, so a filter that empties a stage still moves the chart's series
// count, never its x-axis.
func stageBreakdown(b *Book, from, to string) ([]StageCount, error) {
	if err := validateRange(from, to); err != nil {
		return nil, err
	}
	leads := b.Filter(from, to, "")
	counts := make(map[string]int, len(Stages))
	for _, l := range leads {
		counts[l.Stage]++
	}
	out := make([]StageCount, len(Stages))
	for i, stage := range Stages {
		out[i] = StageCount{Stage: stage, Count: counts[stage]}
	}
	return out, nil
}

func handleListLeads(s *Server, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Stage string `json:"stage"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return errResult(fmt.Sprintf("%s: malformed arguments: %v", ToolListLeads, err)), nil
		}
	}
	if err := validateRange(in.From, in.To); err != nil {
		return errResult(fmt.Sprintf("%s: %v", ToolListLeads, err)), nil
	}
	leads := s.book.Filter(in.From, in.To, in.Stage)
	if s.maxRows > 0 && len(leads) > s.maxRows {
		leads = leads[:s.maxRows]
	}
	return jsonResult(leads)
}

func handleStageBreakdown(s *Server, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return errResult(fmt.Sprintf("%s: malformed arguments: %v", ToolStageBreakdown, err)), nil
		}
	}
	out, err := stageBreakdown(s.book, in.From, in.To)
	if err != nil {
		return errResult(fmt.Sprintf("%s: %v", ToolStageBreakdown, err)), nil
	}
	return jsonResult(out)
}

func handleAdvanceLeadStage(s *Server, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		LeadID string `json:"leadId"`
		Note   string `json:"note"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return errResult(fmt.Sprintf("%s: malformed arguments: %v", ToolAdvanceLeadStage, err)), nil
		}
	}
	if in.LeadID == "" {
		return errResult(fmt.Sprintf("%s: leadId is required", ToolAdvanceLeadStage)), nil
	}
	lead, err := s.book.AdvanceStage(in.LeadID)
	if err != nil {
		return errResult(fmt.Sprintf("%s: %v", ToolAdvanceLeadStage, err)), nil
	}
	return jsonResult(struct {
		Value string `json:"value"`
		Stage string `json:"stage"`
		Note  string `json:"note"`
	}{Value: lead.Value, Stage: lead.Stage, Note: in.Note})
}

// handleSearchAccounts answers search_accounts: the same seeded Book as
// every other tool here (so its results are deterministic and comparable
// against list_leads), reshaped into SearchAccountsEnvelope so the caller
// gets an envelope-with-nested-properties response rather than the flat
// Lead shape handleListLeads returns.
func handleSearchAccounts(s *Server, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Stage string `json:"stage"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return errResult(fmt.Sprintf("%s: malformed arguments: %v", ToolSearchAccounts, err)), nil
		}
	}
	if err := validateRange(in.From, in.To); err != nil {
		return errResult(fmt.Sprintf("%s: %v", ToolSearchAccounts, err)), nil
	}
	leads := s.book.Filter(in.From, in.To, in.Stage)
	results := make([]AccountRecord, len(leads))
	for i, l := range leads {
		results[i] = AccountRecord{Properties: AccountProperties{Name: l.Label, Stage: l.Stage}}
	}
	return jsonResult(SearchAccountsEnvelope{Results: results, Total: len(results), Paging: AccountPaging{}})
}

// jsonResult marshals v into the single TextContent block every leadflow
// tool returns. pkg/web/uibindings/tool's unwrapResult unmarshals the response
// into a Go string and, when that string is itself valid JSON, hands the
// raw JSON to the bound prop — a structured CallToolResult with no text
// content would resolve to a prop value the renderer cannot use.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		// An encoding failure is this package's own bug, not a bad caller
		// input — still surfaced as a tool-level error, never swallowed.
		return errResult(fmt.Sprintf("encode result: %v", err)), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// errResult builds a structured MCP tool-level error: IsError true with the
// message in Content, per the go-sdk's own guidance (errors that originate
// from the tool belong in Content, not as a protocol-level error, so the
// model/caller can see what happened) — never a bare, empty success.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
