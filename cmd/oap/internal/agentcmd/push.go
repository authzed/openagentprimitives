package agentcmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// newAgentPushCmd pushes a packed .oap agent container to an OCI registry.
// Unlike `agent package`/`agent inspect`, push takes an already-packed file
// (or "-" for stdin) rather than a folder/cluster source — packing and
// pushing are kept as separate verbs so a CI pipeline can `agent package`
// once and `agent push` the resulting artifact to more than one registry
// without repacking.
func newAgentPushCmd(_ *apcmd.Globals) *cobra.Command {
	var plainHTTP bool

	cmd := &cobra.Command{
		Use:   "push <ref> <path>",
		Short: "Push a packed .oap agent container to an OCI registry",
		Long: "Push a packed .oap agent container to an OCI registry.\n\n" +
			"<path> is the path to a .oap produced by `oap agent package` ('-' reads it from stdin).\n" +
			"<ref> is an OCI reference (\"host[:port]/name[:tag]\").",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, path := args[0], args[1]

			var packed []byte
			var err error
			if path == "-" {
				packed, err = io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read .oap from stdin: %w", err)
				}
			} else {
				packed, err = os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
			}

			dig, err := oci.Push(cmd.Context(), ref, packed, oci.Options{PlainHTTP: plainHTTP})
			if err != nil {
				return fmt.Errorf("push %s: %w", ref, err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), dig)
			return nil
		},
	}

	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "Use plain HTTP (no TLS) to reach the registry — for loopback/dev registries")
	return cmd
}
