// Package mcp — MCPTool struct + method set, separated from synthesize.go so
// the synthesizer file stays narrowly focused on the spec→tool.Tool pipeline.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcprender "github.com/authzed/openagentprimitives/pkg/tools/mcp/render"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
)

// MCPTool is one allowlisted tool synthesized from an MCPServer CR.
type MCPTool struct {
	serverName  string // LLM prefix (AgentClass.MCPServers[].Name); used for llmName
	originName  string // MCPServer CR name (metadata.name); the revocation key, distinct from serverName/LLM-prefix
	toolName    string // server-side name
	llmName     string // <serverName>_<toolName>, normalized
	description string

	url string
	// authMu guards header+value: Execute mutates them on a reauth retry,
	// and the runner dispatches tool calls in parallel goroutines that may
	// share one *MCPTool instance (loop.go). Read via currentAuth, write
	// via setAuthLocked.
	authMu sync.Mutex
	header string
	value  string
	// authStale marks (header, value) as revoked: a credential revocation
	// arrived on ap.revocation and the frozen token must NOT be sent again.
	// Execute re-resolves via reauth before its first send and fails closed if
	// it cannot. Cleared by setAuthLocked once a fresh credential is attached.
	authStale bool
	// reauthMu serializes the stale-credential re-resolve so that N parallel
	// tool-call goroutines sharing this *MCPTool (loop.go dispatches
	// concurrently) perform exactly one reauth between them. It is NOT held
	// across a send — only across the check-reauth-store sequence.
	reauthMu sync.Mutex
	// reauth re-resolves (header, value) from the operator-refreshed
	// credential; invoked once by Execute on a 401 to survive a
	// mid-session OAuth token rotation. Nil disables the retry. Set once
	// at session start (before dispatch), then read-only.
	reauth             func(ctx context.Context) (header, value string, err error)
	timeout            time.Duration
	args               mcpspec.Args
	httpClient         *http.Client
	inputSchema        json.RawMessage
	permission         authz.Permission
	permissionVariants []authz.PermissionVariant

	// readOnlyHint is the server-declared readOnlyHint (Effects.ReadOnly) for
	// this tool. The MCP-UI app-tool readonly gate (D-B3) auto-runs a widget's
	// app-tool call only when the human-declared StateImpact is Readonly AND
	// this server hint agrees; a mismatch demotes to approval. Harmless on
	// LLM-visible tools, which never flow through that gate.
	readOnlyHint bool

	// spec is a back-pointer to the full MCP spec this tool was
	// synthesized from. Used by Execute to drive the validator.
	// Populated by Synthesize; nil-safe (a defensive fallback inside
	// Execute returns an IsError if absent).
	spec *mcpspec.Spec

	// writesRelationships are the JIT relwrites blocks declared by
	// the MCPServer CR for this tool. Evaluated after Execute returns
	// a SUCCESSFUL tool-call envelope (see slice-4 design §2.3).
	writesRelationships []spiceboxv1alpha1.MCPServerRelationshipWrite
	// relWriter is the SpiceDB write surface. Nil-safe: when unset,
	// the post-effect is skipped silently. Wired by the runner at
	// session start (see internal/cmd/runner T15).
	relWriter relwrites.Writer
	// slotBoundChecker answers, per resolved tuple, whether this session holds
	// a slot grant on the tuple's resource. Consulted ONLY for a block whose
	// RequireSlotBound is true. Wired by the runner at session start alongside
	// relWriter; nil is NOT a bypass — relwrites.Run refuses every tuple of a
	// marked block when it is nil, so a missed wiring surfaces as a loud
	// refusal rather than an unchecked write.
	slotBoundChecker relwrites.SlotBoundChecker
	// logger is used to report post-effect failures without changing
	// the returned tool.Result. Falls back to slog.Default() when nil.
	logger *slog.Logger

	// observes are the declared `observes` blocks from the MCPServer CR
	// for this tool. Evaluated after Execute returns a SUCCESSFUL
	// tool-call envelope, alongside writesRelationships — same
	// (args, result, item) CEL bindings, same "runs only on success"
	// contract, but this emits memory facts (observedfact) rather than
	// SpiceDB tuples.
	observes []spiceboxv1alpha1.ObservesBlock

	// labels are the per-tool labels CEL blocks declared by the
	// MCPServer CR. Evaluated after Execute returns a SUCCESSFUL
	// tool-call envelope; failures are non-fatal.
	labels []spiceboxv1alpha1.MCPServerLabelExtract
	// labelSink receives extracted (resourceType, id, name) tuples.
	// Nil-safe: when unset, the post-effect is skipped.
	labelSink LabelSink

	// mem is the in-process memory framework for forensic audit writes.
	// Nil-safe: when unset, relwrites_audit recording is skipped.
	mem memorypkg.Memory

	// useTokenGate optionally gates every dispatch call behind a
	// fully-consistent SpiceDB use_token check, run immediately before the
	// resolved credential is sent upstream (dispatch.go). Nil disables the
	// check — e.g. a tool with no injected auth credential has nothing to
	// check. Guarded by authMu: set once by the runner at session start
	// (mirroring SetAuth), read on every concurrent Execute.
	useTokenGate *UseTokenGate

	// sessionCache holds one persistent MCP session per server URL for the
	// whole AgentSession, so server-side per-session state (the dedicated-mcp
	// sidecar's PermissionSystem/cluster selection, keyed by the MCP session
	// id) survives across tool calls instead of resetting every call. Shared
	// across all MCPTools of one AgentSession (keyed internally by URL, so
	// tools on the same server share a session). Wired once by the runner at
	// session start; nil-safe — a nil cache falls back to a fresh session per
	// call (the historical behavior, still used by unit tests). Read-only after
	// wiring, so no lock is needed.
	sessionCache *probe.SessionCache
}

