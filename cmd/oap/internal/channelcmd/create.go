// The CLI dispatcher for `oap channel create`. A kind's wizard says what it
// needs as oap.Question values (channelkinds.Wizard); everything around
// that belongs here:
//
//	pick the kind → detect terminal capabilities → theme → seed the flags
//	→ hand the run to cmd/oap/internal/channelwizard and read the result
//	→ hand the result back to channelwizard to apply and to summarize
//	→ watch the Channel and print the verdict
//
// The middle steps — asking the questions, driving the browser handoff,
// resolving the answers into manifests, landing them, and rendering what the
// run recorded — are channelwizard's, because `oap agent install` drives the
// same sequence for the channels a bundle declares and the two must not fork.
// What stays here is what is specific to THIS command: its flags, its chrome,
// its --apply=false manifest dump, its monitoring-channel lookups, and the
// connectivity verdict it ends on.
//
// The split is what lets a non-terminal client render the same wizard: every
// terminal decision above is made HERE, and the kind names no terminal type.
//
// Both flows — the agent channel and the monitoring channel `oap init` offers —
// go through the same path, so there is one place that decides how a wizard is
// presented, applied and summarized.
package channelcmd

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelwizard"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"   // register agent kind: registry.Get's --kind resolution and KindNames()'s menu (no interactive wizard)
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"   // register bento kind: `oap channel create --kind bento`
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser" // register browser kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github" // register github kind: `oap channel create --kind github`
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"  // register local kind
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// channelCreateOptions is everything `oap channel create` was asked for. Held
// as one struct so runChannelCreate has a single parameter to grow, and so the
// `oap init` monitoring offer can call the same function with the same shape.
type channelCreateOptions struct {
	// kind is the channel kind to build. Empty prompts for one, which a
	// non-interactive run refuses.
	kind string
	// apply writes the manifests to the cluster. False prints them instead.
	apply bool
	// monitoring builds a Role=monitoring Channel — one that posts framework
	// events and binds to no AgentClass — instead of an agent channel.
	monitoring bool

	// name is the Channel resource's name. It seeds the shared name question,
	// and on the monitoring path it also decides WHICH Channel is being
	// reconfigured (see resolveMonitoringExisting).
	name string

	// role is the ChannelSpec.Role the created Channel must have. Empty leaves
	// whatever the kind's own manifests set, which is what every run did
	// before this flag existed.
	//
	// It is NOT an --answer: no kind's wizard asks for a role, so there is no
	// question key to seed and nothing would read one. It travels past the
	// questions and is stamped by wizardrun.Finish — the same place `oap agent
	// install` stamps a bundle's declared role, so the command an install
	// prints for a channel it could not wire can actually reproduce what the
	// install would have created.
	//
	// "monitoring" is refused rather than accepted: --monitoring already asks
	// for that, and it does more than set a field (it selects the kind's own
	// monitoring flow, and resolves the existing Channel being reconfigured).
	// Two ways to say one thing is two things that can disagree.
	role string

	// nonInteractive fails the run at the first question with no seeded answer
	// rather than prompting for it.
	nonInteractive bool

	// answers pre-answer a kind's questions as key=value, one per flag
	// occurrence. The keys are the kind's own — the Names of the questions its
	// wizard declares — so a new question in a kind's flow becomes seedable
	// with no change here.
	answers []string

	// wait watches the applied Channel until it connects or reports what is
	// stopping it, instead of ending the run at "applied" and leaving the user
	// to go and look. Meaningless without apply, which has nothing to watch.
	//
	// The flag it is bound to defaults to TRUE; this field does not, so a
	// caller building the struct directly (the `oap init` monitoring offer, a
	// test) opts in explicitly rather than inheriting a default from a flag it
	// never registered.
	wait bool
	// timeout bounds that watch. Zero means channelWatchTimeout.
	timeout time.Duration
}

