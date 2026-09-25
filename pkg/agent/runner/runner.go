package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// HandleStaleSession returns (true, nil) when the AgentSession is already in
// a terminal phase (Succeeded/Failed). It appends a system_note to memory
// and a RunnerNote to status, then returns. Caller exits 0.
//
// Returns (false, nil) when the session is still active; the caller should
// proceed with normal loop construction.
func HandleStaleSession(
	ctx context.Context,
	c client.Client,
	mem MemoryAppender,
	status *StatusPatcher,
	sessKey types.NamespacedName,
	runnerPodName string,
	memKey memory.NamespacedName,
) (bool, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := c.Get(ctx, sessKey, &sess); err != nil {
		return false, fmt.Errorf("get AgentSession: %w", err)
	}
	if !isTerminalPhase(sess.Status.Phase) {
		return false, nil
	}
	msg := fmt.Sprintf("Runner pod %q started but AgentSession is already %s; exiting.", runnerPodName, sess.Status.Phase)
	logInfo := slog.Default().Info
	// Append memory note (best-effort; if it fails, we still exit, but the
	// failure is logged so the missing audit note is diagnosable).
	prior, perr := mem.ReadAll(ctx)
	besteffort.Log(logInfo, "mem.ReadAll for stale-session note", perr,
		"session", sessKey.String())
	nextIdx := 0
	for _, t := range prior {
		if t.Index >= nextIdx {
			nextIdx = t.Index + 1
		}
	}
	besteffort.Log(logInfo, "mem.Append stale-session note", mem.Append(ctx, memory.Turn{
		Index: nextIdx, Role: "system_note",
		Content:   []memory.ContentBlock{{Type: "text", Text: msg}},
		CreatedAt: time.Now().UTC(),
	}), "session", sessKey.String())
	besteffort.Log(logInfo, "status.AppendRunnerNote stale-session note", status.AppendRunnerNote(ctx, msg),
		"session", sessKey.String())
	return true, nil
}

func isTerminalPhase(p string) bool {
	return p == spiceboxv1alpha1.AgentSessionPhaseSucceeded || p == spiceboxv1alpha1.AgentSessionPhaseFailed
}
