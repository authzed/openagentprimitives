package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// ArtifactHistoryConfig holds dependencies for the artifact_history tool.
type ArtifactHistoryConfig struct {
	Artifacts *artifacts.Service
}

// NewArtifactHistory returns a read-only tool that lists artifacts or returns
// the revision tree for a specific artifact in the current session.
func NewArtifactHistory(cfg ArtifactHistoryConfig) tool.Tool { return &artifactHistoryTool{cfg: cfg} }

type artifactHistoryTool struct{ cfg ArtifactHistoryConfig }

func (*artifactHistoryTool) Name() string    { return "artifact_history" }
func (*artifactHistoryTool) Kind() tool.Kind { return tool.KindMeta }
func (*artifactHistoryTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*artifactHistoryTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*artifactHistoryTool) Description() string {
	return "Inspect this session's artifacts and their revision history. " +
		"Omit `artifact` to list all artifacts; pass an artifact ID to get its revision tree (each revision's seq, change description, parent, and tags). " +
		"Use a revision ID or `artifact-ID#tag` with artifact_prepare's `revises` to branch from an earlier revision."
}

func (*artifactHistoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {"artifact": {"type": "string", "description": "Artifact ID (artifact-…). Omit to list all artifacts."}}
	}`)
}

func (t *artifactHistoryTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args struct {
		Artifact string `json:"artifact,omitempty"`
	}
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"artifact": "artifact-…"}`); !ok {
		return res, nil
	}
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	var payload any
	if args.Artifact == "" {
		list, err := t.cfg.Artifacts.ListArtifacts(ctx, scope)
		if err != nil {
			return tool.Result{Content: "artifact_history: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		payload = map[string]any{"artifacts": list}
	} else {
		tree, err := t.cfg.Artifacts.RevisionTree(ctx, scope, args.Artifact)
		if err != nil {
			return tool.Result{Content: "artifact_history: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		payload = map[string]any{"artifact": args.Artifact, "revisions": tree}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{Trusted: true}, fmt.Errorf("artifact_history: marshal: %w", err)
	}
	return tool.Result{Content: string(body), Trusted: true}, nil
}