func newChannelCreateCmd(g *apcmd.Globals) *cobra.Command {
	opts := channelCreateOptions{}
	cmd := &cobra.Command{
		Use:   "create [--kind <kind>]",
		Short: "Interactive wizard: create a Channel via the kind's installation flow.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChannelCreate(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), g, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.kind, "kind", "", "Kind to create (e.g., slack, fake)")
	// The caveat is spelled out because --apply=false reads like a dry run and
	// is not one on every route: a kind whose flow creates something OUTSIDE
	// the cluster — the Slack wizard's "create the app for me" route mints a
	// real Slack app and installs it — has already done that by the time the
	// manifests are withheld.
	f.BoolVar(&opts.apply, "apply", true,
		"Apply the generated manifests after the wizard. --apply=false withholds only the Kubernetes objects; "+
			"anything the wizard creates outside the cluster (a Slack app, when you ask oap to create one) is still created")
	f.BoolVar(&opts.monitoring, "monitoring", false,
		"Create a monitoring channel — one that posts framework events and binds to no agent")
	f.StringVar(&opts.name, "name", "", "Name for the Channel resource (default: derived from your answers)")
	f.StringVar(&opts.role, "role", "",
		"Role for the Channel: "+strings.Join(spiceboxv1alpha1.AgentChannelRoles(), ", ")+
			" (default: whatever this kind's setup produces). Use --monitoring for a monitoring channel")
	f.BoolVar(&opts.nonInteractive, "non-interactive", false,
		"Never prompt: answer every question with --answer/--name, or fail")
	// StringArray, not StringSlice: StringSlice splits its own value on commas,
	// which would shred a multi-value answer like capabilities=a,b into two
	// flag occurrences and lose any answer that legitimately contains a comma.
	f.StringArrayVar(&opts.answers, "answer", nil,
		"Pre-answer a wizard question as key=value (repeatable); commas separate a multi-select's values. "+
			"Avoid passing credentials this way — flags land in shell history and in the process table")
	f.BoolVar(&opts.wait, "wait", true,
		"Watch the Channel after applying it and report whether it connected before returning")
	f.DurationVar(&opts.timeout, "timeout", channelWatchTimeout,
		"How long to watch the Channel for when --wait is true")
	return cmd
}

// checkRoleFlag refuses a --role this command cannot honour, in this command's
// own vocabulary and before anything else happens.
//
// THE VERDICT IS NOT THIS FUNCTION'S. wizardrun.CheckRole decides whether a
// role is stampable, and this asks it — because wizardrun.Finish asks the same
// function at the end of every run, and a command that refused a different set
// from the tail it feeds would either reject a role the tail would have taken
// or wave through one it will not. Two wordings of one rule is a shape this
// repo accepts; two RULES wearing one name is not.
//
// WHAT IS THIS COMMAND'S IS THE WORDING, and it is why the refusal is not just
// delegated whole: wizardrun cannot name `--monitoring` or `--role`, because it
// serves a browser form too. The switch below re-derives which case fired only
// to phrase it; anything the shared rule refuses that it has no phrasing for
// is returned VERBATIM rather than swallowed, so a rule added to CheckRole
// tomorrow is refused here on the day it lands rather than silently accepted.
//
// AND IT IS WHY THE CHECK IS EARLY. Finish's own call is the backstop every
// client gets, but it fires after the kind's flow has run — which for some
// kinds means after a real Slack or GitHub App exists that no API will list
// again. A flag combination that was never going to work must cost nothing.
func checkRoleFlag(opts channelCreateOptions) error {
	role := strings.TrimSpace(opts.role)
	shared := wizardrun.CheckRole(role, opts.monitoring)
	if shared == nil {
		return nil
	}
	legal := strings.Join(spiceboxv1alpha1.AgentChannelRoles(), ", ")
	switch {
	case opts.monitoring:
		return fmt.Errorf("--monitoring and --role %s cannot be combined: a monitoring channel posts framework events and binds to no agent, so it has no agent-facing role", role)
	case role == spiceboxv1alpha1.ChannelRoleMonitoring:
		return fmt.Errorf("--role monitoring is not how a monitoring channel is created; pass --monitoring instead, which also selects the kind's monitoring flow. --role takes one of: %s", legal)
	case !slices.Contains(spiceboxv1alpha1.AgentChannelRoles(), role):
		return fmt.Errorf("unknown --role %q; it must be one of: %s", role, legal)
	}
	return shared
}

// runChannelCreate is the whole presentation lifecycle for one wizard run.
func runChannelCreate(ctx context.Context, in io.Reader, out io.Writer, g *apcmd.Globals, opts channelCreateOptions) error {
	if err := checkRoleFlag(opts); err != nil {
		return err
	}
	kindName := opts.kind
	if kindName == "" {
		if opts.nonInteractive {
			return fmt.Errorf("--kind is required with --non-interactive (registered: %s)",
				strings.Join(KindNames(), ", "))
		}
		k, err := promptKind(in, out)
		if err != nil {
			return err
		}
		kindName = k
	}
	k, ok := registry.Get(kindName)
	if !ok {
		return fmt.Errorf("unknown kind %q (registered: %v)", kindName, KindNames())
	}

	// Flags are read into State before anything runs: a screen whose answer is
	// already there asks nothing, which is what makes both pre-filling and
	// --non-interactive work off one mechanism.
	st, seeded, err := seedState(opts)
	if err != nil {
		return err
	}

	// Build the k8s client only when we need it. With apply=false, no cluster
	// flags and no monitoring lookup to do, skip cluster connectivity so the
	// command can render manifests without an API server.
	var cli client.Client
	ns := g.Namespace
	if ns == "" {
		ns = "default"
	}
	var b *kube.Bundle
	if opts.apply || opts.monitoring || g.Kubeconfig != "" || g.Context != "" {
		b, err = g.Bundle()
		if err != nil {
			return err
		}
		cli = b.Controller
		ns = b.Namespace
	}

	// A name supplied up front is checked for collision HERE, before anything
	// is asked, because a wizard run is not all reversible: the Slack kind's
	// provisioning route creates and installs a real Slack app, and no Slack
	// API can list a user's apps afterwards. channelwizard.Apply makes the same
	// check — that one is the fail-closed choke point in front of the apply,
	// and it still guards the interactive path and the race — but by the time
	// it fires the app exists, while the run's error return means the summary
	// naming it is never reached. The seeded case is the one that can be
	// refused before anything irreversible happens, so it is.
	//
	// Not on the monitoring path: there a name that already exists is the
	// Channel being reconfigured, which resolveMonitoringExisting resolves
	// below and refuses on its own terms when it is something else.
	if !opts.monitoring {
		if err := wizardrun.RefuseExisting(ctx, cli, ns, st.Get(wizardkeys.KeyChannelName)); err != nil {
			return err
		}
	}

	// Seeded is channelwizard.Seeded.Values — the SAME map the driver consults
	// to decide which questions to drop — so a wizard that shapes its question
	// set around what --answer already supplied (the Slack kind's provisioning
	// route, for one) sees exactly what the run will see rather than a second
	// copy that could drift from it. channelwizard.Seeded's own doc is why the
	// two are one value.
	//
	// NonInteractive is carried for the same reason: a kind that must refuse a
	// step no flag can stand in for — creating a Slack app in a browser, a
	// token source that waits on a challenge code — has no other way to know
	// nobody is watching, and this contract gives it no per-question hook to
	// find out later.
	wizIn := channelkinds.WizardInput{
		K8s:            cli,
		Namespace:      ns,
		Seeded:         seeded.Values,
		NonInteractive: opts.nonInteractive,
		// "." and not os.Getwd(): a kind that writes beside the run wants the
		// operator's current directory, whatever it is, and asking for it here
		// would turn a lookup failure into a refusal to run the wizard at all.
		// Empty would mean "this client has no filesystem", which is false for a
		// CLI — see WizardInput.WorkingDir for why that is the default.
		WorkingDir: ".",
		// This command IS the operator's shell: the process they typed the
		// command into, on their machine, with their PATH and their terminal.
		// It is the one client that can say so, and false is what every other
		// client gets by leaving the field alone — see
		// WizardInput.OperatorShell.
		OperatorShell: true,
	}
	if opts.monitoring {
		// Asked of the kind, not of a list of names here. A kind that cannot
		// deliver a MonitoringEvent has no monitoring flow to run, and its
		// wizard would quietly ignore Monitoring and hand back an ordinary
		// agent Channel — a Channel the monitoring relay will never write to.
		if !k.SupportsMonitoring() {
			return fmt.Errorf("kind %q cannot deliver monitoring events, so it has no monitoring channel to create; pick a kind that can (%s)",
				kindName, strings.Join(monitoringCapableKindNames(), ", "))
		}
		wizIn.Monitoring = true
		if wizIn.Existing, err = resolveMonitoringExisting(ctx, cli, ns, kindName, st.Get(wizardkeys.KeyChannelName)); err != nil {
			return err
		}
		if wizIn.CredentialsExist, err = monitoringCredentialsExist(ctx, cli, ns, wizIn.Existing); err != nil {
			return err
		}
	}

	// Capabilities come from the writer this command was handed rather than
	// from os.Stdout: a test or a shell pipeline supplies a plain io.Writer,
	// and taking over a screen the command is not actually writing to would
	// leave the wizard invisible. In production the two are the same file —
	// cobra hands this command os.Stdout.
	theme := g.Theme(out)

	wizOut, answered, err := channelwizard.Run(ctx, k.Wizard(), wizIn, kindName, st, opts.role, seeded,
		CreateRunOptions(theme, in, out, kindName, opts.nonInteractive))
	if err != nil {
		return channelwizard.ReportUnfinished(out, theme, channelwizard.RunNotes(answered, wizOut), err)
	}

	if opts.apply {
		// b is guaranteed non-nil here (apply=true forces b construction above).
		if err := channelwizard.Apply(ctx, b, wizOut); err != nil {
			return channelwizard.ReportUnfinished(out, theme, channelwizard.RunNotes(answered, wizOut), err)
		}
	} else if err := printManifests(out, wizOut); err != nil {
		return channelwizard.ReportUnfinished(out, theme, channelwizard.RunNotes(answered, wizOut), err)
	}

	// The summary is the record that survives the alt-screen being released,
	// so it is written last and to the same stream the run used.
	if err := tui.RenderSummary(out, theme, channelwizard.RunNotes(answered, wizOut)); err != nil {
		return err
	}
	if opts.apply {
		fmt.Fprintf(out, "\nApplied to namespace %s.\n", ns)
	}
	channelwizard.RenderNextSteps(out, theme, wizOut.Notes)

	// Last, so the verdict is the final thing in the scrollback — it is what
	// the user is waiting for, and on a bad outcome its follow-up command is
	// what they will type next.
	reportChannelConnected(ctx, out, theme, cli, ns, wizOut, opts)
	return nil
}

// reportChannelConnected watches the Channel a run just applied and prints how
// it went. A run that applied nothing, was told not to wait, or produced no
// Channel manifest has nothing to watch and prints nothing.
func reportChannelConnected(
	ctx context.Context,
	out io.Writer,
	theme *tui.Theme,
	cli client.Client,
	namespace string,
	wizOut channelkinds.WizardOutput,
	opts channelCreateOptions,
) {
	if !opts.apply || !opts.wait || cli == nil || wizOut.ChannelManifest == nil {
		return
	}
	name := wizOut.ChannelManifest.Name
	timeout := opts.timeout
	if timeout <= 0 {
		timeout = channelWatchTimeout
	}
	fmt.Fprintf(out, "\n%s\n", theme.Render(theme.Subtle,
		fmt.Sprintf("Waiting for Channel %q to connect (up to %s)…", name, timeout.Round(time.Second))))

	v, err := watchChannel(ctx, cli, namespace, name, channelWatchInterval, timeout)
	renderChannelVerdict(out, theme, name, v, err, timeout)
}

// wizardTitle is the chrome's title bar for a run. One function rather than a
// literal at each call site, so the agent flow and the monitoring flow cannot
// title themselves differently.
func wizardTitle(kindName string) string { return "oap · channel create · " + kindName }

// seedState is this command's flags in the shape a run starts from.
//
// The seeding itself is channelwizard.Seed's, not this command's: the State
// and the key list it returns have to agree with each other (see
// channelwizard.Seeded), and they have to agree with what the driver later
// drops, checks and reads back. What belongs HERE is only the mapping from
// this command's own flags onto that call, which is why the body is one line
// and channelCreateOptions does not cross the boundary.
func seedState(opts channelCreateOptions) (*tui.State, channelwizard.Seeded, error) {
	return channelwizard.Seed(opts.name, opts.answers)
}

// CreateRunOptions is how this command's wizard is presented.
//
// Split out because Inline is not visible in the output of any driver a test
// can build, so this is the only place the decision below can be read back.
//
// NOT inline: every answer this wizard collects can be given from what the form
// itself shows. A kind's guidance — scope lists, the app manifest, the Home-tab
// walkthrough — is composed into the screens themselves, not printed to the
// stream, and the only stream writes are the kind picker before the wizard and
// the next-steps block after it. Neither informs an answer.
// (See tui.Options.Inline.)
func CreateRunOptions(
	theme *tui.Theme,
	in io.Reader,
	out io.Writer,
	kindName string,
	nonInteractive bool,
) tui.Options {
	return tui.Options{
		Theme:          theme,
		Title:          wizardTitle(kindName),
		In:             in,
		Out:            out,
		NonInteractive: nonInteractive,
	}
}

// printManifests writes what a run would have applied. Reached only on
// --apply=false, which is the request to see them.
func printManifests(w io.Writer, wizOut channelkinds.WizardOutput) error {
	if wizOut.SecretManifest == nil {
		fmt.Fprintln(w, "--- Secret --- (keeping existing credentials)")
	} else if err := printYAML(w, "Secret", wizOut.SecretManifest); err != nil {
		return err
	}
	if wizOut.ChannelManifest != nil {
		if err := printYAML(w, "Channel", wizOut.ChannelManifest); err != nil {
			return err
		}
	}
	if wizOut.CapabilityPatch != nil {
		if err := printYAML(w, "AgentClass capabilities", wizOut.CapabilityPatch); err != nil {
			return err
		}
	}
	return nil
}

func printYAML(w io.Writer, label string, obj any) error {
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("render the %s manifest: %w", label, err)
	}
	fmt.Fprintf(w, "--- %s ---\n", label)
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write the %s manifest: %w", label, err)
	}
	return nil
}

