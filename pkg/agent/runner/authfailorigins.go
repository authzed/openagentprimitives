package runner

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// AuthFailureOrigins maps a tool ORIGIN — the exact string
// tool.OriginTool.Origin() returns, i.e. "mcpserver/<CR name>",
// "toolkit/<toolkit name>", or "sidecartoolbox/<CR name>" — to the authFailure:
// block declared by the provider behind that origin's credential. It is the
// lookup the credential-update corroboration recorder consults to tell "this
// credential was rejected" from "this call failed for some other reason". An
// origin ABSENT means "no corroboration available here", never a default-yes, so
// every path below fails closed by simply not recording an entry.
//
// # Lifecycle: build at session start, read-only thereafter
//
// Every entry is resolved ONCE, during runner boot, from the same credential
// requirements that built the tools. Deriving a shape per FAILED TOOL CALL would
// put an MCPServer/toolkit read plus a descriptor resolution on the tool hot
// path, for a signal only ever consulted at human latency. And the map is read
// from the parallel tool-dispatch goroutines, where boot-only writes are what
// make those reads race-free without a mutex — so nothing may write to it after
// the Loop is constructed. A mid-session tool refresh that discovers a new
// origin therefore leaves that origin uncorroborable, which is the safe
// direction; sidecars sidestep it by being recorded for every RESOLVED toolbox
// at boot, including ones whose pod is not up yet.
//
// A shared type rather than the derivation inlined per call site: three origin
// kinds × two wiring sites (internal/cmd/runner/main.go and the e2e harness's
// test/e2e/inprocess_runner_factory.go, which must build an identical runner or
// the e2e suite stops exercising what production ships) would be six copies of
// the same fail-closed rules.
type AuthFailureOrigins map[string]*provider.AuthFailure

// NewAuthFailureOrigins returns an empty, writable map.
func NewAuthFailureOrigins() AuthFailureOrigins { return AuthFailureOrigins{} }

// Lookup returns origin's declared auth-failure shape, or nil when the origin
// has none. Its signature is authfail.New's resolver parameter, so it can be
// passed as a method value. Safe on a nil map.
func (m AuthFailureOrigins) Lookup(origin string) *provider.AuthFailure { return m[origin] }

// RecordRequirements records origin's shape derived from the credential
// requirements that built its tools (the authkind SetupRequirements the runner
// already computes to resolve credentials).
//
// FIRST-WINS if reqs ever yields several provider-backed requirements — the mcp
// and cli kinds return at most one today, so it is unreachable, but the
// alternative (last-writer-wins) makes the recorded shape depend on requirement
// ORDER, which no caller controls.
//
// A requirement with no ProviderID, an unknown provider, or a provider that
// declares no authFailure: block all leave the origin ABSENT.
func (m AuthFailureOrigins) RecordRequirements(origin string, reqs []authkind.CredentialRequirement) {
	for _, req := range reqs {
		if m.RecordProvider(origin, req.ProviderID) {
			return
		}
	}
}

// RecordProvider records origin's shape from an explicitly declared provider ID
// (a SidecarToolbox's spec.upstreamAuth.provider names one directly, with no
// requirement to derive it from). Reports whether an entry now exists for
// origin.
//
// An EMPTY origin is refused outright: Origin() returns "" for a tool with no
// managed credential behind it (a toolkit-less sandbox tool), and "" must never
// become a map key — the recorder would then classify every such tool against
// some unrelated provider's shape.
func (m AuthFailureOrigins) RecordProvider(origin, providerID string) bool {
	if m == nil || origin == "" {
		return false
	}
	if _, already := m[origin]; already {
		return true
	}
	if providerID == "" {
		return false
	}
	p, ok := provider.ByID(providerID)
	if !ok || p.AuthFailure == nil {
		return false
	}
	m[origin] = p.AuthFailure
	return true
}

// The keys of this map are composed by pkg/agent/tool/originfmt's ForMCPServer
// / ForToolkit / ForSidecar — the ONLY places a wiring site should build one.
// They live in that leaf package rather than here because the CONSUMER of these
// observations (pkg/platform/identity/credupdate.ResolveOrigin, which turns an
// origin back into the credential a human is asked to replace) cannot import
// this package: pkg/agent/runner imports pkg/platform/identity/credupdate. Both
// sides importing originfmt is what keeps them from silently disagreeing about
// which origin kinds exist and recording entries nothing ever reads.
