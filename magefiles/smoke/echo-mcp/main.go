// Command echo-mcp is a tiny MCP-over-HTTP (Streamable HTTP) server used
// ONLY by the kind-cluster smoke test (mage test:smoke). It is the real
// sidecar container injected into an AgentSession runner Pod so the smoke
// can prove the in-pod loopback MCP path end-to-end without depending on
// Python in the build pipeline.
//
// It implements the minimal JSON-RPC 2.0 surface the platform's probe +
// dispatch code expects:
//
//   - GET  /healthz          → 200 (the SidecarToolbox startup/readiness probe)
//   - POST /                 → JSON-RPC 2.0:
//   - initialize  → {"result":{"protocolVersion","capabilities","serverInfo"}}
//   - tools/list  → {"result":{"tools":[{name,description,inputSchema}]}}
//     (shape consumed by pkg/tools/mcp/probe.Client.ListTools)
//   - tools/call (echo) → {"result":{"content":[{"type":"text","text":...}],"isError":false}}
//     (shape decoded by pkg/agent/tool/mcp.(*MCPTool).Execute)
//
// The bind port is read from MCP_PORT (the operator sets MCP_PORT in the
// sidecar container env regardless of the spec's advisory transport.port).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// rpcRequest is the subset of a JSON-RPC 2.0 request we decode. id is kept
// as raw JSON so we echo back exactly what the caller sent (number or
// string), matching the spec.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// callParams is the params shape for a tools/call request.
type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

const (
	echoToolName   = "echo"
	whoamiToolName = "whoami"
)

// smokeSecretEnvVar is the env var name delivered by the secret-gated
// SidecarToolbox smoke fixture (secretInputs[0].name). The value is injected
// by the operator from the per-session secret-output Secret via deliver:env.
const smokeSecretEnvVar = "SMOKE_SECRET"

// echoInputSchema is the JSON Schema advertised for the echo tool. The
// platform preserves inputSchema as raw JSON (pkg/tools/mcp/probe.Tool), so any
// valid JSON Schema object is acceptable; this declares a single required
// string field "text".
var echoInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": { "type": "string", "description": "Text to echo back verbatim." }
  },
  "required": ["text"],
  "additionalProperties": false
}`)

// whoamiInputSchema is the JSON Schema for the whoami tool. It accepts no
// arguments; the sidecar reads its secretInput from the SMOKE_SECRET env var
// (set by the operator from the per-session secret-output Secret) and returns
// only the sha256 hex fingerprint — never the value itself.
var whoamiInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// secretFingerprint returns the lowercase hex sha256 of the value in the
// SMOKE_SECRET env var, or an empty-input fingerprint when the var is unset.
// The fingerprint is safe to include in tool results — it is a hash, never
// the value itself, so the smoke assertion can match it without leaking the
// secret into any LLM-visible byte.
func secretFingerprint() string {
	val := os.Getenv(smokeSecretEnvVar)
	sum := sha256.Sum256([]byte(val))
	return hex.EncodeToString(sum[:])
}

func main() {
	port := os.Getenv("MCP_PORT")
	if port == "" {
		log.Fatal("echo-mcp: MCP_PORT must be set")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", handleRPC)

	addr := ":" + port
	log.Printf("echo-mcp: listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("echo-mcp: ListenAndServe: %v", err)
	}
}

func handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "echo-mcp", "version": "1"},
		})
	case "notifications/initialized":
		// Notification (no id): the MCP client sends this after initialize.
		// JSON-RPC notifications get no response body — just acknowledge.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeResult(w, req.ID, map[string]any{
			"tools": []map[string]any{
				{
					"name":        echoToolName,
					"description": "Echoes its input text verbatim.",
					"inputSchema": echoInputSchema,
				},
				{
					"name":        whoamiToolName,
					"description": "Returns the sha256 fingerprint of the secret delivered via secretInput (SMOKE_SECRET env var). Safe: the fingerprint is a hash, never the secret value.",
					"inputSchema": whoamiInputSchema,
				},
			},
		})
	case "tools/call":
		handleToolsCall(w, req)
	default:
		writeError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func handleToolsCall(w http.ResponseWriter, req rpcRequest) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	switch p.Name {
	case echoToolName:
		text, _ := p.Arguments["text"].(string)
		writeResult(w, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": text,
			}},
			"isError": false,
		})
	case whoamiToolName:
		// Return only the fingerprint — never the value — so the smoke test can
		// assert delivery without leaking the secret into any log or LLM message.
		writeResult(w, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf(`{"secret_fingerprint":%q}`, secretFingerprint()),
			}},
			"isError": false,
		})
	default:
		// Surface as an in-band tool error (isError=true) rather than a
		// JSON-RPC protocol error so dispatch.go maps it to a tool Result.
		writeResult(w, req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("unknown tool: %q", p.Name),
			}},
			"isError": true,
		})
	}
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}
