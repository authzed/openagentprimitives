package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentShowCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of an AgentClass",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var ac spiceboxv1alpha1.AgentClass
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &ac); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Name:           %s\n", ac.Name)
			fmt.Fprintf(out, "Namespace:      %s\n", ac.Namespace)
			fmt.Fprintf(out, "Description:    %s\n", ac.Spec.Description)

			// Model
			fmt.Fprintf(out, "Model:\n")
			if ac.Spec.Model != nil {
				fmt.Fprintf(out, "  Provider:     %s\n", ac.Spec.Model.Provider)
				fmt.Fprintf(out, "  Name:         %s\n", ac.Spec.Model.Name)
				fmt.Fprintf(out, "  APIKey:       secret=%s key=%s\n", ac.Spec.Model.APIKey.Name, ac.Spec.Model.APIKey.Key)
			} else {
				fmt.Fprintf(out, "  Provider:     (inherited)\n")
				fmt.Fprintf(out, "  Name:         (inherited)\n")
				if ac.Status.EffectiveSettings != nil {
					fmt.Fprintf(out, "  Effective:    %s/%s\n", ac.Status.EffectiveSettings.Model.Provider, ac.Status.EffectiveSettings.Model.Name)
				}
			}

			// SystemPrompt
			fmt.Fprintf(out, "SystemPrompt:\n")
			if ac.Spec.SystemPrompt.Inline != "" {
				preview := ac.Spec.SystemPrompt.Inline
				if len(preview) > 80 {
					preview = preview[:80] + "..."
				}
				fmt.Fprintf(out, "  Source:       inline (%d chars)\n", len(ac.Spec.SystemPrompt.Inline))
				fmt.Fprintf(out, "  Preview:      %s\n", preview)
			} else if ac.Spec.SystemPrompt.ConfigMapRef != nil {
				fmt.Fprintf(out, "  Source:       configMap=%s key=%s\n", ac.Spec.SystemPrompt.ConfigMapRef.Name, ac.Spec.SystemPrompt.ConfigMapRef.Key)
			} else {
				fmt.Fprintf(out, "  Source:       (none)\n")
			}

			// AgentIdentity
			agentID := ac.Spec.AgentIdentity
			if agentID == "" {
				agentID = "(none)"
			}
			fmt.Fprintf(out, "AgentIdentity:  %s\n", agentID)

			// ToolBundles
			fmt.Fprintf(out, "ToolBundles:    %d\n", len(ac.Spec.ToolBundles))
			for i, tb := range ac.Spec.ToolBundles {
				fmt.Fprintf(out, "  [%d] name=%s class=%s toolspecs=%v", i, tb.Name, tb.Class, tb.Toolspecs)
				if tb.AgentIdentity != "" {
					fmt.Fprintf(out, " identity=%s", tb.AgentIdentity)
				}
				fmt.Fprintln(out)
			}

			// Budget
			fmt.Fprintf(out, "Budget:\n")
			if ac.Spec.Budget != nil {
				fmt.Fprintf(out, "  MaxTurns:     %d\n", ac.Spec.Budget.MaxTurns)
				fmt.Fprintf(out, "  MaxTokens:    %d\n", ac.Spec.Budget.MaxTokens)
				fmt.Fprintf(out, "  MaxDuration:  %s\n", ac.Spec.Budget.MaxDuration.Duration)
			} else {
				fmt.Fprintf(out, "  MaxTurns:     (inherited)\n")
				fmt.Fprintf(out, "  MaxTokens:    (inherited)\n")
				fmt.Fprintf(out, "  MaxDuration:  (inherited)\n")
				if ac.Status.EffectiveSettings != nil {
					eb := ac.Status.EffectiveSettings.Budget
					fmt.Fprintf(out, "  Effective:    maxTurns=%d maxTokens=%d maxDuration=%s\n",
						eb.MaxTurns, eb.MaxTokens, eb.MaxDuration.Duration)
				}
			}

			// Conditions
			fmt.Fprintln(out, "Conditions:")
			for _, c := range ac.Status.Conditions {
				fmt.Fprintf(out, "  %s=%s reason=%s message=%s\n", c.Type, c.Status, c.Reason, c.Message)
			}
			return nil
		},
	}
}
