package agentcmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
)

// newAgentPackageCmd authors a .oap agent container from either a folder
// source (a directory laid out as an .oap source: oap.yaml + manifests/) or a
// live AgentClass on the cluster, named by argument. Folder-vs-name dispatch is by
// os.Stat, matching loadBundle's folder-vs-file dispatch in agent_inspect.go —
// content/shape decides, never the string's look.
func newAgentPackageCmd(g *apcmd.Globals) *cobra.Command {
	var out string

	cmd := &cobra.Command{
		Use:   "package <agentclass-name-or-folder>",
		Short: "Author a .oap agent container from a folder source or a live AgentClass",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := args[0]

			var src source.Source
			if info, statErr := os.Stat(arg); statErr == nil && info.IsDir() {
				src = source.OpenFolder(arg)
			} else {
				b, err := g.Bundle()
				if err != nil {
					return fmt.Errorf("connect to cluster to resolve AgentClass %q: %w", arg, err)
				}
				// b.Namespace already resolves the root persistent -n flag
				// (g.Namespace), falling back to the kube context's namespace.
				src = source.OpenCluster(b.Controller, b.Namespace, arg)
			}

			bundle, err := src.Bundle(cmd.Context())
			if err != nil {
				return fmt.Errorf("build bundle for %q: %w", arg, err)
			}

			packed, err := oap.Pack(bundle)
			if err != nil {
				return fmt.Errorf("pack %q: %w", arg, err)
			}

			dig, err := oap.Digest(packed)
			if err != nil {
				return fmt.Errorf("digest packed %q: %w", arg, err)
			}

			outPath := out
			if outPath == "" {
				outPath, err = defaultPackageOutPath(bundle.Manifest.Agent.Name)
				if err != nil {
					return err
				}
			}

			if outPath == "-" {
				if _, err := cmd.OutOrStdout().Write(packed); err != nil {
					return fmt.Errorf("write packed bundle to stdout: %w", err)
				}
			} else if err := os.WriteFile(outPath, packed, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}

			// Digest + output path go to stderr (not stdout) so `-o -` piping
			// stays clean.
			fmt.Fprintf(cmd.ErrOrStderr(), "%s  %s\n", dig, outPath)
			return nil
		},
	}

	cmd.Flags().StringVarP(&out, "output", "o", "", "Output path for the .oap ('-' for stdout; default: <agent-name>"+oap.DefaultExtension+" in the current directory)")
	return cmd
}

// defaultPackageOutPath derives the default .oap filename from the bundle's
// agent name, when no -o was given, reduced to a single path element so the
// file always lands in the current directory.
//
// The name is authored by the BUNDLE, not the invoking user: it comes from the
// folder source's own oap.yaml or from a live AgentClass, and Manifest.Validate
// only requires it to be non-empty — there is no charset or path-element rule.
// Concatenated straight into the os.WriteFile path, a name like
// "../../../../etc/cron.d/x" made `oap agent package ./some-cloned-folder` write
// wherever the invoking user can write, at a path of the bundle author's
// choosing. Same class as the desktop README traversal readmeTempPath closes.
//
// A name that reduces to nothing usable is an ERROR here, where readmeTempPath
// falls back to a placeholder: the menu-bar preview has no way to fail a
// dialog, but a CLI must refuse loudly rather than silently write an artifact
// under a name the user never asked for.
func defaultPackageOutPath(name string) (string, error) {
	base := filepath.Base(filepath.Clean(name))
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", fmt.Errorf("agent name %q yields no usable output filename; pass -o to name the file", name)
	}
	return base + oap.DefaultExtension, nil
}
