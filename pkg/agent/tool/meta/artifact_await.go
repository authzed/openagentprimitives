package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

type ArtifactAwaitConfig struct {
	Client       client.Client
	Artifacts    *artifacts.Service
	PollInterval time.Duration
}

func NewArtifactAwait(cfg ArtifactAwaitConfig) tool.Tool {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	return &artifactAwaitTool{cfg: cfg}
}

type artifactAwaitTool struct {
	cfg ArtifactAwaitConfig
}

func (*artifactAwaitTool) Name() string    { return "artifact_await" }
func (*artifactAwaitTool) Kind() tool.Kind { return tool.KindMeta }
func (*artifactAwaitTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*artifactAwaitTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*artifactAwaitTool) Description() string {
	return "Wait for an artifact render (started by artifact_prepare and returned status=pending) to reach Ready and record the revision. " +
		"Returns the same shape as artifact_prepare. Use this when the renderer is taking longer than your artifact_prepare's max_wait_seconds — typically not needed for fast renderers like html."
}

func (*artifactAwaitTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"handle":           {"type": "string"},
			"max_wait_seconds": {"type": "integer", "minimum": 1, "maximum": 300}
		},
		"required": ["handle"]
	}`)
}

type artifactAwaitArgs struct {
	// Handle is the artifact_prepare handle to wait on.
	Handle string `json:"handle"`
	// MaxWaitSeconds bounds the wait; zero takes the tool's default.
	MaxWaitSeconds int `json:"max_wait_seconds,omitempty"`
}

func (t *artifactAwaitTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args artifactAwaitArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"handle": "ar-..."}`); !ok {
		return res, nil
	}
	maxWait := time.Duration(args.MaxWaitSeconds) * time.Second
	if maxWait <= 0 {
		maxWait = 60 * time.Second
	}
	if maxWait > 300*time.Second {
		maxWait = 300 * time.Second
	}

	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return tool.Result{Content: "artifact_await: " + err.Error(), IsError: true, Trusted: true}, nil
		}
		var fresh spiceboxv1alpha1.ArtifactRender
		if err := t.cfg.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: args.Handle}, &fresh); err != nil {
			if errors.IsNotFound(err) {
				return tool.Result{Content: fmt.Sprintf("artifact_await: handle %q not found in this session", args.Handle), IsError: true, Trusted: true}, nil
			}
			time.Sleep(t.cfg.PollInterval)
			continue
		}
		// The Get above is namespace-scoped, not session-scoped, and the runner
		// Role grants unpinned `list` on artifactrenders — so a name naming
		// ANOTHER session's render resolves here. Finalizing it would pull that
		// session's artifact into this one's chain and hand its metadata back to
		// this model. respond_to_user makes exactly this ownership check on the
		// same object kind (respond.go, tool.OwnedBySession); this is the fourth
		// spelling ownership.go's doc warns drifts. Checked before Finalize so a
		// foreign render is refused before anything is written or returned.
		if !tool.OwnedBySession(fresh.OwnerReferences, sess) {
			return tool.Result{
				Content: fmt.Sprintf("artifact_await: handle %q is not owned by this session — use one returned by your own artifact_prepare", args.Handle),
				IsError: true, Trusted: true,
			}, nil
		}
		switch fresh.Status.Phase {
		case spiceboxv1alpha1.ArtifactRenderPhaseReady:
			rev, err := t.cfg.Artifacts.FinalizeRevision(ctx, memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}, &fresh)
			if err != nil {
				return tool.Result{Content: fmt.Sprintf("artifact_await: render Ready but recording the revision failed: %v", err), IsError: true, Trusted: true}, nil
			}
			return marshalArtifactPrepareResult(artifactPrepareResult{
				Handle: fresh.Name, ArtifactID: rev.ArtifactID, RevisionID: rev.RevisionID, Seq: rev.Seq, Tags: rev.Tags,
				Status: "ready", MIME: fresh.Status.OutputMIME, Size: fresh.Status.OutputSize,
				Filename: fresh.Status.OutputFilename, Warnings: fresh.Status.Warnings,
			})
		case spiceboxv1alpha1.ArtifactRenderPhaseFailed:
			return tool.Result{
				Content: fmt.Sprintf("artifact_await: %s — %s", fresh.Status.FailureReason, fresh.Status.FailureMessage),
				IsError: true,
				Trusted: true,
			}, nil
		}
		time.Sleep(t.cfg.PollInterval)
	}
	return marshalArtifactPrepareResult(artifactPrepareResult{
		Handle:  args.Handle,
		Status:  "pending",
		Message: "Still rendering. Call artifact_await again with a larger max_wait_seconds, or proceed without this artifact.",
	})
}
