// Package sidecartoolbox synthesizes runner-side MCP tools from a
// ResolvedSidecarToolbox snapshot. Reuses pkg/agent/tool/mcp's MCPTool
// (JSON-RPC dispatcher + CEL pre-call validation) so the runner can't
// tell a sidecar apart from a remote MCP at the dispatch layer.
package sidecartoolbox

import (
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcptool "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// EndpointPathOf normalizes a spec.transport.path value: "" for the pod
// root, or a leading-slash path like "/mcp". The CR's transport.path is the
// SINGLE source of truth for where a sidecar serves MCP — every reach path
// (the runner's dispatch and probe URLs, and the operator's admission probe)
// derives from it through this function, so none of them can target a
// different path than the others.
func EndpointPathOf(path string) string {
	p := strings.TrimSpace(path)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// EndpointPath returns the normalized MCP endpoint path suffix for a resolved
// sidecar (spec.transport.path), via the shared EndpointPathOf normalizer:
// "" for the pod root, or a leading-slash path like "/mcp". Shared by the
// runner's reachability-probe URL and this dispatch URL so the two can never
// target different paths. The Phase-1 stub served MCP at "/"; the real
// dedicated-mcp serves it at both "/mcp" and "/", so either value works there
// — but a server that only mounts "/mcp" needs this set.
func EndpointPath(rt spiceboxv1alpha1.ResolvedSidecarToolbox) string {
	return EndpointPathOf(rt.Spec.Transport.Path)
}

// Synthesize returns one tool.Tool per allowlisted entry in rt.Spec.Tools.
// For in-pod sidecars (RunMode != "separate-pod") the target URL is
// http://127.0.0.1:<rt.Port>/. For separate-pod sidecars the target URL is
// http://<rt.SidecarPodIP>:<rt.Port>/ (the operator reflects SidecarPodIP
// once the pod is Ready). Returns an error if any name in the allowlist is
// missing from `live`.
//
// The synthesized MCPServer carries metadata.Name=rt.Name so that the
// MCP synthesizer's prefix-derivation produces the LLM-facing prefix the
// AgentClass author chose (rt.Name = AgentClassSidecarToolboxRef.Name).
func Synthesize(rt spiceboxv1alpha1.ResolvedSidecarToolbox, live []probe.Tool, sessionCache *probe.SessionCache) ([]agenttool.Tool, error) {
	var url string
	if rt.RunMode == "separate-pod" {
		url = fmt.Sprintf("http://%s:%d%s", rt.SidecarPodIP, rt.Port, EndpointPath(rt))
	} else {
		url = fmt.Sprintf("http://127.0.0.1:%d%s", rt.Port, EndpointPath(rt))
	}
	synth := synthCR(rt, url)
	// Sidecars are operator-controlled processes reached over loopback or a pod
	// IP, never an LLM-supplied URL, so the safehttp SSRF guard must not apply —
	// inject a plain client for both reach paths.
	//
	// synth.Spec.MCPUIAppTools stays unset, so an app-only tool
	// (Visibility=["app"]) is rejected by default and absent from both result
	// lists: sidecar toolboxes do not opt into the MCP-UI app-tool split.
	//
	// The session cache is threaded in AT synthesis time, not set afterwards:
	// every tool below is wrapped in *originTool, so a caller holding the
	// returned tools can no longer reach the underlying *MCPTool.
	res, err := mcptool.Synthesize(synth, live,
		mcptool.WithHTTPClient(http.DefaultClient),
		mcptool.WithSessionCache(sessionCache))
	if err != nil {
		return nil, fmt.Errorf("sidecartoolbox %q: %w", rt.Ref, err)
	}
	tools := res.LLMTools
	// Tag every synthesized tool with the sidecar origin so toolguard's
	// origin-level circuit breaker treats all tools of one sidecar as
	// sharing health (replaces the former per-tracker degrade.go logic).
	wrapped := make([]agenttool.Tool, len(tools))
	for i, t := range tools {
		wrapped[i] = &originTool{inner: t, sidecarRef: rt.Ref}
	}
	return wrapped, nil
}

// synthCR builds the MCPServer a sidecar's tools are synthesized through.
//
// One construction site, shared by Synthesize and LLMToolNames, because both
// depend on metadata.Name being rt.Name: that is what makes the LLM-facing
// prefix the one the AgentClass author chose. A second literal would let the
// names a caller PREDICTS drift from the names the runner OFFERS, and the
// symptom of that drift is a tool that appears missing rather than misnamed.
func synthCR(rt spiceboxv1alpha1.ResolvedSidecarToolbox, url string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: rt.Name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:   rt.Spec.Name,
			Server: spiceboxv1alpha1.MCPServerServer{URL: url, Transport: "streamable-http"},
			Tools:  rt.Spec.Tools,
		},
	}
}

// LLMToolNames returns the LLM-facing names Synthesize gives rt's allowlisted
// tools, without probing the sidecar or building anything.
//
// For a caller that has the resolved snapshot and no running sidecar — the
// steelthread capture, naming the tools its own fixture rewrite makes
// reachable. Superset semantics: see mcptool.LLMToolNames. The URL is
// irrelevant to a name, so this passes an empty one rather than inventing a
// port the sidecar never bound.
func LLMToolNames(rt spiceboxv1alpha1.ResolvedSidecarToolbox) []string {
	return mcptool.LLMToolNames(synthCR(rt, ""))
}
