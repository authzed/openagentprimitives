// Package originfmt owns the wire format of a tool's ORIGIN — the string
// tool.OriginTool.Origin() returns, i.e. "mcpserver/<CR name>",
// "toolkit/<toolkit name>", or "sidecartoolbox/<CR name>".
//
// # Why this is its own package
//
// An origin is WRITTEN by one side and READ by another, and the two sides sit
// on opposite ends of the dependency graph:
//
//   - the WRITER (internal/cmd/runner and the e2e in-process factory, via
//     runner.AuthFailureOrigins) composes an origin key to record a failing
//     call's declared auth-failure shape;
//   - the READER (pkg/platform/identity/credupdate.ResolveOrigin) parses that origin
//     back into the credential a human is asked to replace.
//
// pkg/agent/runner imports pkg/platform/identity/credupdate, so the reader
// cannot import the writer's package to reuse its spellings. Each side spelling
// the prefixes itself is how the two silently disagree about which origin kinds
// exist — a writer recording all three kinds against a reader accepting only
// "mcpserver" writes observations nothing ever reads, with no error anywhere.
// Both sides import THIS package, a leaf over only the API types, so that
// divergence is not expressible.
package originfmt

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The origin kinds. These are the "<kind>" half of an origin string — the
// token a reader switches on and the prefix a writer composes with. They are
// declared once, here, so a reader can never accept a set of kinds that
// differs from the set a writer can produce.
const (
	// KindMCPServer is the origin kind of a tool served by an MCPServer CR.
	KindMCPServer = "mcpserver"
	// KindToolkit is the origin kind of a sandbox tool backed by a CLI
	// toolkit (an embedded builtin or a SpiceboxToolkit CR).
	KindToolkit = "toolkit"
	// KindSidecarToolbox is the origin kind of a tool served by a
	// SidecarToolbox CR's sidecar MCP server.
	KindSidecarToolbox = "sidecartoolbox"
)

// The three composers below are the ONLY places a caller should build an origin
// string. Each mirrors one tool kind's Origin(), and a silent divergence is the
// worst failure mode corroboration has: nothing errors, the per-call lookup just
// misses forever, and every credential of that kind becomes permanently
// uncorroborable. runner.TestAuthFailureOriginKeysMatchToolOrigins pins each
// composer against a real tool's Origin().
//
// Each also mirrors Origin()'s EMPTY case: an unnamed subject yields "", not a
// bare prefix. "toolkit/" is a perfectly good map key no Origin() can ever
// equal, so it would sit unreachable while looking, in a debugger, like the
// wiring worked.

// ForMCPServer mirrors mcp.MCPTool's Origin(). name is the MCPServer CR's own
// name — NOT the LLM-facing prefix from the AgentClass ref.
func ForMCPServer(name string) string {
	if name == "" {
		return ""
	}
	return KindMCPServer + "/" + name
}

// ForToolkit mirrors sandbox.SandboxTool's Origin(). name is
// toolkit.Toolkit.Name — the toolkit's LOGICAL name, not necessarily the backing
// CR's metadata.name.
//
// Two bundles referencing the same toolkit name under different credentials
// therefore COLLIDE on one origin, and RecordRequirements' first-wins picks one
// shape for both. That is inherent to Origin() being toolkit-name-keyed
// (toolguard's origin breakers already share a key across those bundles); a
// per-bundle shape would need Origin() itself to carry the bundle first.
func ForToolkit(name string) string {
	if name == "" {
		return ""
	}
	return KindToolkit + "/" + name
}

// ForSidecar mirrors sidecartoolbox's originTool.Origin().
//
// It takes the whole resolved entry rather than a string precisely because
// ResolvedSidecarToolbox carries two plausible names: Ref (the SidecarToolbox
// CR, which is what Origin() uses) and Name (the LLM-facing prefix). Passing
// the struct makes picking the wrong one impossible at the call site.
func ForSidecar(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string {
	if rt.Ref == "" {
		return ""
	}
	return KindSidecarToolbox + "/" + rt.Ref
}
