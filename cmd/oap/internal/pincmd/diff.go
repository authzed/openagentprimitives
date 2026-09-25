package pincmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcppin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func newPinDiffCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <kind> <name>",
		Short: "Compare the recorded pin baseline against the live identity",
		Long: `Compare the recorded pin baseline against the live dependency identity.

Supported kinds:
  mcp    — probe tools/list unauthenticated and compare hashes + tool names
  image  — print the PinDrift condition (live signal from the toolbox reconciler)
  skill  — print declared identity; diff is N/A (identity declared in spec)
  cli    — print declared identity; diff is N/A (identity declared in spec)`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kindArg := strings.ToLower(args[0])
			name := args[1]

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			switch kindArg {
			case "mcp":
				var srv spiceboxv1alpha1.MCPServer
				if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &srv); err != nil {
					return fmt.Errorf("get MCPServer %q: %w", name, err)
				}

				// Print baseline info.
				fmt.Fprintf(out, "MCPServer: %s\n", name)
				if srv.Status.Pin == nil {
					fmt.Fprintln(out, "Baseline:  (not yet recorded — no reconcile observed)")
				} else {
					fmt.Fprintf(out, "Baseline digest:  %s\n", srv.Status.Pin.Digest)
					fmt.Fprintf(out, "Baseline version: %s\n", srv.Status.Pin.Version)
					if srv.Status.Pin.ObservedAt != nil {
						fmt.Fprintf(out, "Baseline at:      %s\n", humanizeTime(srv.Status.Pin.ObservedAt.Time))
					}
				}
				fmt.Fprintln(out, "")

				// Probe live tools/list unauthenticated.
				pc := &probe.Client{URL: srv.Spec.Server.URL}
				liveTools, probeErr := pc.ListTools(ctx, "", "")
				if probeErr != nil {
					// Print the probe error with an auth hint.
					fmt.Fprintln(out, "Live digest: UNKNOWN (probe failed)")
					fmt.Fprintf(out, "Live probe error: %v\n", probeErr)
					fmt.Fprintln(out, "Hint: this server may require authentication.")
					fmt.Fprintln(out, "      Use `oap pin update mcp <name> --to <hash>` and pass the hash from")
					fmt.Fprintln(out, "      a session's observedPins (oap agent sessions / oap session show).")

					// Surface the operator-side PinDrift condition as additional context.
					if c := meta.FindStatusCondition(srv.Status.Conditions, spiceboxv1alpha1.PinDriftCondition); c != nil {
						fmt.Fprintf(out, "\nOperator PinDrift condition:\n  Reason:  %s\n  Message: %s\n", c.Reason, c.Message)
					}
					return nil
				}

				liveHash, err := mcppin.CanonicalManifestHash(liveTools)
				if err != nil {
					return fmt.Errorf("compute live hash: %w", err)
				}
				fmt.Fprintf(out, "Live digest: %s\n", liveHash)

				if srv.Status.Pin != nil {
					if liveHash == srv.Status.Pin.Digest {
						fmt.Fprintln(out, "Status: no drift (baseline == live)")
					} else {
						fmt.Fprintln(out, "Status: DRIFT DETECTED")
					}
					fmt.Fprintln(out, "")
					printToolNameDiff(out, srv.Status.ObservedTools, liveTools)
				} else {
					fmt.Fprintln(out, "Status: no baseline recorded yet — next reconcile will set TOFU baseline")
				}

			case "image":
				var sidecar spiceboxv1alpha1.SidecarToolbox
				if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &sidecar); err != nil {
					return fmt.Errorf("get SidecarToolbox %q: %w", name, err)
				}

				fmt.Fprintf(out, "SidecarToolbox: %s\n", name)
				if sidecar.Status.Pin == nil {
					fmt.Fprintln(out, "Baseline: (not yet recorded — no reconcile observed)")
				} else {
					fmt.Fprintf(out, "Baseline digest:  %s\n", sidecar.Status.Pin.Digest)
					fmt.Fprintf(out, "Baseline version: %s\n", sidecar.Status.Pin.Version)
					if sidecar.Status.Pin.ObservedAt != nil {
						fmt.Fprintf(out, "Baseline at:      %s\n", humanizeTime(sidecar.Status.Pin.ObservedAt.Time))
					}
				}
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, "Live identity comes from the toolbox reconciler (the CLI cannot pull images).")
				if c := meta.FindStatusCondition(sidecar.Status.Conditions, spiceboxv1alpha1.PinDriftCondition); c != nil {
					fmt.Fprintf(out, "PinDrift condition:\n  Reason:  %s\n  Message: %s\n", c.Reason, c.Message)
				} else {
					fmt.Fprintln(out, "PinDrift condition: (not yet set)")
				}

			case "skill":
				// Skills are pinned by spec (git SHA / canonical name); diff is N/A.
				// Try namespaced Skill first, then fall back to ClusterSkill on NotFound.
				var skill spiceboxv1alpha1.Skill
				nsErr := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, &skill)
				if nsErr != nil && !apierrors.IsNotFound(nsErr) {
					return fmt.Errorf("get Skill %s/%s: %w", b.Namespace, name, nsErr)
				}
				if nsErr == nil {
					fmt.Fprintf(out, "Skill: %s\n", name)
					printSkillPinInfo(out, skill.Status.Pin, skill.Spec.CanonicalName)
				} else {
					// nsErr was NotFound — try cluster-scoped.
					var cskill spiceboxv1alpha1.ClusterSkill
					csErr := b.Controller.Get(ctx, types.NamespacedName{Name: name}, &cskill)
					if csErr != nil && !apierrors.IsNotFound(csErr) {
						return fmt.Errorf("get Skill %s/%s: %w", b.Namespace, name, csErr)
					}
					if csErr != nil {
						// Both were NotFound.
						return fmt.Errorf("no Skill or ClusterSkill named %q found (namespace %q)", name, b.Namespace)
					}
					fmt.Fprintf(out, "ClusterSkill: %s\n", name)
					printSkillPinInfo(out, cskill.Status.Pin, cskill.Spec.CanonicalName)
				}
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, "Note: skill identity is declared in spec (canonical name + source SHA).")
				fmt.Fprintln(out, "      Live diff is N/A — edit the spec or SkillSource to change identity.")

			case "cli":
				var toolkit spiceboxv1alpha1.SpiceboxToolkit
				if err := b.Controller.Get(ctx, types.NamespacedName{Name: name}, &toolkit); err != nil {
					return fmt.Errorf("get SpiceboxToolkit %q: %w", name, err)
				}
				fmt.Fprintf(out, "SpiceboxToolkit: %s\n", name)
				if toolkit.Status.Pin == nil {
					fmt.Fprintln(out, "Pin: (not yet recorded)")
				} else {
					fmt.Fprintf(out, "Strength: %s\n", toolkit.Status.Pin.Strength)
					fmt.Fprintf(out, "Digest:   %s\n", toolkit.Status.Pin.Digest)
					fmt.Fprintf(out, "Version:  %s\n", toolkit.Status.Pin.Version)
				}
				fmt.Fprintln(out, "")
				fmt.Fprintln(out, "Note: CLI toolkit identity is declared in spec (pinned binary hash or version range).")
				fmt.Fprintln(out, "      Live diff is N/A — edit the spec to change the declared identity.")

			default:
				return fmt.Errorf("unsupported kind %q; must be one of: mcp, image, skill, cli", kindArg)
			}

			return nil
		},
	}
}

