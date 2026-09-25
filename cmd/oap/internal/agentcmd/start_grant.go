// The standing override for the session-start gate. The gate parks a session
// an org non-member (a channel guest) starts unless they hold
// agentclass#start_session; `starter` is that permission's direct arm, so a
// grant here means "this person may start sessions of this agent without a
// per-session platform-admin approval". Mirrors installcmd's platform
// grant-admin family in shape and canonicalization.
package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/installcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func newAgentGrantStartCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "grant-start <agent> <email>",
		Short: "Let a non-member start sessions of this agent (writes agentclass#starter)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			canonical, err := installcmd.CanonicalForEmail(args[1])
			if err != nil {
				return fmt.Errorf("canonicalize %q: %w", args[1], err)
			}
			if err := cl.TouchAgentClassStarter(cmd.Context(), b.Namespace, args[0], canonical); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted start on agent %s/%s to %s (user:%s)\n", b.Namespace, args[0], args[1], canonical)
			return nil
		},
	}
}

func newAgentRevokeStartCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke-start <agent> <email>",
		Short: "Revoke a non-member's standing to start sessions of this agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			canonical, err := installcmd.CanonicalForEmail(args[1])
			if err != nil {
				return fmt.Errorf("canonicalize %q: %w", args[1], err)
			}
			if err := cl.DeleteAgentClassStarter(cmd.Context(), b.Namespace, args[0], canonical); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked start on agent %s/%s from %s\n", b.Namespace, args[0], args[1])
			return nil
		},
	}
}

func newAgentListStartersCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list-starters <agent>",
		Short: "List the standing starter grants on this agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, cleanup, err := apspicedb.NewAuthzClient(cmd.Context(), apspicedb.ClusterAuthzDialer(g))
			if err != nil {
				return err
			}
			defer cleanup()
			starters, err := cl.ListAgentClassStarters(cmd.Context(), b.Namespace, args[0])
			if err != nil {
				return err
			}
			if len(starters) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no starter grants (only platform admins and org members can start sessions)")
				return nil
			}
			for _, s := range starters {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", s, identity.DecodeForDisplay(s))
			}
			return nil
		},
	}
}
