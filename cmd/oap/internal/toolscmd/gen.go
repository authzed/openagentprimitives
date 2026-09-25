// Package toolscmd — `oap tools gen` opens the agent builder in the browser.
//
// The old local LLM gen-agent loop is retired: the agent builder (an OAP
// agent, the model itself) does that work now, in a browser workshop. This
// command creates a builder session under the user's own identity and opens
// its workshop page.
package toolscmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
)

// builderClassName is the AgentClass the builder bundle installs (plan 6a).
const builderClassName = "agent-builder"

// builderOpeningPrompt is turn 0 for a builder session opened from the CLI. A
// STATEMENT OF FACT (see pkg/web/webui/sessions/start.go's openedViaUIPrompt for
// the rationale) — but deliberately NOT that one: openedViaUIPrompt tells the
// agent to wait beside a data-first view, whereas the builder's workshop starts
// empty and the builder must DRIVE intake. This opening says the person came to
// build, which is what starts the assess phase.
const builderOpeningPrompt = "The person has opened the agent builder to create a new agent."

// I/O seams, as package vars so the command's logic is unit-testable without a
// real cluster, SpiceDB, or browser.
type kubeBundle = kube.Bundle

var (
	dialSpiceDB          = spicedb.DialViaPortForward
	createBrowserSession = browsersession.Create
	openBrowser          = browser.Open
)

func newToolsGenCmd(g *apcmd.Globals) *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "gen",
		Short: "Open the agent builder in your browser to build a new agent",
		Long: "Opens a session with the agent builder in your browser. The agent " +
			"builder walks you from an idea to a working, tested agent — it has " +
			"replaced the old local tool generator.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runToolsGen(cmd.Context(), g, cmd.OutOrStdout(), builderNamespace(g), noBrowser)
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print the workshop URL instead of opening a browser")
	return cmd
}

// builderNamespace resolves where to look for the builder AgentClass: the
// global -n when the user set it, otherwise the system namespace `oap
// install` puts it in. A second, command-local --namespace flag would shadow
// the root's persistent one (TestNoSubcommandShadowsAPersistentFlag in
// cmd/oap guards exactly this) — read the global instead, same as
// installcmd's exposeNamespace. The fallback is deliberately NOT the
// kubeconfig context's namespace: the builder is a platform workload
// installed into a fixed namespace, not a resource the user's context happens
// to be pointed at.
func builderNamespace(g *apcmd.Globals) string {
	if g.Namespace != "" {
		return g.Namespace
	}
	return apcmd.SystemNamespace
}

func runToolsGen(ctx context.Context, g *apcmd.Globals, out io.Writer, namespace string, noBrowser bool) error {
	principal, err := clilogin.EnsureIdentity(ctx, g)
	if err != nil {
		return fmt.Errorf("resolve identity: %w", err)
	}
	canonical, err := principal.Canonical()
	if err != nil {
		return fmt.Errorf("canonicalize identity: %w", err)
	}

	b, err := g.Bundle()
	if err != nil {
		return err
	}

	// Friendly precheck: is the builder installed + Valid? browsersession.Create
	// re-checks, but a plain-language answer here beats a raw NotFound.
	var ac spiceboxv1alpha1.AgentClass
	if err := b.Controller.Get(ctx, client.ObjectKey{Namespace: namespace, Name: builderClassName}, &ac); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the agent builder isn't installed on this cluster (no AgentClass %q in %q) — run `oap install`", builderClassName, namespace)
		}
		return fmt.Errorf("look up the agent builder: %w", err)
	}
	if !apcmd.AgentClassIsValid(&ac) {
		return fmt.Errorf("the agent builder isn't ready yet (AgentClass %q is not Valid=True) — try again shortly", builderClassName)
	}

	// SpiceDB is required to write the session's started_by relation and to
	// answer the class start gate. A successful dial yields a real (non-nil)
	// client; assign it into the Granter/StartChecker interfaces only here.
	sdb, closeSDB, err := dialSpiceDB(ctx, b)
	if err != nil {
		return fmt.Errorf("connect to authorization: %w", err)
	}
	defer closeSDB()

	created, err := createBrowserSession(ctx, genDeps{
		k8s:     b.Controller,
		granter: sdb,
		checker: sdb,
		logger:  logr.Discard(),
	}, browsersession.Params{
		Namespace:  namespace,
		AgentClass: builderClassName,
		Prompt:     builderOpeningPrompt,
		Subject:    canonical.Subject(),
	})
	if err != nil {
		if errors.Is(err, browsersession.ErrNotAnAllowedStarter) {
			return fmt.Errorf("you're not on the agent builder's allowed-starters list — ask a platform admin to add you")
		}
		return fmt.Errorf("start a builder session: %w", err)
	}

	ref := namespace + "/" + created.Session.Name
	url, err := channelkinds.ComposeAgentUIURL(readWebdBaseURL(ctx, b), ref)
	if err != nil {
		return fmt.Errorf("compose the workshop URL: %w", err)
	}
	if url == "" {
		fmt.Fprintf(out, "Started a builder session (%s), but this cluster's web UI URL isn't configured, so I can't open it.\n", ref)
		return nil
	}
	if noBrowser {
		fmt.Fprintln(out, url)
		return nil
	}
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(out, "Started a builder session. Open it in your browser:\n  %s\n", url)
		return nil
	}
	fmt.Fprintf(out, "Opened the agent builder in your browser:\n  %s\n", url)
	return nil
}

// genDeps implements browsersession.Deps.
type genDeps struct {
	k8s     client.Client
	granter authz.Granter
	checker browsersession.StartChecker
	logger  logr.Logger
}

func (d genDeps) K8s() client.Client                        { return d.k8s }
func (d genDeps) Authz() authz.Granter                      { return d.granter }
func (d genDeps) Logger() logr.Logger                       { return d.logger }
func (d genDeps) StartChecker() browsersession.StartChecker { return d.checker }

// readWebdBaseURL is a one-shot read of webd's external trusted base URL. Any
// failure (not installed, unreachable, unpopulated) returns "" — ComposeAgentUIURL
// treats "" as "not configured" and runToolsGen prints guidance rather than the URL.
func readWebdBaseURL(ctx context.Context, b *kubeBundle) string {
	var cm corev1.ConfigMap
	if err := b.Controller.Get(ctx, client.ObjectKey{
		Namespace: externalurl.Namespace,
		Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
	}, &cm); err != nil {
		return ""
	}
	return cm.Data[spiceboxv1alpha1.WebdTrustedURLKey]
}
