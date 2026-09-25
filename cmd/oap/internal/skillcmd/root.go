// Package skillcmd implements `oap skill`: managing Skills and SkillSources,
// namespaced or cluster-scoped.
package skillcmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// NewCmd is the `oap skill` group: manage agent Skills and SkillSources
// (and their cluster-scoped variants via --cluster).
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "List, view, create, and delete agent Skills and SkillSources",
		Long: "Manage agent skills.\n\n" +
			"A SkillSource pulls SKILL.md files from a git repo and materializes Skills.\n" +
			"Skills can also be hand-authored from a local SKILL.md directory (instruction-only).\n" +
			"Use --cluster on any subcommand to target the cluster-scoped ClusterSkill/ClusterSkillSource.",
	}
	cmd.AddCommand(newSkillListCmd(g))
	cmd.AddCommand(newSkillShowCmd(g))
	cmd.AddCommand(newSkillCreateCmd(g))
	cmd.AddCommand(newSkillDeleteCmd(g))
	cmd.AddCommand(newSkillSourceCmd(g))
	return cmd
}

// validCondition extracts the Valid condition's status + reason from a skill's
// conditions (Skill and ClusterSkill share the same condition shape).
func validCondition(conds []metav1.Condition) (status, reason string) {
	status = "Unknown"
	for _, c := range conds {
		if c.Type == "Valid" {
			status = string(c.Status)
			if c.Status != metav1.ConditionTrue {
				reason = c.Reason
			}
		}
	}
	return status, reason
}

// pinnedCondition extracts the Pinned condition's status (frozen/named → True,
// unpinned → False).
func pinnedCondition(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Type == "Pinned" {
			return string(c.Status)
		}
	}
	return ""
}

// newSkillTable returns a table with the standard Skill columns. Both the
// namespaced and cluster-scoped listings render through it, so the two can never
// grow different columns.
func newSkillTable(th *tui.Theme) *tui.Table {
	return tui.NewTable(th, "CANONICAL NAME", "NAME", "VALID", "PINNED", "RESOLVED SHA", "AGE")
}

// skillRow renders one Skill/ClusterSkill row. resolvedSHA is "" when the skill
// has no git provenance (hand-authored).
func skillRow(t *tui.Table, canonical, metaName, valid, pinned, sha string, created time.Time) {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	t.Row(canonical, metaName, valid, pinned, sha, apcmd.DurationSinceShort(created))
}

// printConditionsBlock prints a "Conditions:" block for a skill/source detail view.
func printConditionsBlock(out io.Writer, conds []metav1.Condition) {
	fmt.Fprintln(out, "Conditions:")
	uihelpers.PrintConditions(out, conds, "  ")
}
