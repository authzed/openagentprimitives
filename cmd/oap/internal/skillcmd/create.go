package skillcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillmd"
	skillvalidate "github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
)

func newSkillCreateCmd(g *apcmd.Globals) *cobra.Command {
	var (
		fromDir string
		cluster bool
	)
	cmd := &cobra.Command{
		Use:   "create --from-dir <dir>",
		Short: "Hand-author a Skill from a local SKILL.md directory (instruction-only)",
		Long: "Reads <dir>/SKILL.md, validates its frontmatter, and creates an\n" +
			"instruction-only Skill with a reserved local:// canonical name\n" +
			"(local/<namespace>//<name>, or local//<name> with --cluster).\n\n" +
			"For executable skills (scripts/assets) or git-sourced skills, use a\n" +
			"SkillSource instead (`oap skill source create`); scripts/ and assets/ in\n" +
			"the directory are NOT bundled here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromDir == "" {
				return errors.New("--from-dir is required")
			}
			raw, err := os.ReadFile(filepath.Join(fromDir, "SKILL.md"))
			if err != nil {
				return fmt.Errorf("read SKILL.md: %w", err)
			}
			doc, err := skillmd.Parse(raw)
			if err != nil {
				return fmt.Errorf("parse SKILL.md: %w", err)
			}

			b, err := g.Bundle()
			if err != nil {
				return err
			}

			// Build the reserved local-authority canonical name. Hand-authored
			// skills must use a local// name (git-authority names are reserved
			// for SkillSource-materialized skills — see validate.CheckProvenance).
			var canonicalName string
			if cluster {
				canonicalName = "local//" + doc.Frontmatter.Name
			} else {
				canonicalName = "local/" + b.Namespace + "//" + doc.Frontmatter.Name
			}

			if errs := skillvalidate.Skill(canonicalName, doc.Frontmatter.Name, doc.Frontmatter.Description, doc.Body); len(errs) > 0 {
				return fmt.Errorf("invalid SKILL.md: %v", errs[0])
			}
			if err := skillvalidate.CheckProvenance(canonicalName, false); err != nil {
				return err
			}

			n, err := canonical.Parse(canonicalName)
			if err != nil {
				return err
			}
			spec := spiceboxv1alpha1.SkillSpec{
				CanonicalName: canonicalName,
				DisplayName:   doc.Frontmatter.Name,
				Description:   doc.Frontmatter.Description,
				Body:          doc.Body,
				Frontmatter: spiceboxv1alpha1.SkillFrontmatter{
					Name:          doc.Frontmatter.Name,
					License:       doc.Frontmatter.License,
					Compatibility: doc.Frontmatter.Compatibility,
					Metadata:      doc.Frontmatter.Metadata,
					AllowedTools:  doc.Frontmatter.AllowedTools,
				},
			}

			out := cmd.OutOrStdout()
			if warnIfBundleFiles(out, fromDir) {
				fmt.Fprintln(out, "  (use `oap skill source create` for executable, git-sourced skills)")
			}

			if cluster {
				obj := &spiceboxv1alpha1.ClusterSkill{Spec: spec}
				obj.Name = n.SafeSlug()
				if err := b.Controller.Create(cmd.Context(), obj); err != nil {
					return err
				}
				fmt.Fprintf(out, "created ClusterSkill %s (%s)\n", obj.Name, canonicalName)
				return nil
			}
			obj := &spiceboxv1alpha1.Skill{Spec: spec}
			obj.Namespace = b.Namespace
			obj.Name = n.SafeSlug()
			if err := b.Controller.Create(cmd.Context(), obj); err != nil {
				return err
			}
			fmt.Fprintf(out, "created Skill %s/%s (%s)\n", b.Namespace, obj.Name, canonicalName)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromDir, "from-dir", "", "Directory containing the SKILL.md to author from")
	cmd.Flags().BoolVar(&cluster, "cluster", false, "Create a cluster-scoped ClusterSkill")
	return cmd
}

// warnIfBundleFiles prints a note when the directory carries scripts/ or assets/
// that the CLI does not bundle. Returns true if it warned.
func warnIfBundleFiles(out io.Writer, dir string) bool {
	for _, sub := range []string{"scripts", "assets"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err == nil && info.IsDir() {
			fmt.Fprintf(out, "note: %s/ is present but NOT bundled (instruction-only skill)\n", sub)
			return true
		}
	}
	return false
}
