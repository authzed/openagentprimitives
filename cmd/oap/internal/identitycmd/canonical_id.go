package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func newIdentityCanonicalIDCmd(_ *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "canonical-id <email>",
		Short: "Print the canonical user ID for an email (use when writing SpiceDB group memberships by hand)",
		Long: `Print the canonical user ID derived from an email address.

Use this when constructing SpiceDB group-membership writes for the
multiplayer-sessions feature. Example:

    zed relationship create group:engineering member user:$(oap identity canonical-id alice@example.com)

The canonical form (base64-encoded lowercased email) is the same
encoding channelsd uses internally, so admin-written group memberships
match what's looked up at message time.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := identity.EmailReference(identity.Email(args[0])).Canonical()
			if err != nil {
				return fmt.Errorf("canonicalize %q: %w", args[0], err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
}
