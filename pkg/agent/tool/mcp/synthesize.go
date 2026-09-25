// Package mcp wires MCPServer CRs into the runner-side tool registry. Each
// allowlisted entry becomes one tool.Tool whose Execute issues a JSON-RPC
// tools/call against the server.
package mcp

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Option customizes Synthesize. Knobs today: the HTTP client used by
// synthesized MCPTool dispatch (production leaves it unset and gets the
// SSRF-guarded safehttp.Client(), while in-process tests that target a
// loopback MCP stub inject a plain client via WithHTTPClient), and the
// revocation origin name override (WithOriginName) for callers that pass a
// CR copy whose metadata.name has been overwritten with the LLM-prefix.
type Option func(*synthOpts)

type synthOpts struct {
	httpClient *http.Client
	// originName, when non-empty, overrides the MCPTool revocation origin
	// (Origin() = "mcpserver/"+originName). Defaults to cr.Name. See
	// WithOriginName.
	originName string
	// sessionCache, when non-nil, is the per-AgentSession persistent MCP
	// session cache every synthesized tool dispatches through. See
	// WithSessionCache.
	sessionCache *probe.SessionCache
}

// WithHTTPClient overrides the HTTP client used for tools/call dispatch.
// Intended for tests that drive a loopback MCP stub which the default
// SSRF-guarded client would (correctly) refuse to connect to. Production
// callers MUST NOT use this — omitting it yields the guarded client.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *synthOpts) { o.httpClient = hc }
}

// WithSessionCache wires the caller's per-AgentSession MCP session cache into
// every synthesized tool, so all of this server's tool calls ride ONE MCP
// session and server-side per-session state (the dedicated-mcp sidecar's
// PermissionSystem selection, keyed by the MCP session id) survives across
// calls. Omitting it yields the fallback in dispatch: a fresh session — and
// therefore a reset of that state — per call.
//
// This exists as a Synthesize option, not just the post-hoc SetSessionCache
// setter, because callers that wrap synthesized tools (sidecartoolbox wraps
// each in *originTool) cannot reach the *MCPTool afterwards to set it.
func WithSessionCache(c *probe.SessionCache) Option {
	return func(o *synthOpts) { o.sessionCache = c }
}

// WithOriginName overrides the MCPServer CR name used as each tool's
// revocation origin (Origin() = "mcpserver/"+name). The runner overwrites the
// fetched CR's metadata.name with the AgentClass LLM-prefix before synthesizing
// (so the LLM-facing tool names get that prefix), but revocation publishers and
// the runner's restart filter key on the REAL CR name. The runner must pass the
// real CR name here so Origin() matches the revoke key. Callers that pass an
// un-mangled CR can omit this — it defaults to cr.Name.
func WithOriginName(name string) Option {
	return func(o *synthOpts) { o.originName = name }
}

// SynthesizeResult separates tools offered to the LLM from app-visible-only
// tools held out of the LLM's reach. AppTools are MCP-UI app-visible tools
// (opt-in gated via MCPServerSpec.MCPUIAppTools.Enabled); they are NEVER
// placed in the LLM tool list and are not callable in this phase (no
// transport yet — see pkg/agent/runner.Loop.AppTools).
type SynthesizeResult struct {
	// LLMTools is offered to the model: wire into Loop.Tools / buildToolDefs.
	LLMTools []agenttool.Tool
	// AppTools is NEVER the LLM's: wire into the separate Loop.AppTools
	// registry only. Empty when MCPUIAppTools is unset/disabled or the
	// server declares no app-only tools.
	AppTools []agenttool.Tool
}

// Dispatchable returns every synthesized tool that can reach the upstream
// server, LLM-visible and app-visible alike.
//
// It exists so a caller wiring what a call NEEDS — credential, reauth callback,
// per-call token gate, session cache — cannot wire one list and forget the
// other. Visibility governs who may ASK for a tool and has no bearing on how it
// authenticates.
//
// Wiring only LLMTools makes every app-only tool dispatch with no Authorization
// header, and the resulting 401 points nowhere: the credential is valid, the
// identity fresh, and the session's startup probe passes because listing tools
// needs no credential at all.
func (r SynthesizeResult) Dispatchable() []agenttool.Tool {
	out := make([]agenttool.Tool, 0, len(r.LLMTools)+len(r.AppTools))
	out = append(out, r.LLMTools...)
	return append(out, r.AppTools...)
}

