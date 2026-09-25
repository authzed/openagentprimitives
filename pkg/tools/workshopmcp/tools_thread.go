// tools_thread.go implements `agents_in_thread` (announced with the
// workshop toolbox's `workshop_` prefix as `workshop_agents_in_thread`): the
// sidecar half of plan 9a's reproduce-lookup route
// (pkg/web/workshopthreadsrv, Task 1). A builder session freshly pulled
// into a conversation another agent already worked has no way, from its own
// state alone, to tell who else has already been in that thread — this
// tool is that lookup, and it answers for the calling builder's OWN
// conversation only, never any other thread or namespace.
package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolAgentsInThread is the name `agents_in_thread` announces on the MCP
// surface (`workshop_agents_in_thread` once the runner applies this
// toolbox's prefix).
const toolAgentsInThread = "agents_in_thread"

// agentInThread mirrors pkg/web/workshopthreadsrv.AgentInThread field for
// field — that route's 200 body is a JSON array of exactly this shape.
// Declared locally rather than importing the operator-side package: this
// sidecar binary decodes the wire shape as its own type, the same boundary
// transcript.go's toolRecord and export.go's storeDraft response struct
// already keep between this process and pkg/web.
type agentInThread struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Class     string `json:"class"`
	Role      string `json:"role"`
}

// registerThread wires `agents_in_thread` onto mcpSrv. Read-only and auto —
// that policy is declared on this sidecar's SidecarToolbox entry, not
// decided here.
func (s *Server) registerThread(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolAgentsInThread,
		Description: "List the other agents connected to THIS conversation thread, each as " +
			"{namespace, name, class, role} — role is \"participant\" (bound to this same thread " +
			"directly — solid evidence it was here) or \"descendant\" (a subagent that participant has " +
			"delegated to at some point in its whole delegation history, not scoped to this " +
			"conversation — weaker evidence: useful for seeing what an agent is capable of delegating " +
			"to, not proof it acted here). Scoped to this one conversation only — it never lists agents " +
			"at large, only who is connected to the thread you were just pulled into. These names are " +
			"for understanding what already happened here, not a roster to delegate new work to. Call " +
			"it when you are picking up an existing conversation, before assuming you are the first " +
			"agent to see it.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleAgentsInThread)
}

// handleAgentsInThread answers the `agents_in_thread` tool call.
func (s *Server) handleAgentsInThread(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agents, err := s.fetchAgentsInThread(ctx)
	if err != nil {
		return s.toolErr("agents_in_thread: %v", err), nil
	}
	return s.jsonResult(map[string]any{"agents": agents})
}

// fetchAgentsInThread GETs the operator's tuple-authorized reproduce-lookup
// route (pkg/web/workshopthreadsrv, GET
// {OPERATOR_MEMORY_URL}/workshop/agents-in-thread — no path parameter; the
// route derives the caller's own thread entirely from the bearer it
// authenticates) and returns its decoded body.
//
// A plain http.Client, not pkg/x/safehttp's SSRF-guarded one: that guard
// polices a URL a MODEL hands in as a tool argument, which is what
// probe_mcp's target is. OPERATOR_MEMORY_URL is never a tool argument or
// otherwise model-controlled — it is the fixed operator address the
// controller injects into this sidecar's own environment at pod-create
// time, the same value readChildToolResults (transcript.go) and storeDraft
// (export.go) already reach through for the identical reason.
func (s *Server) fetchAgentsInThread(ctx context.Context) ([]agentInThread, error) {
	memURL := os.Getenv("OPERATOR_MEMORY_URL")
	bearer := os.Getenv("token")
	if memURL == "" || bearer == "" {
		return nil, fmt.Errorf("fetchAgentsInThread: OPERATOR_MEMORY_URL and token (the operator bearer) are both required; got url=%q tokenSet=%v", memURL, bearer != "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(memURL, "/")+"/workshop/agents-in-thread", nil)
	if err != nil {
		return nil, fmt.Errorf("fetchAgentsInThread: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchAgentsInThread: GET %s/workshop/agents-in-thread: %w", memURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The operator's own error text is safe to surface: workshopthreadsrv
		// never echoes another session's data in a denial, only structural
		// denial reasons (unauthorized bearer, workshop not Ready, tuple false).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetchAgentsInThread: operator refused the lookup (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var agents []agentInThread
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return nil, fmt.Errorf("fetchAgentsInThread: decode response: %w", err)
	}
	return agents, nil
}
