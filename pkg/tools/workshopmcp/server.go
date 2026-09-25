// pkg/controllers/agentsession/sidecarpod.go's identity branch is the only
// place a workshop sidecar gets its two controller-issued tokens: the
// projected SA token (mounted at
// /var/run/secrets/workshop/serviceaccount/token, RBAC-bounded to the
// workshop namespace W) and the operator bearer (env var "token", from the
// <session>-workshop-token Secret). server.go is where those two tokens land
// as the Server this binary's tools run against — Task 1 wires the struct and
// the shared error helpers; Task 2 onward add the tools themselves via their
// own register* functions called from Register.
package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// defaultFieldOwner is the stable SSA field manager every workshop_apply call
// uses (Task 3). Stable across restarts and across every tool call in this
// process, so a byte-identical re-apply of the same candidate CR stays an SSA
// no-op regardless of which sidecar pod incarnation applied it first.
const defaultFieldOwner = "workshop-sidecar"

// WorkshopIdentity is which workshop namespace / builder session this sidecar
// process is scoped to, resolved once at startup from the env
// BuildSidecarPod's identity branch injects. Namespace is fixed for the
// process's life, but SessionNamespace/SessionName are late-bound in the
// one caller that needs it (the e2e harness, before any real session
// exists) — see Server.sessionRef(), not this struct's own fields, for
// what a caller should actually read.
type WorkshopIdentity struct {
	// Namespace is the provisioned workshop namespace W ("ws-<uid12>") this
	// sidecar's SA token is RBAC-bounded to — every CR the sidecar reads or
	// writes lives here.
	Namespace string
	// SessionNamespace/SessionName identify the BUILDER AgentSession (B/X)
	// that owns this workshop — distinct from Namespace, which is the
	// workshop's own namespace, not the builder session's.
	SessionNamespace string
	SessionName      string
	// WorkshopID is the workshop identifier the platform env carries
	// (WORKSHOP_ID) — equal to Namespace today (both are W), carried as its
	// own field because the two happen to share a value, not because they
	// are the same concept.
	WorkshopID string
}

// Server holds this sidecar's two controller-issued clients — K8s (the
// SA-token-bound, RBAC-bounded controller-runtime client) and Ops (the
// operator-bearer memory client) — plus the workshop identity, and registers
// every workshop build-tool onto an mcp.Server via Register. Task 1 wires
// the struct with no tools; each later task adds its own registerX call
// inside Register.
type Server struct {
	// K8s is the SA-token-bound controller-runtime client, RBAC-scoped to the
	// workshop namespace Identity.Namespace by the apiserver — the sidecar
	// itself never re-checks a CR's namespace before acting.
	K8s client.Client
	// Ops is the operator-bearer memory client this sidecar reaches the
	// operator's HTTP memory routes (and, from Task 6, the export-draft
	// route) through.
	Ops memory.Memory
	// Identity is this sidecar's workshop identity.
	Identity WorkshopIdentity
	// FieldOwner is the stable SSA field manager every workshop_apply call
	// uses. Defaults to "workshop-sidecar" (defaultFieldOwner) via NewServer.
	FieldOwner string
	// ProbeHTTP overrides the HTTP client probe_mcp's tools/list call uses.
	// Nil in production (and via NewServer) — probe.Client treats a nil
	// HTTP field as "use the SSRF-guarded default"
	// (pkg/tools/mcp/probe.Client.httpClient, backed by safehttp.Client()),
	// which is the correct behavior here: the probed URL is model-supplied.
	// Tests set this to a loopback-permitting client (e.g. http.DefaultClient)
	// to reach an httptest fake MCP server.
	ProbeHTTP *http.Client

	// sessionMu guards sessionNamespace/sessionName/sessionBound below — the
	// two WorkshopIdentity fields (SessionNamespace, SessionName) a caller may
	// late-bind AFTER construction, via SetBuilderSession, once it actually
	// knows the builder AgentSession's real name. Every other identity field
	// (Namespace, WorkshopID) is fixed for the life of a Server and read
	// directly off Identity, as before this existed.
	//
	// This exists for exactly one caller: test/e2e/workshop_mcp.go's
	// mountWorkshopMCP has to construct the real Server before any
	// AgentSession exists (there is nothing else to hand WorkshopIdentity at
	// that point), so it freezes SessionNamespace/SessionName to a
	// placeholder; test/e2e/threadrun's driver calls SetBuilderSession once
	// the harness's pipeline has actually created the session. By then the
	// Server is already SERVING other tool calls on other goroutines (the
	// scripted transcript can call a Namespace-only tool like workshop_apply
	// before the identity-dependent turn arrives), so the two fields cannot
	// be plain struct fields: a bare write on one goroutine racing a bare
	// read on another is a data race regardless of whether the two calls are
	// logically ordered.
	//
	// An RWMutex, not atomic.Pointer[WorkshopIdentity]: SetBuilderSession is
	// called at most once per Server (the driver binds it once, right after
	// the session is confirmed to exist), so write contention is not a
	// concern — the guard exists purely to make the read and the write
	// visible to each other (and to the race detector), never to serialize
	// concurrent readers against each other. sessionRef only ever holds the
	// read lock long enough to copy two strings; SetBuilderSession only ever
	// holds the write lock long enough to set them — neither is ever held
	// across a K8s request.
	// declarationOnly refuses every tools/call (set ONLY by NewDeclarationOnly;
	// see its doc for the admission-probe contract).
	declarationOnly bool

	sessionMu                     sync.RWMutex
	sessionNamespace, sessionName string
	sessionBound                  bool
}

