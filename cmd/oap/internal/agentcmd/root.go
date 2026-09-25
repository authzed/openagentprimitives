// Package agentcmd implements `oap agent`: the lifecycle of an AgentClass —
// package, sign, verify, push, pull, install, uninstall — plus the commands
// that run one: run, logs, sessions, inspect, and the credential setup its
// tools need.
package agentcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/chatcmd"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage and run AgentClass resources",
	}
	cmd.AddCommand(newAgentListCmd(g))
	cmd.AddCommand(newAgentShowCmd(g))
	cmd.AddCommand(newAgentAuthzCmd(g))
	cmd.AddCommand(newAgentApplyCmd(g))
	cmd.AddCommand(newAgentDeleteCmd(g))
	cmd.AddCommand(newAgentSessionsCmd(g))
	cmd.AddCommand(newAgentLogsCmd(g))
	cmd.AddCommand(newAgentRunCmd(g))
	cmd.AddCommand(chatcmd.NewCmd(g))
	cmd.AddCommand(newAgentPutKeyCmd(g))
	cmd.AddCommand(newAgentSetupIdentityCmd(g))
	cmd.AddCommand(newAgentInspectCmd(g))
	cmd.AddCommand(newAgentLintCmd(g))
	cmd.AddCommand(newAgentPackageCmd(g))
	cmd.AddCommand(newAgentPushCmd(g))
	cmd.AddCommand(newAgentPullCmd(g))
	cmd.AddCommand(newAgentInstallCmd(g))
	cmd.AddCommand(newAgentUninstallCmd(g))
	cmd.AddCommand(newAgentSignCmd(g))
	cmd.AddCommand(newAgentVerifyCmd(g))
	cmd.AddCommand(newAgentGrantStartCmd(g))
	cmd.AddCommand(newAgentRevokeStartCmd(g))
	cmd.AddCommand(newAgentListStartersCmd(g))
	return cmd
}