// TokenChecker is the SpiceDB surface the token-use gate checks against.
// *spicedb.Client satisfies it; tests use a fake. Kept as a narrow
// interface (rather than importing pkg/authz/spicedb here) so this package
// doesn't need a SpiceDB dependency to compile or unit-test.
type TokenChecker interface {
	CheckUseToken(ctx context.Context, ns, name, credID, presentedValueHash string, fullyConsistent bool) (bool, error)
}

// UseTokenGate wires the per-call durable authorization check: before every
// MCP call, dispatch.go presents a per-session-keyed hash of the credential
// value about to go upstream and asks SpiceDB whether this session may still
// use it. A definitive deny (revoked) fails only the call — the session
// continues. An indeterminate result (checker unconfigured, or the RPC
// itself errors) fails the session closed via FailSession: an unconfirmed
// authorization must never wave a call through.
type UseTokenGate struct {
	// Checker performs the CheckUseToken RPC. A nil Checker is treated as
	// unconfigured and fails closed (see dispatch.go) — it is deliberately
	// NOT validated here so a zero-value UseTokenGate is a legal (if
	// always-fail-closed) value.
	Checker TokenChecker
	// SessionNS / SessionName identify the AgentSession the check is
	// scoped to (the SpiceDB resource under check).
	SessionNS   string
	SessionName string
	// HMACKey is the per-session key used to compute the presented value
	// hash (externaltoken.ValueHash) — the same key the operator used
	// when writing the authorized_value_hash caveat context at grant time.
	HMACKey []byte
	// CredID is this tool's auth-credential identity (externaltoken.CredID
	// of its CredentialSource), precomputed once at construction — the
	// SpiceDB subject object id checked on every call.
	CredID string
	// FailSession fails the AgentSession closed on an indeterminate check
	// (e.g. wraps statusPatcher.WriteFailed). Should be non-nil whenever
	// Checker is set to a real client — a caller wiring Checker without
	// FailSession still denies the individual call, but dispatch.go can no
	// longer fail the session closed and logs an error instead of calling a
	// nil func (see dispatch.go's failSession helper).
	FailSession func(reason, msg string)
}

func (m *MCPTool) Name() string         { return m.llmName }
func (m *MCPTool) Kind() agenttool.Kind { return agenttool.KindMCP }
func (m *MCPTool) Description() string  { return m.description }

// Origin implements tool.OriginTool: all tools of one MCPServer share health.
// Keyed on the MCPServer CR name (metadata.name), NOT the LLM-prefix
// (serverName): every revocation publisher (settings, the mcpserver CR-delete
// publisher) and the runner's restart/allow filter key on the CR name, so
// Origin must match it or revocation silently never fires when the AgentClass
// LLM-prefix differs from the CR name (the documented norm: name != ref).
func (m *MCPTool) Origin() string { return "mcpserver/" + m.originName }

