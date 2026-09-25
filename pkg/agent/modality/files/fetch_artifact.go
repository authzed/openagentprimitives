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

type fetchArtifact struct{ reader modality.ArtifactReader }

// NewFetchArtifact builds the Tier-1 read-by-reference tool.
func NewFetchArtifact(r modality.ArtifactReader) tool.Tool { return &fetchArtifact{reader: r} }

func (*fetchArtifact) Name() string    { return "fetch_artifact" }
func (*fetchArtifact) Kind() tool.Kind { return tool.KindMeta }
func (*fetchArtifact) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*fetchArtifact) PermissionVariants() []authz.PermissionVariant { return nil }

func (*fetchArtifact) Description() string {
	return "Read bytes of a stored artifact, prior input, or oversized tool output by handle. " +
		"Optionally pass start/length to read a byte range. Only the bytes you read enter context."
}

func (*fetchArtifact) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"handle": {"type": "string", "description": "Artifact ref/handle to read."},
			"start": {"type": "integer", "description": "Start byte offset (default 0)."},
			"length": {"type": "integer", "description": "Bytes to read; 0 = to end."}
		},
		"required": ["handle"]
	}`)
}

func (t *fetchArtifact) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args struct {
		// Handle is the artifactstore ref to read.
		Handle string `json:"handle"`
		// Start is a byte offset from the beginning of the artifact.
		Start int64 `json:"start,omitempty"`
		// Length is a byte count; 0 means read to the end.
		Length int64 `json:"length,omitempty"`
	}
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"handle": "mem://…", "start": 0, "length": 4096}`); !ok {
		return res, nil
	}
	if args.Handle == "" {
		return tool.Result{Content: "fetch_artifact: handle is required", IsError: true}, nil
	}
	b, n, err := t.reader.ReadRange(ctx, artifactstore.Ref(args.Handle), args.Start, args.Length)
	if err != nil {
		return tool.Result{Content: fmt.Sprintf("fetch_artifact: read %s: %v", args.Handle, err), IsError: true}, nil
	}
	return tool.Result{Content: fmt.Sprintf("read %d bytes from %s:\n%s", n, args.Handle, string(b))}, nil
}
