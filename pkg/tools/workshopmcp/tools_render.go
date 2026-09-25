// tools_render.go implements `render_summary`: a plain-language rendering of
// a custom resource — a candidate not yet applied (pass `manifest`) or one
// already applied (pass `kind`+`name`) — for an approval card or the
// running summary (design spec §2.4).
//
// The Copy rule applies: the rendered sentence never names the underlying
// resource machinery — no "kind:"/"apiVersion:" field syntax, no `kubectl`,
// no CRD/YAML/namespace/relation vocabulary. A human approving
// workshop_apply reads this, not the manifest.
package workshopmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolRenderSummary is the name `render_summary` announces on the MCP
// surface.
const toolRenderSummary = "render_summary"

// registerRender wires `render_summary` onto mcpSrv.
func (s *Server) registerRender(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolRenderSummary,
		Description: "Plain-language rendering of a custom resource — pass `manifest` for a " +
			"candidate not yet applied, or `kind`+`name` for one already applied — for an approval " +
			"card or the running summary. Never mentions the underlying resource machinery.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"manifest": map[string]any{
					"type":        "object",
					"description": "a candidate resource to describe, not yet applied",
				},
				"kind": map[string]any{
					"type":        "string",
					"description": "kind of an already-applied resource to describe (used with name)",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "name of an already-applied resource to describe (used with kind)",
				},
			},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleRenderSummary)
}

// renderArgs is render_summary's own argument shape. Exactly one of
// Manifest or (Kind, Name) is expected to be set.
type renderArgs struct {
	Manifest map[string]any `json:"manifest,omitempty"`
	Kind     string         `json:"kind,omitempty"`
	Name     string         `json:"name,omitempty"`
}

// handleRenderSummary answers the `render_summary` tool call.
func (s *Server) handleRenderSummary(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a renderArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("render_summary: decode arguments: %v", err), nil
	}

	var obj client.Object
	switch {
	case len(a.Manifest) > 0:
		decoded, err := decodeManifestForKind(a.Manifest)
		if err != nil {
			return s.toolErr("render_summary: %v", err), nil
		}
		obj = decoded
	case a.Kind != "" && a.Name != "":
		fetched, err := newObjectForKind(a.Kind)
		if err != nil {
			return s.toolErr("render_summary: %v", err), nil
		}
		if err := s.K8s.Get(ctx, client.ObjectKey{Namespace: s.Identity.Namespace, Name: a.Name}, fetched); err != nil {
			return s.crudReadErr("render_summary", a.Kind, a.Name, err)
		}
		obj = fetched
	default:
		return s.toolErr("render_summary: either manifest or kind+name is required"), nil
	}

	return s.jsonResult(map[string]any{"summary": renderSummary(obj)})
}

// decodeManifestForKind decodes a raw candidate manifest map into the typed
// object its own "kind" field names, via a JSON round-trip through
// newObjectForKind's constructor. Kept separate from workshop_apply's own
// unstructured.Unstructured handling: render_summary needs a TYPED object so
// renderSummary's per-kind switch can read named fields, whereas applyCR
// only ever needs to pass the manifest through to the apiserver unchanged.
func decodeManifestForKind(manifest map[string]any) (client.Object, error) {
	kind, _ := manifest["kind"].(string)
	if kind == "" {
		return nil, fmt.Errorf("manifest must set kind")
	}
	obj, err := newObjectForKind(kind)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := json.Unmarshal(b, obj); err != nil {
		return nil, fmt.Errorf("decode manifest as %s: %w", kind, err)
	}
	return obj, nil
}

// renderSummary produces a plain-English sentence describing obj. Reuse of
// pkg/tools/kinds/*'s Detail/Row was considered and rejected: those render
// CLI-presentation output (field-name-shaped strings like "allowedFields="
// and "provider:") for a human already looking at a resource, not the
// Copy-rule-clean prose an approval card needs — so this is a small,
// dedicated per-kind switch instead.
func renderSummary(obj client.Object) string {
	switch cr := obj.(type) {
	case *spiceboxv1alpha1.AgentClass:
		return renderAgentClassSummary(cr)
	case *spiceboxv1alpha1.MCPServer:
		return renderToolConnectionSummary(cr.Name, cr.Spec.Intent, mcpToolNames(cr.Spec.Tools))
	case *spiceboxv1alpha1.SidecarToolbox:
		return renderToolConnectionSummary(cr.Name, cr.Spec.Intent, mcpToolNames(cr.Spec.Tools))
	case *spiceboxv1alpha1.SpiceboxToolspec:
		return renderToolspecSummary(cr)
	default:
		return fmt.Sprintf("%q is part of this agent's setup; no plain-language description is available for it yet.", obj.GetName())
	}
}

// renderAgentClassSummary describes an AgentClass — the agent itself.
func renderAgentClassSummary(cr *spiceboxv1alpha1.AgentClass) string {
	name := cr.Spec.DisplayName
	if name == "" {
		name = cr.Name
	}
	sentence := fmt.Sprintf("%q is an agent", name)
	if cr.Spec.Description != "" {
		sentence += ": " + cr.Spec.Description
	}
	return sentence + "."
}

// renderToolConnectionSummary describes an outside-tool connection —
// MCPServer and SidecarToolbox share this shape (name, one-line intent, an
// allowlist of named actions).
func renderToolConnectionSummary(name, intent string, actions []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q connects this agent to an outside tool.", name)
	if intent != "" {
		fmt.Fprintf(&b, " Its purpose: %s.", intent)
	}
	if len(actions) == 0 {
		b.WriteString(" No specific actions have been allowed yet.")
	} else {
		fmt.Fprintf(&b, " It may use: %s.", strings.Join(actions, ", "))
	}
	return b.String()
}

// renderToolspecSummary describes a SpiceboxToolspec — a command-line
// ability, allowlisted down to specific subcommands.
func renderToolspecSummary(cr *spiceboxv1alpha1.SpiceboxToolspec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q is a command-line ability this agent can use.", cr.Name)
	if cr.Spec.Intent != "" {
		fmt.Fprintf(&b, " Its purpose: %s.", cr.Spec.Intent)
	}
	if len(cr.Spec.AllowSubcommands) == 0 {
		b.WriteString(" No specific actions have been allowed yet.")
	} else {
		fmt.Fprintf(&b, " It may run: %s.", strings.Join(cr.Spec.AllowSubcommands, ", "))
	}
	return b.String()
}

// mcpToolNames extracts the allowlisted tool names from an MCPServerTool
// slice — the shape both MCPServer.Spec.Tools and SidecarToolbox.Spec.Tools
// share.
func mcpToolNames(tools []spiceboxv1alpha1.MCPServerTool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}
