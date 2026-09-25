package agentcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memstream"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/transcript"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentRunCmd(g *apcmd.Globals) *cobra.Command {
	var (
		prompt          string
		promptFile      string
		sessionName     string
		budgetTokens    int64
		budgetTurns     int32
		budgetDuration  time.Duration
		noTail          bool
		cleanupOnCancel bool
		timeout         time.Duration
	)

	cmd := &cobra.Command{
		Use:   "run <agent-class>",
		Short: "Apply a session against an AgentClass and live-stream memory turns",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentRun(cmd.Context(), cmd.OutOrStdout(), g, args[0],
				prompt, promptFile, sessionName, budgetTokens, budgetTurns, budgetDuration,
				noTail, cleanupOnCancel, timeout)
		},
	}
	cmd.Flags().StringVar(&prompt, "prompt", "", "Inline prompt (conflicts with --prompt-file)")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "", "Read prompt from file (or '-' for stdin)")
	apcmd.SessionNameFlag(cmd, &sessionName)
	cmd.Flags().Int64Var(&budgetTokens, "budget-tokens", 0, "Per-session max tokens")
	cmd.Flags().Int32Var(&budgetTurns, "budget-turns", 0, "Per-session max turns")
	cmd.Flags().DurationVar(&budgetDuration, "budget-duration", 0, "Per-session max wall-time")
	cmd.Flags().BoolVar(&noTail, "no-tail", false, "Apply + exit; don't stream")
	cmd.Flags().BoolVar(&cleanupOnCancel, "cleanup-on-cancel", false, "Delete session on Ctrl-C")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "Client-side cap for the entire run")
	return cmd
}