// printToolNameDiff prints the added/removed tool names between the recorded
// baseline (observedTools) and the live tools list. Both are compared as sets.
// No output is printed when there is no difference.
func printToolNameDiff(out io.Writer, baseline []string, live []probe.Tool) {
	baselineSet := make(map[string]struct{}, len(baseline))
	for _, n := range baseline {
		baselineSet[n] = struct{}{}
	}
	liveSet := make(map[string]struct{}, len(live))
	for _, t := range live {
		liveSet[t.Name] = struct{}{}
	}

	var added, removed []string
	for _, t := range live {
		if _, ok := baselineSet[t.Name]; !ok {
			added = append(added, t.Name)
		}
	}
	for _, n := range baseline {
		if _, ok := liveSet[n]; !ok {
			removed = append(removed, n)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	if len(added) == 0 && len(removed) == 0 {
		return
	}
	fmt.Fprintln(out, "Tool name diff:")
	for _, n := range added {
		fmt.Fprintf(out, "  + %s\n", n)
	}
	for _, n := range removed {
		fmt.Fprintf(out, "  - %s\n", n)
	}
}

// printSkillPinInfo prints the pin record fields for a Skill / ClusterSkill.
func printSkillPinInfo(out io.Writer, pin *spiceboxv1alpha1.PinRecord, canonical string) {
	fmt.Fprintf(out, "Canonical name: %s\n", canonical)
	if pin == nil {
		fmt.Fprintln(out, "Pin: (not yet recorded)")
		return
	}
	fmt.Fprintf(out, "Strength: %s\n", pin.Strength)
	fmt.Fprintf(out, "Digest:   %s\n", pin.Digest)
	fmt.Fprintf(out, "Version:  %s\n", pin.Version)
}