// PrependDescription prefixes the LLM-facing description. Used by the runner
// to surface drift warnings ("⚠ PIN DRIFT: …") when the tool's backing
// MCPServer drifted from its pinned manifest baseline.
func (m *MCPTool) PrependDescription(prefix string) {
	m.description = prefix + m.description
}

// MCPSpec returns the MCP spec associated with this tool, or nil if none was
// set at synthesis time. Used by the McpTrust hook to access the static spec
// (deny.effects, deny.trust, constraints) without re-parsing the MCPServer CR.
func (m *MCPTool) MCPSpec() *mcpspec.Spec { return m.spec }

// MCPServerToolName returns the SERVER-SIDE tool name (the key in the spec's
// allowlist), as opposed to Name() which returns the LLM-facing
// <serverName>_<toolName>. validator.Check looks the tool up in the spec by
// this name, so the McpTrust hook must pass it (not the LLM name) — mirroring
// the in-Execute validator call which uses m.toolName (dispatch.go).
func (m *MCPTool) MCPServerToolName() string { return m.toolName }

// SensitiveArgFields returns the argument paths this tool declares sensitive
// (MCPServer.spec.tools[].args.sensitiveFields). Execute already scrubs them
// out of everything the validator emits, but the pre-dispatch approval card is
// built by the runner from the RAW args — it has no Decision to read — so it
// needs the declaration itself to redact what it shows an approver, the
// summarizer LLM, and the append-only approval record.
func (m *MCPTool) SensitiveArgFields() []string { return m.args.SensitiveFields }

// InputSchema returns the LLM-facing JSON schema. We wrap the server's
// per-tool input schema inside a top-level envelope that also carries the
// operation_id + _reason audit fields, mirroring the sandbox-tool shape
// (see pkg/agent/tool/sandbox/sandbox_tool.go::InputSchema). The dispatcher
// (Execute) expects this wrapped shape; without it the LLM emits the raw
// server arguments and every call rejects with "operation_id and _reason
// are required."
func (m *MCPTool) InputSchema() json.RawMessage {
	return agenttool.WrapInputSchema(m.inputSchema)
}

// Introspect renders this MCP tool's effective contract — allowed/sensitive
// fields, constraint justifications, hard stops, writesRelationships. Satisfies
// tool.Introspectable. It passes the underlying SERVER-side tool name, not the
// prefixed llmName, so the renderer can locate the tool in spec.Tools.
func (m *MCPTool) Introspect() (string, error) {
	if m.spec == nil {
		return "", fmt.Errorf("introspect: MCP tool %q has no spec loaded", m.Name())
	}
	return mcprender.Describe(m.spec, m.toolName, mcprender.FormatMarkdown)
}

// Permission returns the authz.Permission for this MCP tool from its MCPServer CR.
func (m *MCPTool) Permission() authz.Permission {
	return m.permission
}

// ServerReadOnlyHint reports the server-declared readOnlyHint (Effects.ReadOnly)
// for this tool. The MCP-UI app-tool responder's readonly gate (D-B3) consults
// it through an optional interface: auto-run requires both a Readonly
// StateImpact and this hint agreeing.
func (m *MCPTool) ServerReadOnlyHint() bool {
	return m.readOnlyHint
}

// PermissionVariants returns the conditional Permission blocks declared
// on the MCPServer CR for this tool. The dispatcher evaluates these in
// order via authz.ResolveVariant (runner.ResolvePermissionForArgs) and
// falls back to Permission() when none match. Returns nil when no
// variants are declared.
func (m *MCPTool) PermissionVariants() []authz.PermissionVariant {
	return m.permissionVariants
}

// SetAuth is called by the runner at session start to attach the resolved
// auth header. Production code calls this; tests can also use it.
func (m *MCPTool) SetAuth(header, value string) {
	m.setAuthLocked(header, value)
}

// InvalidateAuth marks the frozen credential as revoked. The next Execute
// re-resolves before sending anything, and denies the call if it cannot.
//
// Dropping the broker's cache entry is NOT sufficient on its own: this tool
// froze (header, value) into struct fields at session start and never re-reads
// that cache, so a revoked bearer token would keep going upstream until the
// server happened to answer 401. Called by the runner's credential Invalidator
// when a revocation for this tool's backing Secret arrives on ap.revocation.
func (m *MCPTool) InvalidateAuth() {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	m.authStale = true
}