func runAgentRun(
	ctx context.Context, out io.Writer, g *apcmd.Globals, agentName,
	prompt, promptFile, sessionName string,
	budgetTokens int64, budgetTurns int32, budgetDuration time.Duration,
	noTail, cleanupOnCancel bool,
	timeout time.Duration,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// 1. Resolve AgentClass and verify Valid=True.
	var ac spiceboxv1alpha1.AgentClass
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: agentName}, &ac); err != nil {
		return fmt.Errorf("get AgentClass %q: %w", agentName, err)
	}
	if !apcmd.AgentClassIsValid(&ac) {
		return fmt.Errorf("AgentClass %q is not Valid=True yet", agentName)
	}

	// 2. Resolve prompt.
	promptText, err := readPrompt(prompt, promptFile)
	if err != nil {
		return err
	}

	// 3. Build + apply AgentSession.
	if sessionName == "" {
		sessionName = fmt.Sprintf("%s-%s", agentName, uuid.New().String()[:8])
	}
	// TypeMeta is required by server-side apply: Patch+client.Apply needs
	// apiVersion + kind on the wire, which the typed struct only carries
	// when we set them explicitly (the Scheme doesn't auto-populate).
	sess := &spiceboxv1alpha1.AgentSession{
		TypeMeta: metav1.TypeMeta{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "AgentSession",
		},
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: b.Namespace},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  agentName,
			Prompt: spiceboxv1alpha1.PromptSource{Inline: promptText},
		},
	}
	if budgetTurns > 0 || budgetTokens > 0 || budgetDuration > 0 {
		sess.Spec.Budget = &spiceboxv1alpha1.BudgetConfig{
			MaxTurns:    budgetTurns,
			MaxTokens:   budgetTokens,
			MaxDuration: metav1.Duration{Duration: budgetDuration},
		}
	}
	if err := b.Controller.Patch(ctx, sess, client.Apply, client.ForceOwnership, client.FieldOwner("ap-apply")); err != nil {
		return fmt.Errorf("apply AgentSession: %w", err)
	}

	if cleanupOnCancel {
		defer func() {
			if ctx.Err() == context.Canceled {
				_ = b.Controller.Delete(context.Background(), sess)
			}
		}()
	}

	if noTail {
		fmt.Fprintf(out, "applied AgentSession %s/%s; --no-tail set; not streaming\n", b.Namespace, sessionName)
		return nil
	}

	// 4. Wait for the runner-pod-name to populate; then fetch the memory token.
	var memToken string
	err = wait.Until(ctx, 1*time.Second, 60*time.Second, func(ctx context.Context) (bool, error) {
		var fresh spiceboxv1alpha1.AgentSession
		if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &fresh); err != nil {
			return false, err
		}
		if fresh.Status.RunnerPodName == "" {
			return false, nil
		}
		// Fetch the per-session memory-token Secret.
		var sec corev1.Secret
		if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName + "-memory-token"}, &sec); err != nil {
			return false, nil // may not exist yet
		}
		memToken = string(sec.Data["token"])
		return memToken != "", nil
	})
	if err != nil {
		return fmt.Errorf("waiting for runner pod / memory token: %w", err)
	}

	// 5. Port-forward to spicebox-operator:8082.
	pf, err := portforward.New(b.REST, "agentprimitives-system", "spicebox-operator", "app.kubernetes.io/name=spicebox-operator", 8082, 0)
	if err != nil {
		return err
	}
	if err := pf.Start(ctx, io.Discard); err != nil {
		return fmt.Errorf("port-forward: %w", err)
	}
	defer pf.Stop()

	// 6. Stream memory turns + watch session phase. Capabilities come from the
	// stream this command writes to, so a pipe, --no-color and NO_COLOR each
	// land on the colorless theme.
	th := g.Theme(out)
	streamer := memstream.New(pf.URL(), b.Namespace, sessionName, memToken, 1*time.Second)
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	turnCh, errCh := streamer.Stream(streamCtx)

	phaseDone := make(chan string, 1)
	go func() {
		_ = wait.Until(streamCtx, 1*time.Second, timeout, func(ctx context.Context) (bool, error) {
			var fresh spiceboxv1alpha1.AgentSession
			if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &fresh); err != nil {
				return false, err
			}
			if fresh.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
				fresh.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed {
				phaseDone <- fresh.Status.Phase
				return true, nil
			}
			return false, nil
		})
	}()

	for {
		select {
		case t, ok := <-turnCh:
			if !ok {
				goto terminal
			}
			transcript.RenderTurn(out, th, t)
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(out, th.Render(th.Warn, "stream error: "+err.Error()))
			}
			errCh = nil // deselect this case after the channel closes
		case <-phaseDone:
			// Drain remaining turns then exit.
			streamCancel()
			for t := range turnCh {
				transcript.RenderTurn(out, th, t)
			}
			goto terminal
		case <-ctx.Done():
			return ctx.Err()
		}
	}

terminal:
	var final spiceboxv1alpha1.AgentSession
	_ = b.Controller.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: sessionName}, &final)
	fmt.Fprintln(out)
	fmt.Fprintln(out, th.Render(th.Title, strings.Repeat("─", 60)))
	switch final.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
		fmt.Fprintln(out, th.Render(th.Success, "Succeeded."))
		if final.Status.Result != nil {
			fmt.Fprintf(out, "in=%d out=%d\n", final.Status.Progress.InputTokens, final.Status.Progress.OutputTokens)
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "Result:")
			fmt.Fprintln(out, "  "+final.Status.Result.Summary)
		}
		return nil
	case spiceboxv1alpha1.AgentSessionPhaseFailed:
		fmt.Fprintln(out, th.Render(th.Err, "Failed."))
		fmt.Fprintf(out, "reason=%s\n", final.Status.FailureReason)
		return fmt.Errorf("AgentSession Failed: %s", final.Status.FailureReason)
	}
	return nil
}

func readPrompt(inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", fmt.Errorf("cannot use --prompt and --prompt-file together")
	}
	if inline != "" {
		return inline, nil
	}
	if file != "" {
		if file == "-" {
			data, err := io.ReadAll(os.Stdin)
			return string(data), err
		}
		data, err := os.ReadFile(file)
		return string(data), err
	}
	// Try stdin if it's a pipe.
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		data, err := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(data)), err
	}
	return "", fmt.Errorf("no prompt: pass --prompt, --prompt-file, or pipe via stdin")
}
