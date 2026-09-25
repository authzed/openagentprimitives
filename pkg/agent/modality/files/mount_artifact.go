package files

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

type mountArtifact struct{ bridge modality.Bridge }

// NewMountArtifact builds the Tier-2 files-in tool: it moves a stored
// artifact into the provider's code-execution container via bridge so the
// agent can read it there without pasting the bytes into the conversation.
func NewMountArtifact(bridge modality.Bridge) tool.Tool { return &mountArtifact{bridge: bridge} }

func (*mountArtifact) Name() string    { return "mount_artifact" }
func (*mountArtifact) Kind() tool.Kind { return tool.KindMeta }
func (*mountArtifact) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*mountArtifact) PermissionVariants() []authz.PermissionVariant { return nil }

func (*mountArtifact) Description() string {
	return "Preload a stored artifact/large input into your code-execution environment so you can " +
		"read it there without pasting it into the conversation."
}

func (*mountArtifact) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"handle": {"type": "string", "description": "Artifact ref/handle to preload."}
		},
		"required": ["handle"]
	}`)
}

func (t *mountArtifact) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args struct {
		Handle string `json:"handle"`
	}
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"handle": "mem://…"}`); !ok {
		return res, nil
	}
	if args.Handle == "" {
		return tool.Result{Content: "mount_artifact: handle is required", IsError: true}, nil
	}
	if t.bridge == nil {
		return tool.Result{Content: "mount_artifact: native file handling not available", IsError: true}, nil
	}
	id, err := t.bridge.IntoContainer(ctx, artifactstore.Ref(args.Handle))
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("mount_artifact: %v", err), IsError: true}, nil
	}
	return tool.Result{
		Content:         fmt.Sprintf("Preloaded %s into your code-execution environment. Read it there.", args.Handle),
		ContainerUpload: &tool.ContainerUploadSpec{FileID: id},
	}, nil
}