// currentAuth returns the tool's current (header, value) and whether it has been
// revoked, under lock.
func (m *MCPTool) currentAuth() (header, value string, stale bool) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	return m.header, m.value, m.authStale
}

// setAuthLocked replaces the cached (header, value) under lock and clears the
// revoked mark — the caller has just attached a freshly resolved credential.
func (m *MCPTool) setAuthLocked(header, value string) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	m.header = header
	m.value = value
	m.authStale = false
}

// errCredentialRevoked is returned by resolveIfRevoked when the frozen
// credential is revoked and cannot be re-resolved. It carries no token bytes.
var errCredentialRevoked = errors.New("credential revoked and no re-resolution path is configured")

// resolveIfRevoked returns the (header, value) Execute should send. When the
// credential has been revoked it re-resolves exactly once — reauthMu serializes
// concurrent tool-call goroutines, and the post-lock re-read means the losers of
// the race observe the winner's fresh credential instead of re-minting.
//
// Fails closed: an error here must abort the call. Falling back to the frozen
// credential is precisely the bug this exists to prevent.
func (m *MCPTool) resolveIfRevoked(ctx context.Context) (header, value string, err error) {
	if h, v, stale := m.currentAuth(); !stale {
		return h, v, nil
	}

	m.reauthMu.Lock()
	defer m.reauthMu.Unlock()

	// Re-read under reauthMu: a concurrent goroutine may have already refreshed.
	h, v, stale := m.currentAuth()
	if !stale {
		return h, v, nil
	}
	if m.reauth == nil {
		return "", "", errCredentialRevoked
	}
	nh, nv, rerr := m.reauth(ctx)
	if rerr != nil {
		return "", "", fmt.Errorf("credential revoked; re-resolution failed: %w", rerr)
	}
	m.setAuthLocked(nh, nv)
	return nh, nv, nil
}

// SetReauth wires a callback that re-resolves this tool's auth header+value
// from the (operator-refreshed) backing credential. Execute invokes it once
// on an HTTP 401 to recover from a mid-session OAuth token rotation, then
// retries the call with the fresh token. Nil (the default) disables the
// retry — the 401 surfaces as-is. Wired by the runner at session start.
func (m *MCPTool) SetReauth(fn func(ctx context.Context) (header, value string, err error)) {
	m.reauth = fn
}

// SetTimeoutForTest is for tests only.
func (m *MCPTool) SetTimeoutForTest(d time.Duration) { m.timeout = d }

// SetRelWriter wires the SpiceDB writer for post-Execute relationship
// writes. Called by the runner at session start; nil is allowed (the
// post-effect is then skipped).
func (m *MCPTool) SetRelWriter(w relwrites.Writer) { m.relWriter = w }

// SetSlotBoundChecker wires the per-tuple slot-grant check used by any
// declared block with requireSlotBound. Called by the runner at session start
// beside SetRelWriter, and by the e2e harness's own factory — those are two
// wiring sites, not one.
//
// Nil is ACCEPTED but is not a bypass: relwrites.Run refuses every tuple of a
// marked block against a nil checker, naming the block as unwired. That is the
// fallback for a wiring site nobody remembered, deliberately.
func (m *MCPTool) SetSlotBoundChecker(c relwrites.SlotBoundChecker) { m.slotBoundChecker = c }

// SetWritesRelationships wires the declared post-effect blocks from
// the MCPServer CR. Called by the runner at session start.
func (m *MCPTool) SetWritesRelationships(blocks []spiceboxv1alpha1.MCPServerRelationshipWrite) {
	m.writesRelationships = blocks
}

// SetObserves wires the declared `observes` blocks directly onto an
// already-constructed tool.
//
// NOT the production wiring path: Synthesize populates `observes` on every
// tool it builds straight from the CR (its own struct literal sets
// `observes: t.Observes`), so a production tool already carries its blocks
// before anything downstream ever sees it — internal/cmd/runner never calls
// this. It exists so dispatch_test.go / dispatch_observe_test.go can drive
// Execute's recording block in isolation, on a minimal one-tool CR, without
// round-tripping every case through a full Synthesize call. See
// TestSynthesize_WiresObservesFromCR for the assertion that actually covers
// the production path.
func (m *MCPTool) SetObserves(blocks []spiceboxv1alpha1.ObservesBlock) {
	m.observes = blocks
}

