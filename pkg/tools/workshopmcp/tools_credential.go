// tools_credential.go implements request_credential — the builder session's
// way to ask a person to connect a bot/shared credential (an API key, PAT,
// static secret, or shared OAuth-MCP connection) for an AgentIdentity the
// builder has already authored in the workshop (via workshop_apply). It
// records the request onto the builder session's own Workshop CR
// (spec.credentialRequests), which a later channelsd watcher (not this
// file) turns into a card the person sees. The sidecar never touches a
// secret value itself — refs and names only, same discipline as
// WorkshopInstallRequest/WorkshopCapabilityRequest (workshop_types.go).
package workshopmcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
)

// toolRequestCredential is the MCP tool name announced on the sidecar's
// surface.
const toolRequestCredential = "request_credential"

// registerCredential wires request_credential onto mcpSrv — mirrors
// registerTests's shape (tools_testrun.go).
func (s *Server) registerCredential(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolRequestCredential,
		Description: "Ask the person to connect a bot/shared credential (an API key, PAT, static secret, or " +
			"shared OAuth-MCP connection) for an AgentIdentity you authored in this workshop. A card with a " +
			"secure link goes to the person; they paste (or, for oauth-mcp, connect) it and it lands in the " +
			"workshop.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"identity": map[string]any{
					"type":        "string",
					"description": "The AgentIdentity (already authored in this workshop) the credential belongs to.",
				},
				"credential": map[string]any{
					"type":        "string",
					"description": "The credential name on that AgentIdentity.",
				},
				"authKind": map[string]any{
					"type": "string",
					"description": "pat, static, or oauth-mcp. oauth-mcp is a shared/bot OAuth connect: the person " +
						"connects it via a Connect card, same delivery as pat/static. oauth-mcp REQUIRES an " +
						"MCPServer already applied in this workshop (via apply) whose spec.auth.credential " +
						"matches the credential name below — that object's spec.server.url is what the Connect " +
						"link discovers OAuth endpoints against. Apply the MCPServer first, or this call fails.",
				},
			},
			"required": []any{"identity", "credential", "authKind"},
		},
	}, s.handleRequestCredential)
}

// credArgs is request_credential's own argument shape.
type credArgs struct {
	Identity   string `json:"identity"`
	Credential string `json:"credential"`
	AuthKind   string `json:"authKind"`
}

// handleRequestCredential answers the `request_credential` tool call. It
// requires the target AgentIdentity to already exist in the workshop
// namespace W — this tool records a REQUEST, it does not author the
// identity itself — then appends a keyed (identity, credential) entry onto
// the builder session's own Workshop CR (in SessionNamespace, not W),
// skipping the append when that key is already present so a repeated call
// is idempotent rather than piling up duplicate entries.
//
// For AuthKind "oauth-mcp" specifically, it also requires an MCPServer
// already applied in W whose spec.auth.credential equals a.Credential. That
// object is what /link/agent-oauth/<cred> discovers OAuth endpoints
// against (handleLinkAgentOAuthGet, via
// passthroughcatalog.LookupMCPServerByCredentialInNamespace); with no
// matching MCPServer that click 400s with "No matching service" no matter
// how long the person waits. pat/static need no such check — those are a
// plain paste, with no discovery step to fail. Catching the missing
// MCPServer here, before the card is ever minted, turns a silent
// dead-on-arrival Connect link into an actionable error the builder sees
// immediately.
func (s *Server) handleRequestCredential(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a credArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("request_credential: decode arguments: %v", err), nil
	}
	if a.Identity == "" || a.Credential == "" || a.AuthKind == "" {
		return s.toolErr("request_credential: identity, credential, and authKind are required"), nil
	}
	switch a.AuthKind {
	case "pat", "static", "oauth-mcp":
		// supported
	default:
		return s.toolErr("request_credential: authKind must be pat, static, or oauth-mcp"), nil
	}

	// The target AgentIdentity must already exist in W — the builder authored
	// it via workshop_apply; this tool only records a request against it.
	var ai spiceboxv1alpha1.AgentIdentity
	if err := s.K8s.Get(ctx, client.ObjectKey{Namespace: s.Identity.Namespace, Name: a.Identity}, &ai); err != nil {
		return s.toolErr("request_credential: no AgentIdentity %q in this workshop: %v", a.Identity, err), nil
	}

	if a.AuthKind == "oauth-mcp" {
		if _, err := passthroughcatalog.LookupMCPServerByCredentialInNamespace(ctx, s.K8s, s.Identity.Namespace, a.Credential); err != nil {
			return s.toolErr("request_credential: oauth-mcp credential %q has no backing MCPServer in this workshop yet: %v — "+
				"apply one first (kind: MCPServer, spec.auth.type: oauth, spec.auth.credential: %q, spec.server.url: <the service's MCP endpoint>) "+
				"so the Connect link can discover its OAuth endpoints", a.Credential, err, a.Credential), nil
		}
	}

	sessNS, sessName := s.sessionRef()
	key := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		for _, cr := range ws.Spec.CredentialRequests {
			if cr.Identity == a.Identity && cr.Credential == a.Credential {
				return nil // idempotent: already requested
			}
		}
		ws.Spec.CredentialRequests = append(ws.Spec.CredentialRequests, spiceboxv1alpha1.WorkshopCredentialRequest{
			Identity: a.Identity, Credential: a.Credential, AuthKind: a.AuthKind,
		})
		return s.K8s.Update(ctx, &ws)
	}); err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("request_credential: recording the request: %v", err), nil
	}
	return s.jsonResult(map[string]any{"identity": a.Identity, "credential": a.Credential, "status": "requested"})
}
