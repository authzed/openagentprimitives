package sessioncmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memstream"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/transcript"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newSessionLogsCmd(g *apcmd.Globals) *cobra.Command {
	var (
		follow  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Stream memory turns for an AgentSession",
		Long: `Print every memory turn (system / user / assistant / tool) recorded
for the given AgentSession. By default, the command prints the full
history and exits — pass --follow to keep streaming new turns until the
session reaches a terminal phase (or Ctrl-C).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunLogs(cmd.Context(), cmd.OutOrStdout(), g, args[0], follow, timeout)
		},
	}
	apcmd.FollowFlags(cmd, &follow, &timeout)
	return cmd
}

func RunLogs(ctx context.Context, out io.Writer, g *apcmd.Globals, sessionName string, follow bool, timeout time.Duration) error {
	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// Confirm the session exists; read the per-session memory token.
	var sess spiceboxv1alpha1.AgentSession
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &sess); err != nil {
		return fmt.Errorf("get AgentSession %s/%s: %w", b.Namespace, sessionName, err)
	}
	var sec corev1.Secret
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName + "-memory-token"}, &sec); err != nil {
		return fmt.Errorf("get memory-token Secret for %s: %w", sessionName, err)
	}
	memToken := string(sec.Data["token"])
	if memToken == "" {
		return fmt.Errorf("memory-token Secret %s-memory-token has empty 'token' key", sessionName)
	}

	pf, err := portforward.New(b.REST, "agentprimitives-system", "spicebox-operator", "app.kubernetes.io/name=spicebox-operator", 8082, 0)
	if err != nil {
		return err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return fmt.Errorf("port-forward: %w", err)
	}
	defer pf.Stop()

	// Capabilities come from the stream this command writes to, so a pipe,
	// --no-color and NO_COLOR each land on the colorless theme. That matters
	// more here than in a list command: these lines are read through `grep`
	// and `jq` constantly, and an escape code in one corrupts the parse.
	th := g.Theme(out)
	streamer := memstream.New(pf.URL(), b.Namespace, sessionName, memToken, 1*time.Second)
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	turnCh, errCh := streamer.Stream(streamCtx)
	toolCh, toolErrCh := streamer.StreamEntries(streamCtx, "tool_session")

	terminalPhase := isTerminalPhase(sess.Status.Phase)
	phaseDone := make(chan string, 1)
	if follow && !terminalPhase {
		go func() {
			_ = wait.Until(streamCtx, 1*time.Second, timeout, func(ctx context.Context) (bool, error) {
				var fresh spiceboxv1alpha1.AgentSession
				if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &fresh); err != nil {
					return false, err
				}
				if isTerminalPhase(fresh.Status.Phase) {
					phaseDone <- fresh.Status.Phase
					return true, nil
				}
				return false, nil
			})
		}()
	}

	// In dump-and-exit mode (no --follow, or session already terminal) we exit
	// once turns stop arriving. The timer is armed up-front so a session with
	// zero recorded turns (e.g. operator restarted and the in-memory store was
	// wiped) doesn't hang — it exits cleanly after the idle window.
	idleAfterFirstFetch := !follow || terminalPhase
	const idleWindow = 2 * time.Second
	idleTimer := time.NewTimer(idleWindow)
	if !idleAfterFirstFetch {
		idleTimer.Stop()
	}
	turnsEmitted := 0

	// resetIdle re-arms the idle timer in dump-and-exit mode. Tool-session
	// events count as activity too, so a busy tool stream keeps the
	// session alive even when no turns are arriving.
	resetIdle := func() {
		if !idleAfterFirstFetch {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleWindow)
	}

	for {
		select {
		case t, ok := <-turnCh:
			if !ok {
				// The turn stream closed on its own — drain any
				// buffered tool-session entries before exiting, for
				// symmetry with the idleTimer / phaseDone drain paths.
				// toolCh is nil once its stream has already closed;
				// ranging a nil channel blocks forever, so drain it
				// only while live.
				for toolCh != nil {
					e, ok := <-toolCh
					if !ok {
						break
					}
					transcript.RenderToolSessionEntry(out, th, e)
				}
				return nil
			}
			transcript.RenderTurn(out, th, t)
			turnsEmitted++
			resetIdle()
		case e, ok := <-toolCh:
			if !ok {
				// The tool-session stream closing is not terminal for the
				// logs command — keep streaming turns; just stop selecting
				// this channel.
				toolCh = nil
				continue
			}
			transcript.RenderToolSessionEntry(out, th, e)
			resetIdle()
		case <-idleTimer.C:
			streamCancel()
			for t := range turnCh {
				transcript.RenderTurn(out, th, t)
				turnsEmitted++
			}
			// toolCh is nil once its stream has already closed; ranging a
			// nil channel blocks forever, so drain it only while live.
			for toolCh != nil {
				e, ok := <-toolCh
				if !ok {
					break
				}
				transcript.RenderToolSessionEntry(out, th, e)
			}
			if turnsEmitted == 0 {
				fmt.Fprintln(out, th.Render(th.Warn, "no memory turns recorded for "+sessionName+
					" (the operator's in-memory store may have been wiped by a restart)"))
			}
			return nil
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(out, th.Render(th.Warn, "stream error: "+err.Error()))
			}
			errCh = nil
		case err := <-toolErrCh:
			if err != nil {
				fmt.Fprintln(out, th.Render(th.Warn, "tool-session stream error: "+err.Error()))
			}
			toolErrCh = nil
		case <-phaseDone:
			streamCancel()
			for t := range turnCh {
				transcript.RenderTurn(out, th, t)
			}
			// toolCh is nil once its stream has already closed; ranging a
			// nil channel blocks forever, so drain it only while live.
			for toolCh != nil {
				e, ok := <-toolCh
				if !ok {
					break
				}
				transcript.RenderToolSessionEntry(out, th, e)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isTerminalPhase(p string) bool {
	return p == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		p == spiceboxv1alpha1.AgentSessionPhaseFailed
}
