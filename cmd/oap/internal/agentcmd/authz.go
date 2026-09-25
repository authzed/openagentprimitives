package agentcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// newAgentAuthzCmd renders the effective, ordered, active authz hook pipeline
// for an AgentClass. Read-only. The active/order decision is derived from the
// SHARED hooks.ActiveHooks table (the same one the runner's
// buildPipelineRegistry consults) so the view is a faithful projection of the
// real registry, not a hand-maintained duplicate.
func newAgentAuthzCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "authz <name>",
		Short: "Show the effective, ordered authz hook pipeline for an AgentClass",
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
			return renderAuthzView(out, g.Theme(out), &ac)
		},
	}
}

// activationConfigFromClass projects an AgentClass into the neutral
// hooks.ActivationConfig the shared table consumes. Mirrors the runner's
// (*Loop).activationConfig so the CLI and registry cannot drift.
func activationConfigFromClass(ac *spiceboxv1alpha1.AgentClass) hooks.ActivationConfig {
	authz := ac.Spec.GetAuthz()
	scope := ac.Spec.GetScope()
	cfg := hooks.ActivationConfig{
		ScopeEnabled: scope.Enabled,
		ColdStart:    scope.ColdStart,
		// The EFFECTIVE policy, not the declared field: a webhook-driven class
		// never declares one (the only sensible value is the per-install id of
		// the channel it posts into) and has it derived onto status instead.
		// Reading the declared half alone would report the gate off on exactly
		// the classes it is switched on for.
		InteractPermission: ac.EffectiveSessionInteractPermission(),
		BoundEntityCount:   len(ac.Spec.GetSlots()),
	}
	if tc := authz.GetToolCalls(); tc != nil {
		cfg.ToolCallMode = tc.Mode
	}
	cfg.LeakageMode = authz.InformationLeakage.ResolvedMode()
	return cfg
}

// renderAuthzView writes the ordered, active hook list for ac to out.
func renderAuthzView(out io.Writer, th *tui.Theme, ac *spiceboxv1alpha1.AgentClass) error {
	authz := ac.Spec.GetAuthz()
	fmt.Fprintf(out, "AgentClass:      %s/%s\n", ac.Namespace, ac.Name)
	fmt.Fprintf(out, "ApprovalTimeout: %s (authz.approvalTimeout; covers tool-call, leakage-share, and cold-start scope approvals)\n",
		authz.ResolvedApprovalTimeout())

	// Who may interact, and whether anybody chose them. A derived subject-set is
	// the membership of the channel the agent posts into, and it exists on
	// status only — an operator reading the spec would find nothing at all and
	// conclude the gate was unconfigured. Saying which of the two it is matters
	// as much as the value: "declared" is a decision to review, "derived" is a
	// default to notice.
	if perm := ac.EffectiveSessionInteractPermission(); perm != "" {
		origin := "declared"
		if authz.GetSession().InteractPermission == "" {
			origin = "derived from the output channel's membership"
		}
		fmt.Fprintf(out, "Interact:        %s (%s)\n", perm, origin)
	}
	fmt.Fprintln(out)

	descs := hooks.ActiveHooks(activationConfigFromClass(ac))

	t := tui.NewTable(th, "HOOK", "STATUS", "POINTS", "NOTE")
	for _, d := range descs {
		t.Row(d.Name, d.Active.String(), pointsString(d.Points), d.Note)
	}
	_, err := fmt.Fprint(out, t.Render())
	return err
}

func pointsString(points []pipeline.Point) string {
	parts := make([]string, 0, len(points))
	for _, p := range points {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, ",")
}
