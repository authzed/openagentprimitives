package settingscmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// DynIface is an alias for dynamic.Interface so tests can stub the factory.
// The shared DynamicFactory lives in settings_root.go.
type DynIface = dynamic.Interface

func newSettingsApplyCmd(g *apcmd.Globals) *cobra.Command {
	var file string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Apply a ClusterAgentSettings YAML manifest.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			docs, err := manifests.Split(data)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			// Validate kind once (pre-pass) so the dry-run and wet paths can
			// never diverge on what they accept.
			for _, d := range docs {
				if d.GetKind() != "ClusterAgentSettings" {
					return fmt.Errorf("settings apply: unexpected kind %q — only ClusterAgentSettings is accepted here", d.GetKind())
				}
			}

			if dryRun {
				for _, d := range docs {
					fmt.Fprintf(out, "ClusterAgentSettings/%s (dry-run)\n", d.GetName())
				}
				return nil
			}

			dyn, err := DynamicFactory(g)
			if err != nil {
				return err
			}

			for _, d := range docs {
				if err := kube.Apply(cmd.Context(), dyn, d, "ap-apply"); err != nil {
					return fmt.Errorf("apply %s/%s: %w", d.GetKind(), d.GetName(), err)
				}
				fmt.Fprintf(out, "applied %s/%s\n", d.GetKind(), d.GetName())
			}
			return nil
		},
	}
	apcmd.FileFlag(cmd, &file, "Path to ClusterAgentSettings YAML")
	_ = cmd.MarkFlagRequired("file")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be applied without applying")
	return cmd
}