// Synthesize returns one MCPTool per allowlisted entry, split by
// _meta.ui.visibility into LLMTools (offered to the model) and AppTools (an
// MCP-UI app-visible-only registry the LLM never sees). A tool is routed to
// AppTools only when its Visibility contains "app" and excludes "model",
// AND the server has opted in via MCPUIAppTools.Enabled; otherwise (no
// visibility, "model" visibility, or both "app"+"model") it is LLM-visible
// as before. Without the opt-in, an app-only tool is rejected-by-default:
// not synthesized into either set. Returns an error if the allowlist
// contains a name not present in `live` (drift detected at runtime).
func Synthesize(cr *spiceboxv1alpha1.MCPServer, live []probe.Tool, opts ...Option) (SynthesizeResult, error) {
	var so synthOpts
	for _, o := range opts {
		o(&so)
	}
	// Production path: no override → SSRF-guarded client. The MCP server
	// URL comes from a CR an agent may have authored, so the dialer must
	// refuse private/loopback/link-local destinations.
	httpClient := so.httpClient
	if httpClient == nil {
		httpClient = safehttp.Client()
	}
	// Default the revocation origin to the CR's own metadata.name. Callers
	// (the runner) that pass a CR whose name was overwritten with the
	// LLM-prefix override this via WithOriginName(realCRName).
	originName := so.originName
	if originName == "" {
		originName = cr.Name
	}
	byName := map[string]probe.Tool{}
	for _, t := range live {
		byName[t.Name] = t
	}
	timeout := cr.Spec.CallTimeout.Duration
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	// Convert the CR spec once and share the pointer across every
	// synthesized tool — each MCPTool.Execute reads the same spec, so
	// per-tool ToSpec calls would just redo a JSON roundtrip.
	sharedSpec := mustToSpec(cr)

	// appEnabled gates the whole app-visible split: reject-by-default means an
	// app-only tool is simply never synthesized (into either set) unless the
	// server explicitly opted in.
	appEnabled := cr.Spec.MCPUIAppTools != nil && cr.Spec.MCPUIAppTools.Enabled

	llmEntries := make([]synthesize.Entry, 0, len(cr.Spec.Tools))
	var appEntries []synthesize.Entry
	for i := range cr.Spec.Tools {
		// Capture loop variables for the Factory closure.
		t := cr.Spec.Tools[i]
		liveT, ok := byName[t.Name]
		if !ok {
			return SynthesizeResult{}, fmt.Errorf("mcp: server %q does not expose tool %q", cr.Name, t.Name)
		}
		appOnly := !ModelVisible(t)
		if appOnly && !appEnabled {
			continue // reject-by-default: app-only tool with no opt-in is not synthesized AT ALL
		}
		entry := synthesize.Entry{
			Name: t.Name,
			Factory: func(llmName string) agenttool.Tool {
				var p authz.Permission
				if t.Permission != nil {
					p = *t.Permission
				}
				return &MCPTool{
					serverName:  cr.Name,
					originName:  originName,
					toolName:    t.Name,
					llmName:     llmName,
					description: buildDescription(t, liveT),
					url:         cr.Spec.Server.URL,
					timeout:     timeout,
					args: mcpspec.Args{
						AllowedFields:     t.Args.AllowedFields,
						UnconstrainedArgs: t.Args.UnconstrainedArgs,
						Constraints:       convertConstraints(t.Args.Constraints),
						SensitiveFields:   t.Args.SensitiveFields,
					},
					httpClient:          httpClient,
					sessionCache:        so.sessionCache,
					inputSchema:         liveT.InputSchema,
					permission:          p,
					readOnlyHint:        t.Effects.ReadOnly,
					permissionVariants:  t.PermissionVariants,
					writesRelationships: t.WritesRelationships,
					observes:            t.Observes,
					labels:              t.Labels,
					spec:                sharedSpec,
				}
			},
		}
		if appOnly {
			appEntries = append(appEntries, entry)
		} else {
			llmEntries = append(llmEntries, entry)
		}
	}
	llmTools, err := synthesize.Build(synthesize.Slice{P: cr.Name, E: llmEntries})
	if err != nil {
		return SynthesizeResult{}, err
	}
	// Two Build calls sharing the same prefix are safe here: appEntries and
	// llmEntries are disjoint by tool name (each cr.Spec.Tools entry routes to
	// exactly one), so there is no cross-set name collision to detect.
	var appTools []agenttool.Tool
	if len(appEntries) > 0 {
		appTools, err = synthesize.Build(synthesize.Slice{P: cr.Name, E: appEntries})
		if err != nil {
			return SynthesizeResult{}, err
		}
	}
	return SynthesizeResult{LLMTools: llmTools, AppTools: appTools}, nil
}

