package directorycmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"

	// Each blank import below registers that kind's relsync.Kind via its own
	// relsync_kind.go init() — the explicit wiring site for THIS aspect
	// (relsync.Get / relsync.All, which `oap directory list` and `configure`
	// walk), mirroring internal/cmd/operator/main.go's own comment for the
	// same registration. All three are ALSO reachable transitively in this
	// binary already: github/slack via channelcmd, agentcmd and sessioncmd's
	// own channel-kind imports, and all three (github, onepassword, slack)
	// via cmd/oap/main.go's blank import of
	// pkg/authz/spicedb/relsource/imports, which links them for a completely
	// unrelated reason (completing the SpiceDB relsource claim table so
	// guarded writes like `oap memory share` work — see that package's own
	// doc: every relsync.Kind's Source() carries real Claims, so every
	// relsync kind ends up listed there too). None of that transitive
	// coverage makes this import block redundant to keep: it is what makes
	// `oap directory`'s dependency on these three kinds legible and
	// deliberate at ITS OWN definition site, rather than an accident of an
	// unrelated authorization-plumbing import happening to reach the same
	// packages — the same reasoning internal/cmd/operator/main.go's own
	// comment gives for keeping its (also technically redundant) github
	// import. See root_test.go's TestDirectoryRootGo_BlankImportsEveryRelsyncKind
	// for why this fact has to be checked structurally rather than
	// behaviorally: no assertion made against relsync.All() or the real `oap`
	// binary's registry can ever go red if one of these three lines is
	// deleted, because the transitive coverage above keeps it green anyway.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// ClientFactory resolves the clients and target namespace every `oap
// directory` subcommand needs, from Globals. A package-level var — mirroring
// settingscmd.DynamicFactory — so a test can stub it without a cluster; see
// root_test.go's runCmd/withDyn.
//
// It hands back BOTH clients because the two things this family does need
// different ones: reading and applying RelationshipSource/AgentIdentity goes
// through the dynamic client, while creating a credential goes through
// setup.Store, which takes a controller-runtime client. Resolving them
// together, from one Bundle, is what keeps a `configure` run from building a
// second set of clients against the same cluster.
var ClientFactory = defaultClientFactory

func defaultClientFactory(g *apcmd.Globals) (dynamic.Interface, client.Client, string, error) {
	b, err := g.Bundle()
	if err != nil {
		return nil, nil, "", err
	}
	return b.Dynamic, b.Controller, b.Namespace, nil
}

// NewCmd builds `oap directory`: list what is configured (and what could
// be), and configure a source for any registered relsync.Kind.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "directory",
		Short: "Configure directory-sync sources (RelationshipSource): Slack, GitHub, 1Password, ...",
	}
	cmd.AddCommand(newListCmd(g), newConfigureCmd(g))
	return cmd
}

func newListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List directory-sync sources configured in this namespace, and every kind you could configure.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dyn, _, ns, err := ClientFactory(g)
			if err != nil {
				return fmt.Errorf("resolve cluster client: %w", err)
			}
			return runList(cmd.Context(), cmd.OutOrStdout(), dyn, ns)
		},
	}
}

// runList is the read-only answer to "what is syncing here, and what could
// be": every registered relsync.Kind that has a RelationshipSource in ns
// already, and — always, whether or not anything is configured yet — the
// full set of kinds this binary has registered, so an operator learns what
// they could configure next.
func runList(ctx context.Context, out io.Writer, dyn dynamic.Interface, ns string) error {
	kinds := relsync.All()

	var configuredLines []string
	for _, k := range kinds {
		cfg, err := ExistingFor(ctx, dyn, ns, k.Name())
		if err != nil {
			var ambiguous *AmbiguousSourceError
			if errors.As(err, &ambiguous) {
				// Ambiguity is a legitimate shape (see ExistingFor's own doc) that
				// `configure` genuinely cannot resolve on its own — but `list` is
				// read-only, so report it on this kind's line and keep listing
				// every other kind rather than aborting the whole command.
				configuredLines = append(configuredLines, fmt.Sprintf(
					"  - %s: ambiguous (%d sources: %s) — run 'oap directory configure' or resolve manually",
					k.Name(), len(ambiguous.Names), strings.Join(ambiguous.Names, ", ")))
				continue
			}
			// Anything else is a genuine failure (e.g. the API call itself
			// erroring) — abort rather than silently omit a kind, which would
			// look identical to "nothing configured" from an unreachable cluster.
			return fmt.Errorf("list directory sources: %w", err)
		}
		if cfg.Name == "" {
			continue
		}
		line := fmt.Sprintf("  - %s (kind: %s", cfg.Name, k.Name())
		if cfg.Identity != "" || cfg.Credential != "" {
			line += fmt.Sprintf(", credential: %s/%s", cfg.Identity, cfg.Credential)
		}
		if cfg.Endpoint != "" {
			line += fmt.Sprintf(", endpoint: %s", cfg.Endpoint)
		}
		line += ")"
		configuredLines = append(configuredLines, line)
	}

	if len(configuredLines) == 0 {
		fmt.Fprintln(out, "This namespace has no directory sources configured yet.")
	} else {
		fmt.Fprintln(out, "Configured directory sources:")
		for _, l := range configuredLines {
			fmt.Fprintln(out, l)
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Registered directory-sync kinds (configure any with `oap directory configure`):")
	for _, k := range kinds {
		fmt.Fprintf(out, "  - %s\n", k.Name())
	}
	return nil
}

func newConfigureCmd(g *apcmd.Globals) *cobra.Command {
	var nonInteractive bool
	var answerFlags []string

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Interactively configure a directory-sync source (Slack, GitHub, 1Password, ...).",
		Long: `configure walks you through picking a directory-sync kind, a credential to
sync with, its endpoint, and that kind's own questions, then server-side-applies
the resulting RelationshipSource under a fixed field manager.

Re-running against an already-configured kind edits the same source in
place — entering through a screen keeps its prior answer.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dyn, ctrl, ns, err := ClientFactory(g)
			if err != nil {
				return fmt.Errorf("resolve cluster client: %w", err)
			}

			answers, err := parseAnswers(answerFlags)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			pres := NewPresentation(g.Theme(out), cmd.InOrStdin(), out)
			sel, err := RunWizard(cmd.Context(), NewDeps(dyn, ctrl, ns), WizardOpts{
				Pres:           pres,
				Answers:        answers,
				NonInteractive: nonInteractive,
			})
			if err != nil {
				return err
			}
			if sel == nil {
				fmt.Fprintln(out, "No kind selected; nothing configured.")
				return nil
			}

			if err := Apply(cmd.Context(), dyn, ns, *sel); err != nil {
				return err
			}
			fmt.Fprintf(out, "Configured directory source %q (kind %s) in namespace %q.\n", sel.Name, sel.Kind, ns)
			return nil
		},
	}
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Fail rather than prompt for any answer not already supplied via --answer.")
	cmd.Flags().StringArrayVar(&answerFlags, "answer", nil, "Pre-seed one answer as key=value (e.g. --answer kind=github). Repeatable.")
	return cmd
}

// parseAnswers turns --answer key=value flags into the map RunWizard's
// WizardOpts.Answers pre-seeds tui.State from.
func parseAnswers(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	answers := make(map[string]string, len(flags))
	for _, kv := range flags {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--answer %q must be key=value", kv)
		}
		answers[key] = val
	}
	return answers, nil
}
