package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/sessioncmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// terminalAgentSessionPhases are the phases that mean the session is
// no longer making progress; logs are still readable but not "current."
var terminalAgentSessionPhases = map[string]bool{
	spiceboxv1alpha1.AgentSessionPhaseFailed:    true,
	spiceboxv1alpha1.AgentSessionPhaseSucceeded: true,
}

// activeAgentSessionPhases are the phases where the session is actually
// doing work right now (or imminently expected to). `--follow-live`
// uses this stricter filter — `Idle` channel-attached sessions are
// dormant until the next user message, so following them adds no
// signal until they transition back to `Running`. The single-shot
// `agent logs` command still treats every non-terminal phase as a
// valid candidate (including Idle), since the user asked for a
// specific session by name in that mode.
var activeAgentSessionPhases = map[string]bool{
	spiceboxv1alpha1.AgentSessionPhasePending:          true,
	spiceboxv1alpha1.AgentSessionPhaseRunning:          true,
	spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval: true,
	spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision: true,
}

func newAgentLogsCmd(g *apcmd.Globals) *cobra.Command {
	var (
		follow      bool
		followLive  bool
		timeout     time.Duration
		includeDone bool
	)
	cmd := &cobra.Command{
		Use:   "logs <agent-class>",
		Short: "Stream memory turns for the current single AgentSession of an AgentClass",
		Long: `Convenience wrapper for the common debugging case: when an AgentClass has
exactly one in-flight (non-terminal) AgentSession, dump or follow its
memory turns.

If zero or more than one matching session exists, the command prints
the candidates and exits non-zero — fall back to ` + "`oap session logs <name>`" + ` to
pick one explicitly.

By default Failed/Succeeded sessions are excluded so that "the current
session" is unambiguous. Pass --include-done to consider every session
that references the AgentClass.

--follow-live keeps the channel-bound debug loop alive across sessions
indefinitely. "Live" here means actively making progress — phase
Running, Pending, or AwaitingApproval. Idle channel-attached sessions
(dormant between user messages) are intentionally excluded so the
loop doesn't sit on a thread that isn't doing anything.
  * 0 active sessions → poll quietly until one appears.
  * exactly 1 active → stream it; when it transitions out of an
    active phase (becomes Idle, Failed, or Succeeded), return to the
    poll loop and pick up the next active session for the same class.
  * more than 1 active → print the candidate list, then KEEP polling.
    When the set collapses back to a single active session, switch
    to streaming it. The list is only re-printed when the set
    changes, so the loop stays quiet between updates.
The only exit paths are Ctrl-C and the --timeout cap.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			className := args[0]
			if followLive {
				return runAgentLogsFollowLive(cmd.Context(), cmd.OutOrStdout(), g, className, timeout)
			}
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.AgentSessionList
			if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
				return err
			}
			candidates := make([]spiceboxv1alpha1.AgentSession, 0, len(list.Items))
			for _, s := range list.Items {
				if s.Spec.Class != className {
					continue
				}
				if !includeDone && terminalAgentSessionPhases[s.Status.Phase] {
					continue
				}
				candidates = append(candidates, s)
			}
			switch len(candidates) {
			case 0:
				if includeDone {
					return fmt.Errorf("no AgentSessions found for AgentClass %q in namespace %s",
						className, b.Namespace)
				}
				return fmt.Errorf("no in-flight AgentSessions for AgentClass %q in namespace %s; pass --include-done to look at terminal sessions",
					className, b.Namespace)
			case 1:
				return sessioncmd.RunLogs(cmd.Context(), cmd.OutOrStdout(), g, candidates[0].Name, follow, timeout)
			default:
				sort.Slice(candidates, func(i, j int) bool {
					return candidates[i].CreationTimestamp.Time.Before(candidates[j].CreationTimestamp.Time)
				})
				var lines []string
				for _, s := range candidates {
					lines = append(lines, fmt.Sprintf("  %s\t%s\t%s", s.Name, s.Status.Phase, s.CreationTimestamp.Format(time.RFC3339)))
				}
				return fmt.Errorf("found %d candidate AgentSessions for AgentClass %q in namespace %s — pick one with `oap session logs <name>`:\n%s",
					len(candidates), className, b.Namespace, strings.Join(lines, "\n"))
			}
		},
	}
	apcmd.FollowFlags(cmd, &follow, &timeout)
	cmd.Flags().BoolVar(&followLive, "follow-live", false, "Watch for live sessions of the AgentClass indefinitely; stream when exactly one exists, print + refresh the candidate list when more than one exists, switch back to streaming when it collapses to one.")
	cmd.Flags().BoolVar(&includeDone, "include-done", false, "Include Failed and Succeeded sessions when picking the candidate")
	return cmd
}

// activeAgentSessionsForClass lists in-namespace AgentSessions whose
// spec.class == className and whose phase is in activeAgentSessionPhases
// (Running / Pending / AwaitingApproval) — i.e. actively making
// progress, not just non-terminal. Idle and the two terminal phases
// are filtered out. The result is sorted oldest-first so the printed
// candidate list reads in chronological order.
func activeAgentSessionsForClass(ctx context.Context, b *kube.Bundle, className string) ([]spiceboxv1alpha1.AgentSession, error) {
	var list spiceboxv1alpha1.AgentSessionList
	if err := b.Controller.List(ctx, &list, client.InNamespace(b.Namespace)); err != nil {
		return nil, err
	}
	out := make([]spiceboxv1alpha1.AgentSession, 0, len(list.Items))
	for _, s := range list.Items {
		if s.Spec.Class != className {
			continue
		}
		if !activeAgentSessionPhases[s.Status.Phase] {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreationTimestamp.Time.Before(out[j].CreationTimestamp.Time)
	})
	return out, nil
}

// runAgentLogsFollowLive implements --follow-live: poll for live
// sessions; stream the unique one when present; on terminal transition
// loop back to discover the next live session; abort with the candidate
// list whenever the count exceeds 1.
//
// The "switch sessions automatically" UX is exactly the debug loop the
// channel-attached agent flow needs: send a message, watch its session,
// the session ends Idle/Failed, send another message, watch the new
// session — all without manually `oap session logs <name>`ing each one.
func runAgentLogsFollowLive(ctx context.Context, out io.Writer, g *apcmd.Globals, className string, timeout time.Duration) error {
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	// Capabilities come from the stream this command writes to, so a pipe,
	// --no-color and NO_COLOR each land on the colorless theme.
	th := g.Theme(out)

	// `seen` tracks sessions we already streamed so we don't replay the
	// same one if it briefly re-appears in the list before terminal state
	// propagates through the watcher. Bounded growth: one entry per
	// session, dropped when the AgentSession is deleted.
	seen := map[string]struct{}{}

	// lastMultiSig is the previously-printed multi-live candidate set
	// (encoded by multiLiveSignature). The loop re-prints the candidate
	// list only when this signature changes, so a long-running ambiguous
	// state doesn't spam the terminal.
	var lastMultiSig string

	const pollInterval = 2 * time.Second

	fmt.Fprintln(out, th.Render(th.Subtle, fmt.Sprintf(
		"watching live AgentSessions for class %q in namespace %s (Ctrl-C to stop)…",
		className, b.Namespace)))

	for {
		live, err := activeAgentSessionsForClass(ctx, b, className)
		if err != nil {
			return err
		}
		// Drop sessions we've already streamed (avoids re-streaming a
		// session whose phase transition hasn't reached our cache yet).
		fresh := live[:0]
		for _, s := range live {
			if _, ok := seen[string(s.UID)]; ok {
				continue
			}
			fresh = append(fresh, s)
		}

		switch len(fresh) {
		case 0:
			// No new live session right now — wait and re-poll. The
			// channel-attached flow can take a moment to spawn a new
			// session after a user message lands.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
				continue
			}
		case 1:
			s := fresh[0]
			seen[string(s.UID)] = struct{}{}
			fmt.Fprintln(out, th.Render(th.Title,
				fmt.Sprintf("── live session %s (phase=%s, created %s) ──",
					s.Name, s.Status.Phase, s.CreationTimestamp.Format(time.RFC3339))))

			// sessioncmd.RunLogs(follow=true) only exits on Failed/Succeeded
			// (per isTerminalPhase). For follow-live we ALSO want to
			// release the stream when the session goes Idle so the loop
			// can pick up the next active session. Run a watcher that
			// cancels streamCtx as soon as the session leaves the
			// active set.
			streamCtx, cancelStream := context.WithCancel(ctx)
			watchStop := make(chan struct{})
			go func() {
				defer close(watchStop)
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
					}
					var fresh spiceboxv1alpha1.AgentSession
					if err := b.Controller.Get(streamCtx, client.ObjectKey{
						Namespace: b.Namespace, Name: s.Name,
					}, &fresh); err != nil {
						continue
					}
					if !activeAgentSessionPhases[fresh.Status.Phase] {
						cancelStream()
						return
					}
				}
			}()

			err := sessioncmd.RunLogs(streamCtx, out, g, s.Name, true, timeout)
			cancelStream()
			<-watchStop
			if err != nil {
				// streamCtx.Err() means the watcher cancelled (phase
				// transitioned out of active) — that's the success
				// path for follow-live, not an error to surface.
				if errors.Is(err, context.Canceled) && ctx.Err() == nil {
					// only the inner stream ctx is done — outer ctx
					// is still live; fall through and loop.
				} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				} else {
					// Don't abort the watch loop on per-session errors —
					// surface and keep looking for the next session.
					fmt.Fprintln(out, th.Render(th.Warn, fmt.Sprintf(
						"session %s: %v (continuing to watch for next active session)", s.Name, err)))
				}
			}
			// Loop back to discover the next active session.
		default:
			// More than one live session — print the candidate list
			// (only when it has changed since last poll) and keep
			// watching. When the set collapses back to a single live
			// session the outer loop picks it up and starts streaming.
			signature := multiLiveSignature(fresh)
			if signature != lastMultiSig {
				lines := make([]string, 0, len(fresh))
				for _, s := range fresh {
					lines = append(lines, fmt.Sprintf("  %s\t%s\t%s",
						s.Name, s.Status.Phase, s.CreationTimestamp.Format(time.RFC3339)))
				}
				fmt.Fprintln(out, th.Render(th.Warn, fmt.Sprintf(
					"multiple live AgentSessions (%d) for AgentClass %q — waiting for the set to collapse to one. Pick one explicitly with `oap session logs <name>` if you don't want to wait:\n%s",
					len(fresh), className, strings.Join(lines, "\n"))))
				lastMultiSig = signature
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
				continue
			}
		}
	}
}

// multiLiveSignature stable-encodes the multi-live candidate set so
// runAgentLogsFollowLive only re-prints when the set actually changes.
// liveAgentSessionsForClass already returns sessions sorted by creation
// time, so a deterministic name-phase concatenation is enough.
func multiLiveSignature(sessions []spiceboxv1alpha1.AgentSession) string {
	parts := make([]string, 0, len(sessions))
	for _, s := range sessions {
		parts = append(parts, s.Name+":"+s.Status.Phase)
	}
	return strings.Join(parts, ",")
}
