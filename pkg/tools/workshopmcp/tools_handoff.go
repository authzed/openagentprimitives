// tools_handoff.go implements the two tools that hand a finished build off
// to a person: request_install (ask a platform admin to install the agent
// this workshop built) and recommend_capability (recommend a platform
// capability the build surfaced a need for; the builder then calls
// agent_work_complete — separately — to end the session). Both are
// once-only: each writes a single pointer field on the builder session's
// own Workshop CR (spec.installRequest / spec.capabilityRequest) exactly
// once, same discipline as WorkshopCredentialRequest's dedupe
// (tools_credential.go) but keyed by presence rather than by content,
// because a repeat call here is a second, distinct decision — not a retry
// of the same one — and must be refused rather than silently accepted as a
// no-op. A later plan's channelsd watcher turns these fields into an admin
// card; this file only records the request.
package workshopmcp

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolRequestInstall / toolRecommendCapability are the MCP tool names
// announced on the sidecar's surface.
const (
	toolRequestInstall      = "request_install"
	toolRecommendCapability = "recommend_capability"
)

// errAlreadyRequested is the sentinel handleRequestInstall and
// handleRecommendCapability return from inside their RetryOnConflict
// closure when the once-only field is already set. retry.RetryOnConflict
// only re-invokes the closure on an apierrors.IsConflict error, so returning
// this non-conflict sentinel stops the retry immediately and lets the
// caller distinguish "refuse, do not overwrite" from a genuine write
// conflict (which IS retried) or an apiserver denial (checked separately
// via isDeniedErr).
var errAlreadyRequested = errors.New("already requested")

// registerHandoff wires request_install and recommend_capability onto
// mcpSrv — mirrors registerCredential's shape (tools_credential.go).
func (s *Server) registerHandoff(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolRequestInstall,
		Description: "Ask a platform admin to install the agent this workshop has built. Records the " +
			"request on the workshop; an admin card (delivered by a later plan) is what the person sees. " +
			"Once-only: a second call after an install has already been requested is refused, not overwritten. " +
			"If this agent's roster hands off to another agent via a stand-in you projected with " +
			"project_agent, that roster entry resolves to the REAL agent only if a class of that same " +
			"name already exists wherever this agent gets installed — an empty or unrelated namespace " +
			"means that hand-off will not work post-install. Tell the person this before they confirm.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"suggestedName": map[string]any{
					"type":        "string",
					"description": "The name to suggest for the installed AgentClass.",
				},
				"bundleDigest": map[string]any{
					"type":        "string",
					"description": "The exported .oap bundle's digest (from export_draft), if one has been exported.",
				},
			},
			"required": []any{"suggestedName"},
		},
	}, s.handleRequestInstall)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolRecommendCapability,
		Description: "Recommend a platform capability this build surfaced a need for — something no " +
			"existing tool or toolbox covers. Records the recommendation on the workshop for a platform " +
			"admin to review; a later plan delivers it as a card off spec.capabilityRequest. Once-only: a " +
			"second call after one has already been recorded is refused, not overwritten. This tool does " +
			"not end the session by itself — call agent_work_complete afterward to do that.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{
					"type":        "string",
					"description": "Plain-language summary of the capability gap and why the platform needs it.",
				},
				"artifactRef": map[string]any{
					"type":        "string",
					"description": "A reference to supporting material (a probe result, an exported draft, etc.) backing the recommendation.",
				},
				"draftRef": map[string]any{
					"type":        "string",
					"description": "An optional reference to a draft sketch of the recommended capability.",
				},
			},
			"required": []any{"summary", "artifactRef"},
		},
	}, s.handleRecommendCapability)
}

// installArgs is request_install's own argument shape.
type installArgs struct {
	SuggestedName string `json:"suggestedName"`
	BundleDigest  string `json:"bundleDigest"`
}

// handleRequestInstall answers the `request_install` tool call: sets
// ws.Spec.InstallRequest exactly once. A repeat call when it is already set
// is refused (errAlreadyRequested) rather than overwritten — the admin card
// this field later drives must not be silently re-pointed mid-review.
func (s *Server) handleRequestInstall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a installArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("request_install: decode arguments: %v", err), nil
	}
	if a.SuggestedName == "" {
		return s.toolErr("request_install: suggestedName is required"), nil
	}

	sessNS, sessName := s.sessionRef()
	key := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		if ws.Spec.InstallRequest != nil {
			return errAlreadyRequested
		}
		ws.Spec.InstallRequest = &spiceboxv1alpha1.WorkshopInstallRequest{
			SuggestedName: a.SuggestedName,
			BundleDigest:  a.BundleDigest,
		}
		return s.K8s.Update(ctx, &ws)
	}); err != nil {
		if errors.Is(err, errAlreadyRequested) {
			return s.toolErr("request_install: an install has already been requested for this workshop"), nil
		}
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("request_install: recording the request: %v", err), nil
	}
	return s.jsonResult(map[string]any{"suggestedName": a.SuggestedName, "status": "requested"})
}

// capabilityArgs is recommend_capability's own argument shape.
type capabilityArgs struct {
	Summary     string `json:"summary"`
	ArtifactRef string `json:"artifactRef"`
	DraftRef    string `json:"draftRef"`
}

// handleRecommendCapability answers the `recommend_capability` tool call:
// sets ws.Spec.CapabilityRequest exactly once, same once-only discipline as
// handleRequestInstall. It does not end the session itself — the builder
// calls agent_work_complete afterward to do that.
func (s *Server) handleRecommendCapability(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a capabilityArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("recommend_capability: decode arguments: %v", err), nil
	}
	if a.Summary == "" || a.ArtifactRef == "" {
		return s.toolErr("recommend_capability: summary and artifactRef are required"), nil
	}

	sessNS, sessName := s.sessionRef()
	key := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws spiceboxv1alpha1.Workshop
		if err := s.K8s.Get(ctx, key, &ws); err != nil {
			return err
		}
		if ws.Spec.CapabilityRequest != nil {
			return errAlreadyRequested
		}
		ws.Spec.CapabilityRequest = &spiceboxv1alpha1.WorkshopCapabilityRequest{
			Summary:     a.Summary,
			ArtifactRef: a.ArtifactRef,
			DraftRef:    a.DraftRef,
		}
		return s.K8s.Update(ctx, &ws)
	}); err != nil {
		if errors.Is(err, errAlreadyRequested) {
			return s.toolErr("recommend_capability: a capability has already been recommended for this workshop"), nil
		}
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("recommend_capability: recording the recommendation: %v", err), nil
	}
	return s.jsonResult(map[string]any{"summary": a.Summary, "status": "recommended"})
}
