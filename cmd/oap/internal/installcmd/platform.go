package installcmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// CanonicalForEmail derives the canonical SpiceDB user id for an email,
// matching the encoding channelsd applies at the pipeline boundary. Errors on
// an empty/invalid email (the synthetic guard) so a blank <email> arg fails
// loudly instead of writing a phantom admin grant.
func CanonicalForEmail(email string) (identity.CanonicalUserID, error) {
	c, err := identity.EmailReference(identity.Email(strings.ToLower(strings.TrimSpace(email)))).Canonical()
	if err != nil {
		return identity.CanonicalUserID{}, err
	}
	return c, nil
}

func NewPlatformCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Manage platform-level admin access (the admin UI gate)",
	}
	cmd.AddCommand(newPlatformGrantAdminCmd(g))
	cmd.AddCommand(newPlatformRevokeAdminCmd(g))
	cmd.AddCommand(newPlatformListAdminsCmd(g))
	return cmd
}

func newPlatformGrantAdminCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "grant-admin <email>",
		Short: "Grant platform admin (writes platform:platform#admin@user:<canonical(email)>)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			canonical, err := CanonicalForEmail(args[0])
			if err != nil {
				return fmt.Errorf("canonicalize %q: %w", args[0], err)
			}
			if err := cl.TouchPlatformAdmin(cmd.Context(), canonical); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted platform admin to %s (user:%s)\n", args[0], canonical)
			return nil
		},
	}
}

func newPlatformRevokeAdminCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke-admin <email>",
		Short: "Revoke platform admin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			canonical, err := CanonicalForEmail(args[0])
			if err != nil {
				return fmt.Errorf("canonicalize %q: %w", args[0], err)
			}
			if err := cl.DeletePlatformAdmin(cmd.Context(), canonical); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked platform admin from %s\n", args[0])
			return nil
		},
	}
}

func newPlatformListAdminsCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list-admins",
		Short: "List platform admins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			admins, err := cl.ListPlatformAdmins(cmd.Context())
			if err != nil {
				return err
			}
			if len(admins) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no platform admins")
				return nil
			}
			for _, a := range admins {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", a, identity.DecodeForDisplay(a))
			}
			return nil
		},
	}
}