// resolveMonitoringExisting decides which monitoring Channel, if any, a run is
// reconfiguring.
//
// A requested name wins over the role scan, and that ordering is load-bearing.
// findExistingMonitoringChannel matches on role and kind REGARDLESS of name,
// while the monitoring flow refuses a seeded name that differs from the
// Channel it was handed. Scanning first would therefore hand `--monitoring
// --name new-mon` whichever monitoring Channel already existed and refuse the
// run — making a second monitoring Channel impossible to create without
// deleting the first.
//
// A name that resolves to something that is not a monitoring Channel of this
// kind is refused rather than adopted: silently reconfiguring an agent channel
// as a monitoring one would rewrite a Channel the user never named.
func resolveMonitoringExisting(ctx context.Context, c client.Client, namespace, kindName, wantName string) (*spiceboxv1alpha1.Channel, error) {
	if c == nil {
		return nil, nil
	}
	if wantName == "" {
		return findExistingMonitoringChannel(ctx, c, namespace, kindName)
	}
	var ch spiceboxv1alpha1.Channel
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: wantName}, &ch)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing by that name yet: this run creates it.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("look up Channel %q in namespace %q: %w", wantName, namespace, err)
	}
	if ch.Spec.Role != spiceboxv1alpha1.ChannelRoleMonitoring || ch.Spec.Kind != kindName {
		return nil, fmt.Errorf("channel %q already exists in namespace %q and is not a %s monitoring channel; choose a different name or delete it first",
			wantName, namespace, kindName)
	}
	return &ch, nil
}