// SetLogger wires a slog logger used for post-effect failure reporting.
// Nil falls back to slog.Default() at call time.
func (m *MCPTool) SetLogger(l *slog.Logger) { m.logger = l }

// LabelSink is the surface MCPTool dispatch uses to stamp extracted
// labels. The runner's LabelStore satisfies it; tests use a recorder
// double. Decoupled from the full LabelStore so this package doesn't
// need to import the runner package.
type LabelSink interface {
	Put(resourceType, id, label string)
}

// SetLabels wires the per-tool labels CEL blocks declared on the
// MCPServer CR. Called by the runner at session start; nil/empty is
// allowed (the post-effect becomes a no-op).
func (m *MCPTool) SetLabels(blocks []spiceboxv1alpha1.MCPServerLabelExtract) {
	m.labels = blocks
}

// SetLabelSink wires the destination for extracted labels. Called by
// the runner at session start; nil is allowed (post-effect becomes a
// no-op).
func (m *MCPTool) SetLabelSink(sink LabelSink) {
	m.labelSink = sink
}

// SetMemory wires the in-process memory framework for forensic audit
// writes. Called by the runner at session start; nil is allowed (the
// relwrites_audit post-effect is skipped silently).
func (m *MCPTool) SetMemory(mem memorypkg.Memory) {
	m.mem = mem
}

// SetUseTokenGate wires the per-call SpiceDB use_token check (see
// UseTokenGate doc). Called by the runner at session start for tools with
// an injected auth credential; a nil gate (the default — never set) leaves
// the check disabled, matching the behavior of an unauthenticated tool with
// nothing to check.
func (m *MCPTool) SetUseTokenGate(g *UseTokenGate) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	m.useTokenGate = g
}

// SetSessionCache wires the per-AgentSession persistent-MCP-session cache. Called
// once by the runner at session start with the SAME cache instance for every
// MCPTool, so tools sharing a server URL reuse one session and server-side
// per-session state (dedicated-mcp's PermissionSystem selection) survives across
// calls. Nil (the default) falls back to a fresh session per call. Set before the
// tool loop begins, then read-only, so no lock is needed.
func (m *MCPTool) SetSessionCache(c *probe.SessionCache) { m.sessionCache = c }

// credentialIdentity names WHICH credential this tool presents, as opposed to
// the bytes it currently presents for it. SessionCache keys on this, so it must
// stay CONSTANT across a value rotation — an OAuth refresh, or a post-revocation
// re-resolve — while still separating two MCPServer CRs that share one endpoint
// under different credentials. `value` is the credential about to be sent, read
// only to tell "authenticated" from "not".
//
// externaltoken.CredID is exactly that identity: a hash of the CredentialSource
// coordinates, which a refresh does not touch. It also collapses two CRs bound
// to the SAME Secret+key onto one session — they are one principal upstream and
// one externaltoken subject in SpiceDB, so a revocation reaches both. Two CRs on
// different Secrets stay separate even if today's bytes match, because their
// grants are revocable independently.
//
// A tool with no named credential falls back to its own MCPServer CR: the
// conservative direction — never shared across CRs, still stable across rotation.
func (m *MCPTool) credentialIdentity(value string) string {
	if g := m.currentUseTokenGate(); g != nil && g.CredID != "" {
		return "cred/" + g.CredID
	}
	if value == "" {
		return "" // nothing to separate: unauthenticated callers are one principal
	}
	return "mcpserver/" + m.originName
}

// currentUseTokenGate returns the wired gate under lock, mirroring
// currentAuth. Exported test seam: mcp_tool_test.go (package mcp_test)
// cannot reach the unexported field directly.
func (m *MCPTool) currentUseTokenGate() *UseTokenGate {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	return m.useTokenGate
}

// UseTokenGateForTest exposes currentUseTokenGate to the external test
// package (mcp_test), mirroring SetTimeoutForTest's "…ForTest" convention
// for test-only accessors that must cross the package boundary.
func (m *MCPTool) UseTokenGateForTest() *UseTokenGate {
	return m.currentUseTokenGate()
}