// sessionRef returns the (namespace, name) of the builder AgentSession these
// tools key their Workshop-CR / parent-session lookups on: request_credential,
// request_install, recommend_capability, export_draft's registry read, and
// watch_test's/test_sessions' Workshop CR reads all Get the session's own
// Workshop CR at {ns, WorkshopName(name)}; test_tool sets it as its child
// SubagentRequest's Parent.
//
// Returns the driver-bound values once SetBuilderSession has been called (the
// e2e harness's only path to a real answer here — see mountWorkshopMCP's own
// doc), or falls back to WorkshopIdentity's own construction-time
// SessionNamespace/SessionName otherwise — unit tests that build a Server
// literal (or a future production sidecar-pod identity that sets Identity
// correctly at construction and never calls SetBuilderSession) keep working
// exactly as they did before this method existed.
func (s *Server) sessionRef() (namespace, name string) {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	if s.sessionBound {
		return s.sessionNamespace, s.sessionName
	}
	return s.Identity.SessionNamespace, s.Identity.SessionName
}

// SetBuilderSession late-binds the builder AgentSession's real (namespace,
// name) onto an already-mounted Server. See sessionMu's own doc for why this
// exists at all (WorkshopIdentity is frozen at construction, before any
// session exists, in the one caller that needs this) and for the concurrency
// guard — safe to call while the Server is already serving other tool calls.
func (s *Server) SetBuilderSession(namespace, name string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.sessionNamespace = namespace
	s.sessionName = name
	s.sessionBound = true
}

// NewServer builds a Server from its two clients and its workshop identity.
// fieldOwner defaults to defaultFieldOwner when empty.
func NewServer(k8s client.Client, ops memory.Memory, identity WorkshopIdentity, fieldOwner string) *Server {
	if fieldOwner == "" {
		fieldOwner = defaultFieldOwner
	}
	return &Server{K8s: k8s, Ops: ops, Identity: identity, FieldOwner: fieldOwner}
}

// NewDeclarationOnly builds a Server that serves ONLY tool declarations: it
// registers the full tool surface (tools/list answers exactly what a wired
// server announces) and refuses every tools/call via receiving middleware.
//
// This is the admission-probe mode: buildProbePod
// (pkg/controllers/sidecartoolbox/probe.go) runs the image with no projected
// SA token, no operator bearer and no WORKSHOP_* identity, and only ever asks
// tools/list — so the image must be able to DESCRIBE itself without an
// identity while remaining unable to ACT without one. Declaration-only is an
// EXPLICIT choice by the binary's boot path (main.go's probe-mode predicate),
// never inferred from a nil client: a mis-wired production sidecar must
// crash, not silently degrade into a server that refuses everything.
func NewDeclarationOnly() *Server {
	return &Server{declarationOnly: true, FieldOwner: defaultFieldOwner}
}

