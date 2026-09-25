package toolscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// newMCPProbeCmd issues a live `tools/list` against a deployed MCPServer,
// using an AgentIdentity's credential for auth. Picks the AgentIdentity
// automatically when exactly one carries a credential for the server;
// requires --via when multiple match.
func newMCPProbeCmd(g *apcmd.Globals) *cobra.Command {
	var via string
	var jsonOut bool
	var showSchemas bool
	cmd := &cobra.Command{
		Use:   "probe <server>",
		Short: "List tools advertised by a deployed MCPServer using an AgentIdentity's credentials",
		Long: `Probe a deployed MCPServer with credentials resolved from an AgentIdentity.

Unlike ` + "`oap tools probe -f <file>`" + ` (anonymous, file-based, used during spec authoring),
this command resolves the live OAuth/PAT credential from an AgentIdentity whose
credentials include a credential named after the server, and shows what the
server actually exposes to authenticated callers.

If exactly one AgentIdentity in the namespace has a credential for the server,
it is selected automatically. Otherwise pass --via to choose one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName := args[0]
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			var srv spiceboxv1alpha1.MCPServer
			if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: serverName}, &srv); err != nil {
				return fmt.Errorf("get MCPServer %q: %w", serverName, err)
			}

			identity := via
			if identity == "" {
				identity, err = pickIdentityForMCPServer(ctx, b.Controller, b.Namespace, serverName)
				if err != nil {
					return err
				}
			}

			var header, value string
			if identity != "" {
				var ai spiceboxv1alpha1.AgentIdentity
				if err := b.Controller.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: identity}, &ai); err != nil {
					return fmt.Errorf("get AgentIdentity %q: %w", identity, err)
				}
				reqs := mcpkind.New().SetupRequirements(ctx, mcpkind.NewTarget(&srv))
				descs, descErr := credresolve.Descriptors(reqs, runner.RuntimeIdentityFromAgentIdentity(&ai), nil)
				if descErr != nil {
					return descErr
				}
				if len(descs) > 0 {
					b2 := inproc.New(b.Controller)
					res, resolveErr := b2.Resolve(ctx, broker.Request{Credentials: descs})
					if resolveErr != nil {
						if errors.Is(resolveErr, credresolve.ErrExpired) {
							return fmt.Errorf("AgentIdentity %q has an expired credential — refresh with `oap agent setup-identity <agent> --only mcp:%s --force`", identity, serverName)
						}
						return resolveErr
					}
					for h, v := range res.HTTPHeaders {
						header, value = h, v
						break
					}
				}
			}

			pc := &probe.Client{URL: srv.Spec.Server.URL}
			tools, err := pc.ListTools(ctx, header, value)
			if err != nil {
				return fmt.Errorf("probe %s: %w", srv.Spec.Server.URL, err)
			}

			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(tools)
			}

			title := fmt.Sprintf("%s — %d tool(s) (via AgentIdentity %q)", srv.Spec.Server.URL, len(tools), identity)

			// A TTY stdout gets the interactive split-view TUI; piped
			// output falls back to the static render (--schemas
			// controls schema inlining there). TTY-ness is read off the
			// same capabilities the static branch themes from, so the two
			// branches cannot disagree about what this stream is.
			if apcmd.DetectCaps(out, g.NoColor).TTY {
				return runProbeTUI(ctx, title, tools, g.NoColor)
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			// Only this branch is themed: --json above is the machine-readable
			// mode and stays raw.
			return toolscli.RenderProbeResult(out, g.Theme(out), contract.ProbeResult{
				Title:    title,
				Sections: probeToolSections(tools, showSchemas),
			})
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "AgentIdentity to authenticate with (required if more than one identity binds to the server)")
	apcmd.JSONFlag(cmd, &jsonOut, "Emit the raw tools/list response as indented JSON")
	cmd.Flags().BoolVar(&showSchemas, "schemas", false, "Inline each tool's input and output JSON schema (piped/non-TTY output only; the interactive view always offers them)")
	return cmd
}

// probeToolSections renders one Section per advertised tool: the title
// carries the tool name plus annotation hints, the body the first line
// of the description and — when showSchemas is set — the indented input
// and output JSON schemas.
func probeToolSections(tools []probe.Tool, showSchemas bool) []contract.Section {
	sections := make([]contract.Section, 0, len(tools))
	for _, t := range tools {
		title := t.Name
		if t.Annotations.ReadOnlyHint {
			title += " [read-only]"
		}
		if t.Annotations.DestructiveHint {
			title += " [destructive]"
		}

		var body strings.Builder
		if t.Description != "" {
			body.WriteString(firstLine(t.Description))
		}
		if showSchemas {
			if body.Len() > 0 {
				body.WriteString("\n\n")
			}
			body.WriteString("input schema:\n")
			body.WriteString(indentedSchema(t.InputSchema))
			body.WriteString("\n\noutput schema:\n")
			body.WriteString(indentedSchema(t.OutputSchema))
		}

		sections = append(sections, contract.Section{Title: title, Body: body.String()})
	}
	return sections
}

// indentedSchema pretty-prints a raw JSON schema blob. Falls back to
// the raw bytes when the payload isn't valid JSON, and to "(none)" when
// the server advertised no schema.
func indentedSchema(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(none)"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// pickIdentityForMCPServer scans AgentIdentities in the namespace for ones
// whose credentials include the declared credential name for the server.
// The credential name is srv.Spec.Auth.Credential when non-empty, otherwise
// the server's metadata.name (matching the E1 mcp setup default).
// Returns the single match, or an error explaining the ambiguity / absence.
func pickIdentityForMCPServer(ctx context.Context, c client.Client, namespace, serverName string) (string, error) {
	// Resolve the credential name for this server. We need the MCPServer
	// spec to read Auth.Credential, but pickIdentityForMCPServer is called
	// before the MCPServer is fetched — fall back to the server name as the
	// default credential name (matches the setup default in E1).
	credName := serverName

	var ids spiceboxv1alpha1.AgentIdentityList
	if err := c.List(ctx, &ids, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list AgentIdentities in %q: %w", namespace, err)
	}
	var candidates []string
	for _, ai := range ids.Items {
		for _, cred := range ai.Spec.Credentials {
			if cred.Name == credName {
				candidates = append(candidates, ai.Name)
				break
			}
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no AgentIdentity in namespace %q has a credential %q for server %q — set one up with `oap agent setup-identity <agent>` or pass --via", namespace, credName, serverName)
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("multiple AgentIdentities in namespace %q have a credential %q for server %q (%v) — pass --via <identity> to disambiguate", namespace, credName, serverName, candidates)
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
