// tools_project.go implements `project_agent` (announced with the workshop
// toolbox's `workshop_` prefix as `workshop_project_agent`): the sidecar half
// of plan 9b's stand-in-projection route (pkg/web/workshopprojectsrv, Task
// 1). A builder that was pulled into an existing conversation — see
// `agents_in_thread` (tools_thread.go) for how it learns who else is there —
// may want the agent it is building to hand work off to one of those other
// agents. Rehearsing that hand-off must never invoke the real agent (its
// real credentials, tools, or authz policy); this tool is what lets the
// builder ask the operator to project a credential-free PRACTICE DOUBLE of
// that other agent's class into its own workshop instead, so the hand-off
// itself — not the real agent's work — can be rehearsed.
//
// New reach, stated plainly: the double carries the source class's own
// SystemPrompt into the workshop namespace, where this sidecar has full
// CRUD on AgentClass — so `workshop_get`/`workshop_list` read another
// agent's instructions back to the builder verbatim, something the sidecar
// otherwise has no access to at all.
//
// WHAT BOUNDS THAT REACH IS THE SERVER, NOT THE CARD. stateImpact: external
// on this tool keys `workshop_draft:draft#project` (see the workshop
// SidecarToolbox manifest), so the approval a human gives is per PHASE and
// names the DRAFT: one approval covers every projection in that phase, and
// the card never shows the {namespace, name} a given call will actually
// project. The per-target bound is enforced where the projection happens —
// pkg/web/workshopprojectsrv refuses any class not reachable from this
// conversation's own agent set, the exact set this builder's
// `agents_in_thread` already returned it. That refusal, not the card, is the
// control; the card is what makes the REACH visible to the person, and this
// tool's own Description says what the reach is.
package workshopmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolProjectAgent is the name `project_agent` announces on the MCP surface
// (`workshop_project_agent` once the runner applies this toolbox's prefix).
const toolProjectAgent = "project_agent"

// registerProject wires `project_agent` onto mcpSrv.
func (s *Server) registerProject(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolProjectAgent,
		Description: "Create a credential-free PRACTICE DOUBLE of another agent's class, so a hand-off " +
			"to it can be rehearsed. This does NOT summon or run the real agent: the double shares its " +
			"system prompt and whichever of its skills resolve in this workshop (one that doesn't is " +
			"dropped, and named in the double's own description), but holds no credentials, no tools, and " +
			"no authorization policy of its own, so it cannot do the real agent's actual work — it exists " +
			"only so you can rehearse the hand-off itself (what you would send it, what it says back), " +
			"never to get real work done through it. IMPORTANT: because the double carries the real " +
			"agent's own system prompt verbatim, this tool hands YOUR workshop the other agent's own " +
			"instructions to read — get/list on the projected double return them in full — which is new " +
			"reach beyond a name to delegate to, not merely a label. It only works for an agent that " +
			"agents_in_thread reports for this conversation — " +
			"call that first and choose from what it returns; naming a class outside that set is refused. " +
			"Note that set includes agents a participant delegated to at some point, not only agents that " +
			"acted here, so it reports who is REACHABLE rather than who certainly took part. On success it returns the double's own name, identical to the real agent's own name " +
			"— add that name to your own roster exactly as you would the real one. Calling it again for the " +
			"same source class updates the same double in place rather than creating a second one.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespace": map[string]any{
					"type":        "string",
					"description": "The source agent class's own namespace — the same Namespace field an agents_in_thread row reports.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "The source agent class's own name — the same Class field an agents_in_thread row reports.",
				},
			},
			"required": []any{"namespace", "name"},
		},
	}, s.handleProjectAgent)
}

// projectAgentArgs is `project_agent`'s own argument shape — field for field
// the same {namespace, name} pair pkg/web/workshopprojectsrv's projectRequest
// decodes, so it can be marshaled straight through as that route's request
// body.
type projectAgentArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// projectAgentResult mirrors workshopprojectsrv's projectResponse field for
// field — that route's 200 body is exactly this shape. Declared locally
// rather than importing the operator-side package, the same wire-shape
// boundary agentInThread (tools_thread.go) already keeps between this
// sidecar process and pkg/web.
type projectAgentResult struct {
	Name string `json:"name"`
}

// handleProjectAgent answers the `project_agent` tool call.
func (s *Server) handleProjectAgent(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a projectAgentArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("project_agent: decode arguments: %v", err), nil
	}
	if a.Namespace == "" || a.Name == "" {
		return s.toolErr("project_agent: namespace and name are both required"), nil
	}

	name, err := s.projectAgent(ctx, a.Namespace, a.Name)
	if err != nil {
		return s.toolErr("project_agent: %v", err), nil
	}
	return s.jsonResult(map[string]any{"name": name, "status": "projected"})
}

// projectAgent POSTs {namespace, name} to the operator's stand-in-projection
// route (pkg/web/workshopprojectsrv, POST
// {OPERATOR_MEMORY_URL}/workshop/project-agent) and returns the projected
// stand-in's own name.
//
// A plain http.Client, not pkg/x/safehttp's SSRF-guarded one: that guard
// polices a URL a MODEL hands in as a tool argument, which is what
// probe_mcp's target is. OPERATOR_MEMORY_URL is never a tool argument or
// otherwise model-controlled — it is the fixed operator address the
// controller injects into this sidecar's own environment at pod-create time,
// the same value fetchAgentsInThread (tools_thread.go) and storeDraft
// (export.go) already reach through for the identical reason.
//
// Every non-200 response is surfaced as an error rather than decoded as a
// success: the route's own refusal text (a class not reachable from this
// builder's thread, a name collision with a class the builder authored,
// a malformed request) is safe to pass through verbatim — workshopprojectsrv
// never echoes another session's data in a denial, only structural denial
// reasons — and a caller told "done" when nothing was projected would go on
// to rehearse against a stand-in that was never created.
func (s *Server) projectAgent(ctx context.Context, namespace, name string) (string, error) {
	memURL := os.Getenv("OPERATOR_MEMORY_URL")
	bearer := os.Getenv("token")
	if memURL == "" || bearer == "" {
		return "", fmt.Errorf("OPERATOR_MEMORY_URL and token (the operator bearer) are both required; got url=%q tokenSet=%v", memURL, bearer != "")
	}

	reqBody, err := json.Marshal(projectAgentArgs{Namespace: namespace, Name: name})
	if err != nil {
		return "", fmt.Errorf("encode request body: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(memURL, "/")+"/workshop/project-agent", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("POST %s/workshop/project-agent: %w", memURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("operator refused the projection (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out projectAgentResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if out.Name == "" {
		return "", fmt.Errorf("operator reported success but named no stand-in")
	}
	return out.Name, nil
}