// monitoringCredentialsExist reports whether the Channel being reconfigured
// still has its credentials Secret, which is what lets the wizard offer "leave
// the stored token alone" instead of demanding a fresh one.
func monitoringCredentialsExist(ctx context.Context, c client.Client, namespace string, existing *spiceboxv1alpha1.Channel) (bool, error) {
	if c == nil || existing == nil || existing.Spec.CredentialsRef.SecretName == "" {
		return false, nil
	}
	name := existing.Spec.CredentialsRef.SecretName
	var sec corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("probe existing creds Secret %q in namespace %q: %w", name, namespace, err)
	}
}

// findExistingMonitoringChannel returns the single monitoring-role Channel of
// the given kind in the namespace, or nil when zero or more than one match
// (the wizard does not guess which of several channels to update). A nil client
// reports "none".
func findExistingMonitoringChannel(ctx context.Context, c client.Client, namespace, kindName string) (*spiceboxv1alpha1.Channel, error) {
	if c == nil {
		return nil, nil
	}
	var list spiceboxv1alpha1.ChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list channels in %q: %w", namespace, err)
	}
	var matches []spiceboxv1alpha1.Channel
	for i := range list.Items {
		ch := list.Items[i]
		if ch.Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring && ch.Spec.Kind == kindName {
			matches = append(matches, ch)
		}
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	return nil, nil
}

