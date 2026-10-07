// Package setupmcpcmd implements `oap setup-mcp`: emitting a config snippet
// that points a local MCP-capable tool (Claude Code, Cursor, or a generic
// client) at this cluster's MCP server.
package setupmcpcmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// toolTarget bundles one --tool value's pure emitter with the human-readable
// description of where the emitted bearer token (if any) ends up living --
// used both to pick the emitter and to word the --mint warning.
type toolTarget struct {
	emit         func(serverURL, bearer string) string
	credLocation string
	nextStep     string
}

var toolTargets = map[string]toolTarget{
	"claude-code": {
		emit:         emitClaudeCode,
		credLocation: "claude-code's own MCP config (~/.claude.json, under mcpServers.oap.headers.Authorization)",
		nextStep:     "run the `claude mcp add` command above, or merge the JSON snippet into Claude Code's MCP config, then restart Claude Code",
	},
	"cursor": {
		emit:         emitCursor,
		credLocation: ".cursor/mcp.json, under mcpServers.oap.headers.Authorization",
		nextStep:     "merge the JSON snippet above into .cursor/mcp.json, then reload Cursor's MCP servers",
	},
	"generic": {
		emit:         emitGeneric,
		credLocation: "whatever config file you save this JSON into, under headers.Authorization",
		nextStep:     "add the JSON snippet above to your MCP client's server configuration",
	},
}

const defaultTool = "claude-code"

// NewCmd is the `oap setup-mcp` command.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	var serverFlag string
	var toolFlag string
	var mint bool

	cmd := &cobra.Command{
		Use:   "setup-mcp",
		Short: "Emit MCP client config pointing a local tool at this cluster's MCP server",
		Long: `Resolve this cluster's MCP server URL -- from --server, or by discovering the
webd external-URL ConfigMap via the current kube context -- and print a config
snippet for a local MCP-capable tool (claude-code, cursor, or generic).

By default (no --mint), no credential is embedded: OAuth-native MCP clients
run their own discovery/consent flow against the server directly, and consent
there defaults to read-only. Pass --mint to run that flow here instead --
dogfooding this cluster's own OAuth authorization server -- and embed the
resulting bearer token in the emitted config, for tools that do not speak MCP
OAuth themselves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, ok := toolTargets[toolFlag]
			if !ok {
				return fmt.Errorf("unknown --tool %q; valid values: claude-code, cursor, generic", toolFlag)
			}

			serverURL, err := resolveServerURL(cmd.Context(), g, serverFlag)
			if err != nil {
				return err
			}

			var bearer string
			if mint {
				tok, err := oauth.Login(cmd.Context(), mcpURL(serverURL), oauth.LoginOpts{
					Out: cmd.ErrOrStderr(),
				})
				if err != nil {
					return fmt.Errorf("mint a token via OAuth: %w", err)
				}
				bearer = tok.AccessToken
			}

			fmt.Fprintln(cmd.OutOrStdout(), target.emit(serverURL, bearer))

			if mint {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: the bearer token above is embedded in %s -- oap does not store it anywhere; it lives only in that file from here on.\n", target.credLocation)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "next step: %s\n", target.nextStep)

			return nil
		},
	}

	cmd.Flags().StringVar(&serverFlag, "server", "", "MCP server base URL (default: discover from the cluster's webd external-URL ConfigMap)")
	cmd.Flags().StringVar(&toolFlag, "tool", defaultTool, "Target tool: claude-code, cursor, or generic")
	cmd.Flags().BoolVar(&mint, "mint", false, "Run the OAuth flow now and embed the resulting bearer token in the emitted config")

	return cmd
}

// resolveServerURL implements the task's URL-resolution contract: --server
// wins outright; otherwise fall back to kube discovery of the webd
// external-URL ConfigMap; if neither works, fail with a message that tells
// the user to pass --server rather than leaving them to guess.
func resolveServerURL(ctx context.Context, g *apcmd.Globals, flag string) (string, error) {
	if flag != "" {
		return strings.TrimRight(flag, "/"), nil
	}

	b, err := g.Bundle()
	if err != nil {
		return "", fmt.Errorf("no --server given and could not reach the cluster to discover one (%w); pass --server explicitly", err)
	}
	url, err := clilogin.ResolveIdentitydBaseURL(ctx, b.Controller)
	if err != nil {
		return "", fmt.Errorf("no --server given and could not discover this cluster's MCP server URL (%w); pass --server explicitly", err)
	}
	return url, nil
}
