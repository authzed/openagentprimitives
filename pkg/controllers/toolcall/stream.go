package toolcall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/web/gateway"
)

// interactiveSafetyCeiling bounds an interactive ToolCall whose MaxDuration
// is unset. Long-lived but not forever — protects against a stuck pod with no
// human attention.
const interactiveSafetyCeiling = 4 * time.Hour

// reconcileStreaming handles the spec.mode=stream path. Unlike the sync path,
// exec runs in a goroutine managed by the Registry; the current Reconcile
// returns immediately after registering the stream + populating
// status.streaming. Terminal conditions are set from the goroutine via a
// direct status patch on the ToolCall.
func (r *Reconciler) reconcileStreaming(ctx context.Context, tc *spiceboxv1alpha1.ToolCall, resolved *resolvedCall, agentEnv map[string]string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Already running in this operator's registry?
	if hasTrueCondition(tc, spiceboxv1alpha1.ToolCallConditionRunning) {
		if r.Registry != nil && r.Registry.Has(tc.Namespace, tc.Name) {
			// This process still owns the exec. Nothing to do; the watcher
			// goroutine will finalize when exec completes.
			return ctrl.Result{}, nil
		}
		// Running=True but no stream in the registry: the operator restarted
		// mid-execution and the in-process registry did not survive. There is
		// nothing left to reattach to and no goroutine left to finalize this
		// call, so it must be failed out here — exactly as the sync path does
		// for the identical situation.
		//
		// Leaving it Running is worse than a stale CR: channelsd selects the
		// session's Running-and-not-terminal interactive ToolCall as the live
		// one, so every subsequent user message would be routed to this dead
		// call and dropped, leaving the session permanently unresponsive.
		conditions.Set(tc, &tc.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonOperatorRestart,
		})
		r.setFailed(tc, spiceboxv1alpha1.ReasonOperatorRestart, "operator restarted mid-execution")
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	// Resolve the exec transport bound to this session's own sandbox. Never a
	// fallback to a default backend: a session with no handle yet, or one
	// whose kind has no runtime, fails the tool call outright.
	executor, err := r.executorFor(resolved.session)
	if err != nil {
		r.setFailed(tc, spiceboxv1alpha1.ReasonSandboxUnresolved, err.Error())
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	// Prepare hydration (re-uses Phase 2 code path — works in stream mode too).
	if err := r.hydrateInputs(ctx, resolved.session, string(tc.UID), executor, tc.Spec.InputArtifacts); err != nil {
		r.setFailed(tc, spiceboxv1alpha1.ReasonHydrateFailed, err.Error())
		finished := metav1.Now()
		tc.Status.FinishedAt = &finished
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	streamer, ok := executor.(exec.StreamingExecutor)
	if !ok {
		r.setFailed(tc, spiceboxv1alpha1.ReasonExecClosed, "executor does not support streaming")
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	// The runner commits the gateway stream token by hash: it generates the raw
	// token, keeps the preimage in memory, and stamps only spec.streamTokenHash.
	// A streaming ToolCall with no hash cannot be served — the gateway would
	// have nothing to compare a presented token against — so fail fast.
	streamTokenHash := tc.Spec.StreamTokenHash
	if streamTokenHash == "" {
		r.setFailed(tc, spiceboxv1alpha1.ReasonStreamTokenHashMissing,
			"streaming ToolCall is missing spec.streamTokenHash — the runner must commit the stream token hash at creation")
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	cmd := append([]string{}, resolved.tool.Command...)
	cmd = append(cmd, resolved.tool.DefaultArgs...)
	cmd = append(cmd, tc.Spec.Args...)

	// Deadline depends on mode:
	//   stream      → spec.timeout (default 60s) — upper bound on a short-lived stream.
	//   interactive → spec.maxDuration (default interactiveSafetyCeiling) — long-lived
	//                 sessions; the runner-side bridge enforces idle independently.
	var timeout time.Duration
	if tc.Spec.Mode == spiceboxv1alpha1.ToolCallModeInteractive {
		timeout = tc.Spec.MaxDuration.Duration
		if timeout <= 0 {
			timeout = interactiveSafetyCeiling
		}
	} else {
		timeout = tc.Spec.Timeout.Duration
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
	}
	execCtx, cancel := context.WithTimeout(context.Background(), timeout)

	stream, err := streamer.StreamExec(execCtx, exec.Request{
		Command: cmd,
		Env:     execEnv(agentEnv, tc, resolved.session, resolved.toolkit),
		Stdin:   streamingStdin(tc),
	})
	if err != nil {
		cancel()
		r.setFailed(tc, spiceboxv1alpha1.ReasonExecClosed, fmt.Sprintf("stream exec: %v", err))
		return ctrl.Result{}, r.Client.Status().Update(ctx, tc)
	}

	if err := r.Registry.Register(&gateway.ActiveStream{
		Namespace:    tc.Namespace,
		ToolCallName: tc.Name,
		TokenHash:    streamTokenHash,
		Stream:       stream,
		// Carrying cancel here lets the controller's deletion path stop the
		// exec for interactive ToolCalls — see Reconciler's deletion branch.
		// stream-mode ToolCalls inherit it too; harmless because nothing
		// calls CancelAndUnregister for them.
		Cancel: cancel,
	}); err != nil {
		cancel()
		_ = stream.Close()
		return ctrl.Result{}, fmt.Errorf("register: %w", err)
	}

	// Populate status: Running=True + streaming endpoint. The status carries
	// only Available + GatewayEndpoint — never a token. The raw token lives
	// solely in the runner's memory; the operator holds only the hash.
	r.setRunning(tc)
	tc.Status.Streaming = &spiceboxv1alpha1.StreamingEndpoint{
		Available:       true,
		GatewayEndpoint: r.GatewayEndpoint,
	}
	now := metav1.Now()
	tc.Status.StartedAt = &now
	if err := r.Client.Status().Update(ctx, tc); err != nil {
		cancel()
		_ = stream.Close()
		r.Registry.Unregister(tc.Namespace, tc.Name)
		return ctrl.Result{}, err
	}

	// Kick off a goroutine that waits for exec completion and finalizes status.
	go r.watchStreamCompletion(tc.DeepCopy(), resolved.session.DeepCopy(), stream, cancel, logger)

	return ctrl.Result{}, nil
}

func (r *Reconciler) watchStreamCompletion(
	tc *spiceboxv1alpha1.ToolCall,
	sess *spiceboxv1alpha1.SpiceboxSession,
	stream *exec.Stream,
	cancelExec context.CancelFunc,
	logger interface{ Error(error, string, ...any) },
) {
	defer cancelExec()
	// Remove from the registry only AFTER the terminal status patch below.
	// reconcileStreaming reads Running=True + registry-absence as "the operator
	// restarted"; unregistering first would open a window in which a reconcile
	// stamps a spurious OperatorRestart failure over a healthy completion. Once
	// the terminal condition is written, Reconcile short-circuits on it
	// instead. A defer also makes removal unconditional on every exit path.
	defer r.Registry.Unregister(tc.Namespace, tc.Name)

	result, waitErr := stream.Wait()

	// Patch terminal status. Retry on conflict: the main reconciler may be
	// doing a concurrent status update (e.g., re-validating a Running ToolCall),
	// which increments resourceVersion and causes a 409.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var fresh spiceboxv1alpha1.ToolCall
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(tc), &fresh); err != nil {
			logger.Error(err, "stream watcher: get toolcall")
			return
		}
		if terminalCondition(&fresh) != "" {
			return // already finalized (e.g., delete-while-running)
		}
		exitCode := result.ExitCode
		fresh.Status.ExitCode = &exitCode
		finished := metav1.Now()
		fresh.Status.FinishedAt = &finished
		conditions.Set(&fresh, &fresh.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.ToolCallConditionRunning, Status: metav1.ConditionFalse,
			Reason: spiceboxv1alpha1.ReasonExecClosed,
		})
		switch {
		case waitErr != nil && strings.Contains(waitErr.Error(), "context deadline exceeded"):
			r.setTimeout(&fresh)
		case exitCode == 0:
			r.setSucceeded(&fresh)
		default:
			r.setFailed(&fresh, spiceboxv1alpha1.ReasonNonZeroExit, fmt.Sprintf("exit code %d", exitCode))
		}
		if err := r.Client.Status().Update(ctx, &fresh); err != nil {
			if errors.IsConflict(err) {
				continue
			}
			logger.Error(err, "stream watcher: status update")
			return
		}
		break
	}
	if err := r.bumpSessionActivity(ctx, sess); err != nil {
		logger.Error(err, "stream watcher: bump session activity")
	}
}

// streamingStdin decides what is wired to a streaming ToolCall's stdin.
//
//   - interactive mode: returns nil. Channel input flows through the gateway
//     bridge into a live stdin pipe (see pkg/agent/tool/sandbox/bridge.go and
//     pkg/channels/channelsd/pipeline — KindToolSessionInput), so the exec layer must
//     keep the pipe open and writable. nil is the StreamExec contract for
//     "open a live pipe".
//   - stream mode: returns a finite reader (spec.stdin if set, otherwise an
//     empty reader). Stream-mode ToolCalls have no bridge — nothing ever
//     writes to their stdin. A finite reader reaches EOF immediately, so a
//     one-shot tool like `claude --print` sees stdin closed instead of an
//     open never-written pipe (which makes it stall ~3s and print a
//     "no stdin data received" warning).
func streamingStdin(tc *spiceboxv1alpha1.ToolCall) io.Reader {
	if tc.Spec.Mode == spiceboxv1alpha1.ToolCallModeInteractive {
		return nil
	}
	if tc.Spec.Stdin != "" {
		return strings.NewReader(tc.Spec.Stdin)
	}
	// Empty, immediately-at-EOF reader: stdin is closed from the tool's view.
	return bytes.NewReader(nil)
}