// ModelVisible reports whether an allowlist entry is offered to the MODEL.
//
// A tool is app-only when its visibility contains "app" and NOT "model"; every
// other shape — no visibility at all, "model", or both — is the model's. An
// app-only tool never reaches the LLM tool list: with the MCP-UI opt-in it is
// routed to AppTools, and without it it is not synthesized at all. Either way
// the model is not offered it, which is the single question this answers.
func ModelVisible(t spiceboxv1alpha1.MCPServerTool) bool {
	return !(slices.Contains(t.Visibility, "app") && !slices.Contains(t.Visibility, "model"))
}

// LLMToolNames returns the LLM-facing names Synthesize gives cr's allowlisted
// tools, assuming the upstream server exposes every one of them.
//
// It answers "what WILL these tools be called" without building them, for a
// caller that has a spec and no live server to probe — the steelthread capture,
// which has to name the tools a fixture rewrite makes reachable. It shares both
// halves of the answer with Synthesize itself (ModelVisible for which entries
// count, synthesize.FullName for what each is called), so the two cannot drift
// into disagreeing about a name.
//
// It is a SUPERSET of what Synthesize would actually return against a given
// server: Synthesize errors out on an allowlist entry the server does not
// expose, and this cannot see the server. Callers using it as a permission to
// expect a name must tolerate the name being absent.
func LLMToolNames(cr *spiceboxv1alpha1.MCPServer) []string {
	out := make([]string, 0, len(cr.Spec.Tools))
	for _, t := range cr.Spec.Tools {
		if !ModelVisible(t) {
			continue
		}
		out = append(out, synthesize.FullName(cr.Name, t.Name))
	}
	return out
}

// convertConstraints maps the CRD's local MCPServerConstraint slice into the
// runtime toolspec.Constraint slice. The fields are identical by construction;
// we keep the explicit conversion so the two types can diverge independently.
func convertConstraints(in []spiceboxv1alpha1.MCPServerConstraint) []toolspec.Constraint {
	if len(in) == 0 {
		return nil
	}
	out := make([]toolspec.Constraint, len(in))
	for i, c := range in {
		out[i] = toolspec.Constraint{
			CEL:     c.CEL,
			Message: c.Message,
		}
	}
	return out
}

// mustToSpec converts the CR spec to mcpspec.Spec. Synthesize is called
// from the AgentSession controller after admission has validated the
// CR; the conversion is JSON-roundtrip on identical shapes, so a
// failure here indicates a programming error.
func mustToSpec(cr *spiceboxv1alpha1.MCPServer) *mcpspec.Spec {
	sp, err := cr.Spec.ToSpec()
	if err != nil {
		// The same shape is enforced by admission; surface the panic
		// so the controller's recovery captures it instead of silently
		// running a half-built MCPTool.
		panic(fmt.Sprintf("mcp: Synthesize: ToSpec failed: %v", err))
	}
	return sp
}

func buildDescription(t spiceboxv1alpha1.MCPServerTool, live probe.Tool) string {
	if t.DescriptionOverride != "" {
		return t.DescriptionOverride
	}
	parts := []string{}
	if t.Intent != "" {
		parts = append(parts, strings.TrimSpace(t.Intent))
	}
	if live.Description != "" {
		parts = append(parts, strings.TrimSpace(live.Description))
	}
	return strings.Join(parts, " — ")
}
