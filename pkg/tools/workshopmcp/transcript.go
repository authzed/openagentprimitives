// transcript.go: readChildToolResults, the sidecar half of the plan 4a
// tuple-authorized child-transcript read route (Task 2, pkg/web/workshoptranscriptsrv).
// test_tool and read_test_log call this to see what a session DID — its tool
// calls and their results — never what it SAID, plus the signed transitions
// that say what happened TO it when it did nothing at all.
package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// toolRecord is the prose-free view of one transcript block the test tools
// return to the builder: a tool CALL (the tool + its args) or its RESULT
// (the output, and whether it errored). The route's contract is "the
// child's tool-result records ... never its prose", so text/attachment
// blocks are dropped entirely by toolResultsOnly and never reach this type.
type toolRecord struct {
	Kind    string          `json:"kind"` // "call" | "result"
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Result  string          `json:"result,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

// auditEntry is one entry of the session's own signed log, as the operator's
// transcript route returns it: when it was recorded, which transition it was,
// and that transition's payload. Passed through verbatim — this is evidence
// about a run, and re-wording it here would put the sidecar between a reader
// and what actually happened.
type auditEntry struct {
	At      string          `json:"at"`
	Type    string          `json:"type"`
	Details json.RawMessage `json:"details,omitempty"`
}

// childLog is everything the operator's transcript route returns about one
// session: what it DID (tool calls and results, prose stripped) and what
// HAPPENED TO IT (its signed transitions). The second list is why this is a
// struct rather than a bare slice — a session that failed before calling
// anything has no records at all, and Records alone reads as "nothing
// happened" for a run that in fact failed for a nameable reason.
type childLog struct {
	Records []toolRecord
	Audit   []auditEntry
}

// toolResultsOnly is the pure filter: it walks every turn's content blocks
// in transcript order and keeps only tool_use (as Kind:"call") and
// tool_result (as Kind:"result", errors included) blocks. text and
// attachment blocks are dropped unconditionally — a builder session must
// never see a child's prose, only what it did.
func toolResultsOnly(turns []memory.Turn) []toolRecord {
	var out []toolRecord
	for _, t := range turns {
		for _, b := range t.Content {
			switch b.Type {
			case "tool_use":
				if b.ToolUse != nil {
					out = append(out, toolRecord{Kind: "call", Tool: b.ToolUse.Name, Input: json.RawMessage(b.ToolUse.Input)})
				}
			case "tool_result":
				if b.ToolResult != nil {
					out = append(out, toolRecord{Kind: "result", Result: b.ToolResult.Content, IsError: b.ToolResult.IsError})
				}
			} // text, attachment: dropped — never returned
		}
	}
	return out
}

// readChildToolResults GETs the child's transcript from the operator's
// tuple-authorized route (pkg/web/workshoptranscriptsrv, GET
// {OPERATOR_MEMORY_URL}/workshop/transcript/{child}) and returns its
// tool-call/tool-result records — prose and attachments stripped — alongside
// the session's own signed transitions.
//
// A plain http.Client, not pkg/x/safehttp's SSRF-guarded one — mirrors
// storeDraft's rationale in export.go: that guard exists for probe_mcp's
// target, a URL a MODEL supplies as a tool argument. OPERATOR_MEMORY_URL is
// neither model-supplied nor a tool argument — it is the same operator
// address this process already reaches through s.Ops and storeDraft, via
// the identical operator-injected env vars.
func (s *Server) readChildToolResults(ctx context.Context, child string) (childLog, error) {
	memURL := os.Getenv("OPERATOR_MEMORY_URL")
	bearer := os.Getenv("token")
	if memURL == "" || bearer == "" {
		return childLog{}, fmt.Errorf("readChildToolResults: OPERATOR_MEMORY_URL and token (the operator bearer) are both required; got url=%q tokenSet=%v", memURL, bearer != "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(memURL, "/")+"/workshop/transcript/"+url.PathEscape(child), nil)
	if err != nil {
		return childLog{}, fmt.Errorf("readChildToolResults: build request for %q: %w", child, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return childLog{}, fmt.Errorf("readChildToolResults: GET %s/workshop/transcript/%s: %w", memURL, child, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The operator's own error text (its 4xx/5xx body) is safe to surface:
		// workshoptranscriptsrv never echoes transcript content in a denial, only
		// structural denial reasons (unauthorized, not-Ready workshop, tuple false).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return childLog{}, fmt.Errorf("readChildToolResults: operator refused the transcript read for %q (status %d): %s", child, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Turns        []memory.Turn `json:"turns"`
		AuditEntries []auditEntry  `json:"auditEntries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return childLog{}, fmt.Errorf("readChildToolResults: decode response for %q: %w", child, err)
	}
	return childLog{Records: toolResultsOnly(payload.Turns), Audit: payload.AuditEntries}, nil
}