// Register wires every workshop build-tool onto mcpSrv. Task 1 registers
// none — each later task (inventory, apply/CRUD/render, validate, probes,
// export/load, test) adds its own registerX(mcpSrv) call here, so this
// function is the one place that lists the sidecar's full tool surface.
func (s *Server) Register(mcpSrv *mcp.Server) {
	s.registerInventory(mcpSrv)
	s.registerApply(mcpSrv)
	s.registerCRUD(mcpSrv)
	s.registerRender(mcpSrv)
	s.registerValidate(mcpSrv)
	s.registerProbes(mcpSrv)
	s.registerExport(mcpSrv)
	s.registerTests(mcpSrv)
	s.registerTryTest(mcpSrv)
	s.registerCredential(mcpSrv)
	s.registerHandoff(mcpSrv)
	s.registerCloseOthers(mcpSrv)
	s.registerThread(mcpSrv)
	s.registerProject(mcpSrv)

	// Declaration-only: refuse every tools/call at ONE choke point, before any
	// handler runs. Middleware rather than per-handler guards — the handlers
	// hold nil clients in this mode, and 21 hand-written nil checks is the
	// shape that drifts the first time a tool is added. tools/list (and every
	// other method) passes through untouched: announcing the surface is the
	// entire point of the mode.
	if s.declarationOnly {
		mcpSrv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/call" {
					return s.toolErr("workshop sidecar is running without a workshop identity (declaration-only admission-probe mode): tool calls are refused, only tool declarations are served"), nil
				}
				return next(ctx, method, req)
			}
		})
	}
}

// toolErr builds a structured IsError CallToolResult carrying
// {"error": "<formatted message>"} — the shape every workshop tool failure
// uses, so a caller can rely on one field name regardless of which tool or
// which failure produced it.
func (s *Server) toolErr(format string, a ...any) *mcp.CallToolResult {
	return structuredErrorResult(map[string]any{"error": fmt.Sprintf(format, a...)})
}

// deniedResult surfaces an apiserver/webhook denial VERBATIM as a structured
// {"denied": true, "message": "..."} IsError result — never re-worded. Per
// the sidecar's fail-closed contract (design spec §13): every CR read/write
// this sidecar does is bounded by the apiserver via the SA token's RBAC; a
// call outside its bounds is denied by the apiserver, and the sidecar does
// not re-check, it surfaces the denial exactly as the apiserver phrased it.
func (s *Server) deniedResult(err error) *mcp.CallToolResult {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return structuredErrorResult(map[string]any{"denied": true, "message": msg})
}

// structuredErrorResult marshals body into a single-text-block IsError
// CallToolResult. body is always a map of strings/bools in this file, so
// json.Marshal cannot fail in practice; the fallback exists so a future
// caller passing an unmarshalable value still surfaces SOMETHING instead of
// silently losing the error (see CLAUDE.md: never silently drop an error).
func structuredErrorResult(body map[string]any) *mcp.CallToolResult {
	b, err := json.Marshal(body)
	if err != nil {
		b = fmt.Appendf(nil, "%q", body)
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}
}

// jsonResult marshals body into a single-text-block SUCCESSFUL CallToolResult
// — the shared success-path counterpart to structuredErrorResult/toolErr,
// used by every workshop tool that answers with a JSON body. body is always
// a plain map/slice/struct of JSON-safe values at every call site in this
// binary, so json.Marshal cannot fail in practice — but per CLAUDE.md's
// never-silently-drop-an-error rule, a hypothetical failure still surfaces as
// a structured tool error rather than an empty or malformed result.
func (s *Server) jsonResult(body any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return s.toolErr("marshaling result: %v", err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

// decodeArgs decodes req's raw MCP tool-call arguments into out. This
// sidecar is a LEAF MCP server: the operation_id/_reason/args envelope a
// SidecarToolbox-sourced tool sees on the LLM-facing schema
// (agenttool.WrapInputSchema) is unwrapped by the runner's MCP dispatcher
// BEFORE the call ever reaches here (pkg/agent/tool/mcp/dispatch.go forwards
// only the inner `args` object over the wire) — exactly like every other
// downstream MCP server's tools.InputSchema (see e.g.
// pkg/web/uidemo/leadflow/tools.go). So req.Params.Arguments is this tool's
// own flat argument shape, never the envelope; a nil/empty Arguments (a tool
// called with no args at all) leaves out at its zero value rather than
// erroring, mirroring encoding/json's own treatment of a missing field.
func decodeArgs(req *mcp.CallToolRequest, out any) error {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, out)
}