// ListMonitoringChannels returns every monitoring-role Channel in the
// namespace, across all kinds, sorted by name for stable output. A nil client
// reports none. Unlike findExistingMonitoringChannel it does NOT filter by
// kind and does NOT collapse multiple matches to nil: the oap init offer needs
// to know whether *any* monitoring channel exists before a kind is chosen.
func ListMonitoringChannels(ctx context.Context, c client.Client, namespace string) ([]spiceboxv1alpha1.Channel, error) {
	if c == nil {
		return nil, nil
	}
	var list spiceboxv1alpha1.ChannelList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list channels in %q: %w", namespace, err)
	}
	var matches []spiceboxv1alpha1.Channel
	for i := range list.Items {
		if list.Items[i].Spec.Role == spiceboxv1alpha1.ChannelRoleMonitoring {
			matches = append(matches, list.Items[i])
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	return matches, nil
}

// RunMonitoringCreate runs the monitoring flow and applies what it
// produces. Called from the oap init monitoring offer, which names no Channel,
// so the existing one (if any) is found by the role scan.
func RunMonitoringCreate(ctx context.Context, in io.Reader, out io.Writer, g *apcmd.Globals, kindName string) error {
	return runChannelCreate(ctx, in, out, g, channelCreateOptions{
		kind:       kindName,
		apply:      true,
		monitoring: true,
		// Same default a user typing the command gets. A monitoring channel
		// that never attaches is exactly as worth reporting as an agent one,
		// and the offer is made mid-`oap init`, where the user is already
		// watching components come up.
		wait: true,
	})
}

func promptKind(in io.Reader, out io.Writer) (string, error) {
	names := KindNames()
	if len(names) == 0 {
		return "", fmt.Errorf("no kinds registered (build error)")
	}
	fmt.Fprintln(out, "Available kinds:")
	for i, n := range names {
		fmt.Fprintf(out, "  %d. %s\n", i+1, n)
	}
	fmt.Fprint(out, "Choice [1]: ")
	var s string
	_, _ = fmt.Fscanln(in, &s)
	if s == "" {
		return names[0], nil
	}
	for _, n := range names {
		if n == s {
			return n, nil
		}
	}
	return "", fmt.Errorf("unknown kind %q", s)
}

// KindNames is every registered channel kind, for this command's own menus and
// refusals. registry.Names is already sorted (registry.All orders by name), so
// this adds nothing but the name the CLI reads better under.
func KindNames() []string { return registry.Names() }

// IsDemoKind reports whether a kind exists to serve tests and local
// development rather than a real workspace. Those kinds are registered in `oap`
// unconditionally, so anything that offers a kind to a user has to leave them
// out — suggesting the test fixture as a way to receive production monitoring
// events would be advice nobody should take.
//
// Named here rather than repeated at each menu so the suggestion a refusal
// makes and the menu `oap init` prints cannot drift apart.
func IsDemoKind(name string) bool { return name == "fake" || name == "local" }

// monitoringCapableKindNames lists the kinds a user could actually pick to
// deliver monitoring events, derived from the registry rather than
// transcribed, so a refusal can never suggest a kind that would be refused in
// turn.
func monitoringCapableKindNames() []string {
	var out []string
	for _, k := range registry.All() {
		if k.SupportsMonitoring() && !IsDemoKind(k.Name()) {
			out = append(out, k.Name())
		}
	}
	sort.Strings(out)
	return out
}
