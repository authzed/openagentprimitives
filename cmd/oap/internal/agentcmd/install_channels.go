// The half of `oap agent install` that acts on the channels a bundle declares
// (oap.Requires.Channels). A direct install drives the flow after its CRs land;
// a graph install collects ordinary answers during read-only planning, then
// resolves provider handoffs and their result-dependent fallbacks graph-wide
// before it applies any bundle-owned resource.
//
// It decides nothing about WHICH channels there are or what is already known —
// pkg/platform/oap/channelplan does that, once, for this command and for
// admind's install endpoint — and it drives no wizard of its own:
// cmd/oap/internal/channelwizard owns that sequence, because `oap channel
// create` walks the identical one and a second copy would drift on the steps
// whose order cost a bug to establish. What is HERE is the install-specific
// part: the order the declarations are processed in, what each verdict means
// for the exit code, and the summary an operator is left with.
//
// THE PASS NEVER STOPS ON A FAILURE. A wizard that fails leaves work behind
// that nothing can take back — by the time a github handoff fails, the App
// exists on the org, and deleting the Channel would not unmake it — so there
// is nothing to roll back to, and stopping would additionally throw away the
// channels that would have wired after it. Each declaration is recorded and
// the next is attempted; the exit code is non-zero if any was left unwired by
// something this run attempted and could not finish.
//
// A RUN WITH NOBODY AT THE TERMINAL IS NOT THAT. It cannot ask a kind's
// questions at all, so it asks none, records every channel it would have set
// up as SKIPPED, and exits 0 — see channelSkipped. "Cannot prompt" covers
// exactly the channels a prompt would have wired; it is not a blanket over
// verdicts that have nothing to do with prompting, and wireOne's guard order
// is what keeps it from becoming one.
package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelwizard"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// channelWiring is everything the wiring pass brings to a declared channel:
// the cluster to create it in, and the presentation its questions are asked
// over.
//
// driver and theme travel together and come from ONE call to
// installQuestionPresentation — the same driver every other question this
// command asks is presented over, because they all read one stdin and the
// line-oriented driver buffers what it reads. A driver of this pass's own
// would let the manifest questions' buffer swallow the line a channel question
// is waiting for. See installQuestionPresentation for the whole rule.
type channelWiring struct {
	// kube is the cluster the bundle was just installed into. Its Controller
	// answers the wizards' lookups; its Dynamic lands their manifests.
	kube *kube.Bundle

	in  io.Reader
	out io.Writer

	// theme styles the run and is required by tui.RunWith. It is nil exactly
	// when driver is.
	theme *tui.Theme

	// driver is nil when nobody is at stdin — a scripted `--set`/`--values`
	// install, or the UI's ref-based one. This pass runs no channel flow
	// there: it records each declared channel as SKIPPED, in this command's
	// own words, and never asks channelwizard to run a flow it cannot answer.
	// See wireOne for why that second half is load-bearing rather than tidy.
	driver tui.Driver

	// chrome is the ONE frame every channel's questions are drawn in: this
	// pass's own title, and a rail whose steps are the declared channels. It
	// is built once, from the plan, before the first question — see
	// channelsChrome — and handed to each channel's run with that channel's
	// step marked active.
	//
	// Nil exactly when driver is, and for the same reason: a run nobody is at
	// draws nothing, so a frame it could never render is a frame it must not
	// acquire. wireChannels is where the two are kept in step.
	chrome *tui.Chrome

	// strat is the cluster kind this install landed in, read back from the
	// AP_CLUSTER_KIND `oap install` stamped (cloud.Stamped). It decides ONE
	// thing here: whether a declared webhook channel opens a tunnel before the
	// pass — see ensureWebhookAddress.
	//
	// NIL IS A REAL STATE, not a wiring bug, which is why check() does not
	// refuse it. `oap agent install` runs routinely under a namespace-scoped
	// kubeconfig that cannot read a Deployment in agentprimitives-system, and
	// a channels pass that refused to run there would fail every such install
	// over a question only the desktop's tunnel needs answered. The read's
	// failure is surfaced where it happens (wireDeclaredChannels warns), so
	// the nil that arrives here has already been spoken about.
	strat cloud.Strategy

	// endpointReadyTimeout bounds the wait for a just-created PublicEndpoint
	// to publish a URL. Zero means publicEndpointReadyTimeout — the production
	// value — so a caller that does not set it gets the sane bound rather than
	// a zero that would never wait at all; tests set a short one.
	endpointReadyTimeout time.Duration

	// prepared is populated only by graph planning. Each entry has been fully
	// resolved and sealed before graph-owned resources begin applying; drive
	// may only retrieve and apply its manifests.
	prepared map[string]*preparedChannelRun
}

type preparedChannelRun struct {
	run                 *channelwizard.PreparedRun
	channelName         string
	credentialName      string
	agentClass          string
	tracked             bool
	rootName            string
	path                oap.DependencyPath
	receipt             *wizardrun.ApplyReceipt
	credentialPreflight *install.ChannelCredentialPreflight
}

type preparedChannelPass struct {
	plans                []channelplan.ChannelPlan
	sensitiveValues      func() []string
	resolve              func(context.Context) error
	rollbackResolve      func(context.Context) error
	apply                func(context.Context) error
	rollbackApplied      func(context.Context) error
	finalize             func()
	credentialPreflights []install.ChannelCredentialPreflight
}

// installChannelWiring keeps every install-time channel flow on the same
// presentation as authored questions, capacity consent, image decisions, and
// adoption. Graph callers invoke it once per node but pass the one driver
// created by the command; constructing a fresh driver here would strand input
// already buffered by an earlier node.
func installChannelWiring(kb *kube.Bundle, in io.Reader, out io.Writer, driver tui.Driver, theme *tui.Theme) channelWiring {
	return channelWiring{kube: kb, in: in, out: out, driver: driver, theme: theme}
}

// channelStatus is what became of one declared channel on this run.
type channelStatus int

const (
	// channelWired: this run drove the kind's flow and its manifests landed.
	channelWired channelStatus = iota
	// channelAlreadyWired: a Channel of this name was there before this run,
	// and it is ours. Nothing was created, which is what makes re-running
	// install safe.
	channelAlreadyWired
	// channelSkipped: this run could not ask this channel's setup questions —
	// nobody was at the terminal — so it did not create it, and that is not a
	// failure. A scripted install that applies the bundle, names each channel
	// it could not wire, and prints the command that wires it has done its
	// job: CI stays green and the gap is named rather than discovered later
	// (spec §2.2). EVERY ONE OF THESE EXITS 0, which is the whole difference
	// between it and channelUnwired.
	channelSkipped
	// channelUnwired: this run did not wire it, for a reason the outcome
	// carries. Every one of these is a non-zero exit.
	channelUnwired
)

// channelOutcome is one declared channel and what happened to it.
type channelOutcome struct {
	// plan is the declaration and everything the planner worked out about it,
	// carried whole so the summary can report the pre-seeded answers and their
	// sources without a second lookup.
	plan channelplan.ChannelPlan

	status channelStatus

	// reason is why this channel is not wired, in the words of whatever
	// refused — a kind's own sentence, a conflict the planner found, or this
	// pass's. Always nil unless status is channelUnwired or channelSkipped:
	// the two differ in the exit code, not in whether the operator is owed an
	// explanation.
	reason error
	// remedy is what the operator can do about it, and it is NOT always "run
	// oap channel create": a name that is already taken by someone else's
	// Channel has to be resolved before that command could succeed, and a kind
	// this build does not link cannot be created by any command it offers.
	remedy string

	// warnings are things that went wrong AFTER the Channel was already on the
	// cluster, or that are worth saying about one that is. They do not make a
	// wired channel unwired — the object exists either way — so they are
	// reported rather than promoted to a failure.
	warnings []string
}

// name is the Channel name the bundle declared, which is the name every other
// bundled CR was written against.
func (o channelOutcome) name() string { return strings.TrimSpace(o.plan.Required.Name) }

// wireReport is what the pass did, in declaration order.
//
// The three name lists are DERIVED rather than stored beside the outcomes:
// one list of outcomes is the single record of what happened, and a stored
// second copy is a thing that can disagree with it after an edit nobody
// notices.
type wireReport struct {
	// namespace is where the channels were to be created, named in the
	// finishing commands so a copy-pasted one lands where this install did.
	namespace string
	outcomes  []channelOutcome
}

// Wired names the channels this run wired.
func (r wireReport) Wired() []string { return r.namesWith(channelWired) }

// AlreadyWired names the channels that were already there and already ours.
func (r wireReport) AlreadyWired() []string { return r.namesWith(channelAlreadyWired) }

// Skipped names the channels this run never attempted because it could not
// prompt. They are not wired and not failures; see channelSkipped.
func (r wireReport) Skipped() []string { return r.namesWith(channelSkipped) }

// Failed names the channels this run left unwired for a reason that is a
// failure — which is every unwired one EXCEPT a skip. Keeping the two apart
// here is what makes them differ in the exit code, since unwiredError reads
// only this list.
func (r wireReport) Failed() []string { return r.namesWith(channelUnwired) }

func (r wireReport) namesWith(s channelStatus) []string {
	var out []string
	for _, o := range r.outcomes {
		if o.status == s {
			out = append(out, o.name())
		}
	}
	return out
}

// unwiredError is the command's exit verdict: non-nil exactly when a declared
// channel was left unwired by something this run tried and could not finish.
//
// A SKIPPED CHANNEL IS DELIBERATELY NOT COUNTED. A run nobody was at could
// never have wired it, so failing the install would fail every scripted
// install of every channel-declaring bundle — a red CI job whose only remedy
// is to stop declaring channels. The channel is still reported, still with
// the command that wires it; only the exit code differs.
//
// It names the channels and points at the summary rather than restating each
// reason, because the summary is where the reasons are — with, for each one,
// the thing the operator would type next.
func (r wireReport) unwiredError() error {
	failed := r.Failed()
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%d of %d declared channel(s) are not wired (%s); the agent is installed but cannot use them — see the channel summary above for why, and for how to finish each",
		len(failed), len(r.outcomes), strings.Join(failed, ", "))
}

// String is the summary the operator is left with: what was wired, what was
// not, and how to finish each — plus, for every channel this run actually
// drove, the answers it supplied on the operator's behalf and where each came
// from.
//
// Deliberately unthemed. It is the block that has to survive being piped into
// a file or a ticket, and every claim in it is a fact rather than a
// decoration.
func (r wireReport) String() string {
	if len(r.outcomes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nDeclared channels (namespace %q):\n", r.namespace)
	for _, o := range r.outcomes {
		fmt.Fprintf(&b, "\n  %s (%s) — %s\n", o.name(), o.plan.Required.Kind, statusPhrase(o.status))
		if o.reason != nil {
			writeIndented(&b, "      ", o.reason.Error())
		}
		for _, warn := range o.warnings {
			writeIndented(&b, "      note: ", warn)
		}
		writeSeeded(&b, o)
		if o.remedy != "" {
			writeIndented(&b, "      ", o.remedy)
		}
	}
	// Every status gets a number, so the four add up to the declared count.
	// A tally that silently omitted one would make a skipped channel vanish
	// from the only line an operator skims.
	fmt.Fprintf(&b, "\n%d declared, %d wired, %d already wired, %d skipped, %d not wired.\n",
		len(r.outcomes), len(r.Wired()), len(r.AlreadyWired()), len(r.Skipped()), len(r.Failed()))
	return b.String()
}

func statusPhrase(s channelStatus) string {
	switch s {
	case channelWired:
		return "wired"
	case channelAlreadyWired:
		return "already wired; nothing to do"
	case channelSkipped:
		return "skipped; not wired, and not a failure"
	default:
		return "NOT wired"
	}
}

// writeSeeded reports what this install answered on the operator's behalf, and
// what it tried to answer and could not.
//
// PRE-SEEDING MUST NOT BE SILENT (the spec is explicit): a value taken from a
// ConfigMap the operator has never seen, applied without a word, is impossible
// to debug when it is wrong. So every seeded answer is printed with its
// source.
//
// The two maps are rendered under SEPARATE headings and are never joined,
// which is the whole reason ChannelPlan splits them. SeededFrom holds
// provenance for keys that ARE in Seeded; NotSeeded holds the reason a key is
// absent. Iterating SeededFrom without first testing Seeded — or printing a
// NotSeeded entry under the "from" heading — would render "external base URL
// came from: could not read ConfigMap … forbidden", labelling a failure as a
// provenance note.
//
// ONLY FOR A CHANNEL THIS RUN ACTUALLY WIRED, which is a narrower test than
// "not already wired" and the difference is the whole point. A conflict, an
// unknown wiring state and an unregistered kind are channelUnwired, a run
// nobody was there to answer is channelSkipped, and every one of them is
// decided BEFORE the kind's flow starts — so nothing asked anything, and
// printing "answered for you, so you were not asked" under them claims a run
// that never happened. The equality test against channelWired is what keeps
// that true for a status added later, too.
//
// It is the same class of mistake as rendering a NotSeeded reason as
// provenance, one direction over: saying something true about the plan in a
// place where the operator reads it as something about the run.
//
// A channel whose flow started and then FAILED is also excluded, and that
// costs nothing: the answers it did use are the ones the kind's own error is
// about, and channelwizard has already rendered what that run recorded to the
// same stream.
func writeSeeded(b *strings.Builder, o channelOutcome) {
	if o.status != channelWired {
		return
	}
	if len(o.plan.Seeded) > 0 {
		fmt.Fprintf(b, "      answered for you, so you were not asked:\n")
		for _, k := range slices.Sorted(maps.Keys(o.plan.Seeded)) {
			fmt.Fprintf(b, "        %s = %q\n", k, o.plan.Seeded[k])
			if from := o.plan.SeededFrom[k]; from != "" {
				fmt.Fprintf(b, "            source: %s\n", from)
			}
		}
	}
	if len(o.plan.NotSeeded) > 0 {
		fmt.Fprintf(b, "      could not be answered for you, so it was asked:\n")
		for _, k := range slices.Sorted(maps.Keys(o.plan.NotSeeded)) {
			fmt.Fprintf(b, "        %s — %s\n", k, o.plan.NotSeeded[k])
		}
	}
}

// writeIndented prefixes every line of s, so a kind's multi-line refusal keeps
// its shape under this summary's indentation instead of running back to
// column zero after the first newline.
func writeIndented(b *strings.Builder, prefix, s string) {
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(b, "%s%s\n", prefix, line)
	}
}

// wireDeclaredChannels is the whole channel half of one install: work out what
// the bundle's declarations still need, drive them, and print the summary.
//
// It runs AFTER install.Install, because a channel's wizard binds the Channel
// to an AgentClass and verifies that the class exists in the namespace — which
// it does not until the bundle's CRs have landed. What must NOT wait until
// here is judging the declaration itself: checkDeclaredChannels does that
// before the first cluster write.
//
// The summary is printed whatever happened, before the error is returned, so a
// partial failure still leaves the operator with what succeeded and with the
// command that finishes the rest.
func wireDeclaredChannels(
	ctx context.Context,
	b *oap.Bundle,
	kb *kube.Bundle,
	agentClass, instanceName string,
	out io.Writer,
	in io.Reader,
	driver tui.Driver,
	theme *tui.Theme,
) error {
	if b == nil || b.Manifest == nil || len(b.Manifest.Requires.Channels) == 0 {
		return nil
	}
	w := installChannelWiring(kb, in, out, driver, theme)
	// Checked before the planner rather than only inside wireChannels: a nil
	// client reaching PlanChannels is the offline path, and a TYPED-nil one
	// panics there instead of reporting anything at all.
	if err := w.check(); err != nil {
		return err
	}
	w.strat = clusterKindOrWarn(ctx, kb, out)

	if err := w.ensureWebhookAddress(ctx, b.Manifest.Requires.Channels); err != nil {
		return err
	}
	plans, err := w.planDeclaredChannels(ctx, b, agentClass, instanceName)
	if err != nil {
		return err
	}
	rep, wireErr := wireChannels(ctx, w, plans)
	fmt.Fprint(out, rep.String())
	return wireErr
}

// planDeclaredChannels is the read-only half of channel setup. Direct installs
// materialize their endpoint immediately before calling it; graph installs
// prepare endpoint execution separately and refresh the machine-derived URL
// only after that execution has completed.
func (w channelWiring) planDeclaredChannels(ctx context.Context, b *oap.Bundle, agentClass, instanceName string) ([]channelplan.ChannelPlan, error) {
	return channelplan.PlanChannels(ctx, w.kube.Controller, b, w.kube.Namespace, agentClass, instanceName)
}

type physicalChannelNames struct {
	channel    string
	credential string
}

// mappedChannelBundle gives the shared channel planner the physical names
// that Prepare uses for this graph node. The caller's bundle is never mutated.
func mappedChannelBundle(b *oap.Bundle, names instance.NameMap) (*oap.Bundle, map[string]physicalChannelNames, error) {
	if b == nil || b.Manifest == nil {
		return b, nil, nil
	}
	mapped := *b
	manifest := *b.Manifest
	requires := manifest.Requires
	requires.Channels = slices.Clone(requires.Channels)
	physical := make(map[string]physicalChannelNames, len(requires.Channels))
	for i := range requires.Channels {
		logical := strings.TrimSpace(requires.Channels[i].Name)
		channelName, ok := names["Channel/"+logical]
		if !ok || channelName == "" {
			return nil, nil, fmt.Errorf("declared channel %q has no physical resource name", logical)
		}
		credentialName, ok := names["Secret/"+logical+"-creds"]
		if !ok || credentialName == "" {
			return nil, nil, fmt.Errorf("declared channel %q credential Secret has no physical resource name", logical)
		}
		requires.Channels[i].Name = channelName
		physical[channelName] = physicalChannelNames{channel: channelName, credential: credentialName}
	}
	manifest.Requires = requires
	mapped.Manifest = &manifest
	return &mapped, physical, nil
}

// prepareDeclaredChannels collects ordinary human answers without mutating the
// cluster and stages private resolve/apply closures for graph execution.
func (w channelWiring) prepareDeclaredChannels(
	ctx context.Context,
	b *oap.Bundle,
	agentClass, instanceName string,
	resourceNames instance.NameMap,
	rootName string,
	path oap.DependencyPath,
	tracked bool,
) (preparedChannelPass, error) {
	if err := w.check(); err != nil {
		return preparedChannelPass{}, err
	}
	mapped, physical, err := mappedChannelBundle(b, resourceNames)
	if err != nil {
		return preparedChannelPass{}, err
	}
	plans, err := w.planDeclaredChannels(ctx, mapped, agentClass, instanceName)
	if err != nil {
		return preparedChannelPass{}, err
	}
	// A graph plan is executable once this function returns. Verdicts that no
	// wizard answer can change therefore belong on the read-only side of that
	// boundary: deferring them to wireOne would apply the bundle first and only
	// then discover that its required Channel can never be wired.
	for _, p := range plans {
		switch {
		case p.Conflict != nil:
			return preparedChannelPass{}, fmt.Errorf("required Channel %q cannot be wired: %s", strings.TrimSpace(p.Required.Name), p.Conflict.Reason)
		case !p.WiringKnown:
			return preparedChannelPass{}, fmt.Errorf(
				"required Channel %q cannot be wired: this install could not determine whether it already exists in namespace %q",
				strings.TrimSpace(p.Required.Name), w.kube.Namespace)
		}
	}
	endpoint, err := w.prepareWebhookAddress(ctx, mapped.Manifest.Requires.Channels)
	if err != nil {
		return preparedChannelPass{}, err
	}
	if endpoint.ReplacesLocalURL() {
		for i := range plans {
			delete(plans[i].Seeded, wizardkeys.KeyExternalBaseURL)
			delete(plans[i].SeededFrom, wizardkeys.KeyExternalBaseURL)
			plans[i].NotSeeded[wizardkeys.KeyExternalBaseURL] = "the local webd address is not reachable by webhook providers"
		}
	}

	w.chrome = channelsChrome(plans, w.theme, w.driver)
	prepared := make(map[string]*preparedChannelRun, len(plans))
	var credentialPreflights []install.ChannelCredentialPreflight
	for _, p := range plans {
		if p.Conflict != nil || p.AlreadyWired || !p.WiringKnown || w.driver == nil {
			continue
		}
		kind, ok := registry.Get(p.Required.Kind)
		if !ok {
			continue
		}
		w.announce(p)
		var credentialPreflight *install.ChannelCredentialPreflight
		if tracked {
			preflight, err := install.PreflightGraphChannelCredential(ctx, w.kube.Controller, rootName, w.kube.Namespace, physical[p.Required.Name].credential)
			if err != nil {
				return preparedChannelPass{}, err
			}
			credentialPreflight = &preflight
			credentialPreflights = append(credentialPreflights, preflight)
		}
		run, _, err := channelwizard.PrepareRun(ctx, kind.Wizard(), channelkinds.WizardInput{
			Namespace: w.kube.Namespace, Seeded: p.Seeded, WorkingDir: ".", OperatorShell: true,
		}, p.Required.Kind, tui.NewState(), p.Required.Role, channelWizardSeeds(p), map[string]bool{
			wizardkeys.KeyExternalBaseURL: endpoint.NeedsReadyURL(),
		}, w.runOptions(p))
		if err != nil {
			return preparedChannelPass{}, err
		}
		names := physical[p.Required.Name]
		prepared[p.Required.Name] = &preparedChannelRun{
			run: run, channelName: names.channel, credentialName: names.credential, agentClass: agentClass,
			tracked: tracked, rootName: rootName, path: slices.Clone(path), credentialPreflight: credentialPreflight,
		}
	}

	type executionState struct {
		runtimePlans []channelplan.ChannelPlan
		endpoint     *publicendpoint.AppliedWebdOnDemand
		resolved     bool
	}
	execution := &executionState{}
	pass := preparedChannelPass{
		plans: plans,
		sensitiveValues: func() []string {
			values := endpoint.SensitiveValues()
			for _, run := range prepared {
				values = append(values, run.run.SensitiveValues()...)
			}
			return values
		},
		resolve: func(resolveCtx context.Context) error {
			if execution.resolved {
				return nil
			}
			timeout := w.endpointReadyTimeout
			if timeout <= 0 {
				timeout = publicEndpointReadyTimeout
			}
			receipt, err := endpoint.Apply(resolveCtx, w.out, w.kube.Controller, w.strat, timeout)
			execution.endpoint = receipt
			if err != nil {
				return err
			}
			runtimePlans := make([]channelplan.ChannelPlan, len(plans))
			for i := range plans {
				runtimePlans[i] = plans[i]
				runtimePlans[i].Seeded = maps.Clone(plans[i].Seeded)
				runtimePlans[i].SeededFrom = maps.Clone(plans[i].SeededFrom)
				runtimePlans[i].NotSeeded = maps.Clone(plans[i].NotSeeded)
			}
			if endpoint.NeedsReadyURL() {
				for i := range runtimePlans {
					refreshed, err := channelplan.PlanOne(resolveCtx, w.kube.Controller, runtimePlans[i].Required, w.kube.Namespace, agentClass)
					if err != nil {
						return err
					}
					url := refreshed.Seeded[wizardkeys.KeyExternalBaseURL]
					if url == "" {
						return fmt.Errorf("PublicEndpoint %s is Ready, but webd has not published its external URL", publicendpoint.WebdName)
					}
					runtimePlans[i].Seeded[wizardkeys.KeyExternalBaseURL] = url
					runtimePlans[i].SeededFrom[wizardkeys.KeyExternalBaseURL] = refreshed.SeededFrom[wizardkeys.KeyExternalBaseURL]
					delete(runtimePlans[i].NotSeeded, wizardkeys.KeyExternalBaseURL)
				}
			}
			for _, p := range runtimePlans {
				run := prepared[p.Required.Name]
				if run == nil {
					continue
				}
				answered, err := channelwizard.ResolvePreparedRun(resolveCtx, run.run, p.Seeded, w.kube.Controller)
				if err != nil {
					return channelwizard.ReportUnfinished(w.out, w.theme,
						channelwizard.RunNotes(answered, channelkinds.WizardOutput{}), err)
				}
			}
			execution.runtimePlans = runtimePlans
			execution.resolved = true
			return nil
		},
		rollbackResolve: func(rollbackCtx context.Context) error {
			if execution.endpoint == nil {
				return nil
			}
			err := execution.endpoint.Rollback(rollbackCtx, w.kube.Controller)
			if err == nil {
				execution.endpoint = nil
			}
			return err
		},
		apply: func(applyCtx context.Context) error {
			if !execution.resolved {
				return errors.New("channel apply payload is not resolved")
			}
			w.prepared = prepared
			report, err := wireChannels(applyCtx, w, execution.runtimePlans)
			fmt.Fprint(w.out, report.String())
			return err
		},
		rollbackApplied: func(rollbackCtx context.Context) error {
			var errs []error
			for i := len(plans) - 1; i >= 0; i-- {
				run := prepared[plans[i].Required.Name]
				if run == nil || run.receipt == nil {
					continue
				}
				errs = append(errs, run.receipt.Rollback(rollbackCtx, w.kube.Controller))
			}
			return errors.Join(errs...)
		},
		finalize: func() {
			for _, run := range prepared {
				run.receipt = nil
			}
		},
		credentialPreflights: credentialPreflights,
	}
	return pass, nil
}

// wireChannels drives each declared channel's setup flow, in declaration
// order, and reports what became of every one of them.
//
// It returns a report ALWAYS — including alongside an error, and including for
// the failures that stopped the pass before any channel was attempted —
// because what succeeded is as much a fact as what did not, and the caller
// prints it either way.
//
// This cluster's public address is NOT opened here. Direct installs open it
// before planning; graph installs open it in their graph-wide resolution phase
// and refresh only the machine-derived URL before reaching this function.
func wireChannels(ctx context.Context, w channelWiring, plans []channelplan.ChannelPlan) (wireReport, error) {
	rep := wireReport{}
	if len(plans) == 0 {
		return rep, nil
	}
	if err := w.check(); err != nil {
		return rep, err
	}
	rep.namespace = w.kube.Namespace
	// Built HERE, once, from every plan — before the first channel is
	// attempted and therefore before the first question. A frame assembled as
	// the pass went would be a second record of what the pass is doing, and it
	// could only ever show the operator the channels already reached; the
	// planner has already decided the whole list.
	w.chrome = channelsChrome(plans, w.theme, w.driver)

	for _, p := range plans {
		rep.outcomes = append(rep.outcomes, w.wireOne(ctx, p))
	}
	return rep, rep.unwiredError()
}

// clusterKindOrWarn reads back the cluster kind `oap install` stamped, or says
// out loud that it could not.
//
// The stamp is the only honest source for this question. cloud.Detect scans
// node providerIDs and can never return `local` or `desktop` — neither reports
// a prefix, on purpose, so that neither can be auto-selected onto durable
// infrastructure — so detection on a desktop cluster answers "default", the one
// kind that must never open a tunnel.
//
// A failure WARNS and returns nil rather than failing the install. `oap agent
// install` runs routinely under a namespace-scoped kubeconfig that cannot read
// a Deployment in agentprimitives-system, and against clusters installed before
// the stamp existed; refusing the whole channels pass there would break installs
// that have nothing to do with tunnels. What it must not be is silent — the
// consequence is named here, where the answer was lost, so the operator is not
// left to discover it as a webhook that never arrives.
//
// The return type is the INTERFACE, and cloud.Stamped's own nil is what a
// failure yields: a typed-nil pointer assigned into an interface field compares
// non-nil and panics on the first method call.
func clusterKindOrWarn(ctx context.Context, kb *kube.Bundle, out io.Writer) cloud.Strategy {
	if kb.Typed == nil {
		cliout.Warn(out, "this install has no typed cluster client, so it cannot tell which cluster kind this is; "+
			"a channel that receives webhooks will get no tunnel")
		return nil
	}
	strat, err := cloud.Stamped(ctx, kb.Typed)
	if err != nil {
		cliout.Warn(out, "could not read this cluster's kind (%v); a channel that receives webhooks will get "+
			"no tunnel, and will use whatever external URL webd already advertises", err)
		return nil
	}
	return strat
}

// publicEndpointReadyTimeout bounds the wait for a just-created tunnel to
// publish its URL, and it is generous on purpose: the operator has to schedule
// the provider's agent, pull its image and complete a session with the provider
// before status.url exists, and a bound that expired mid-pull would fail an
// install that was about to work.
//
// It is a bound rather than an absence of one because the OTHER outcome is
// permanent: a cluster with no tunnel credential never reaches Ready at all.
const publicEndpointReadyTimeout = 3 * time.Minute

// ensureWebhookAddress opens this cluster's tunnel when — and only when — the
// declarations this pass is about to wire include one that receives inbound
// HTTP, and the cluster kind's policy is to open one on demand.
//
// THE TRIGGER IS THE DECLARATION, NOT THE CHANNEL, and it is the bundle's own
// declarations this reads rather than the plans made from them. The address
// has to exist before the wizard runs, because that is when it is registered
// with the provider; by the time a Channel exists the App already carries
// whatever URL it was given. A direct install opens it before its plan so the
// plan seeds the published URL. Graph planning must be read-only, so its
// prepared path defers that one machine-derived answer until execution.
//
// THE KIND IS ASKED, NEVER NAMED. `registry.NeedsWebhook` is the same
// WebhookReceiver seam webd mounts its routes from, so a channel kind added
// later inherits the tunnel without a line here changing. A `kind == "github"`
// here would be a second, drifting answer to a question the kind already
// answers.
func (w channelWiring) ensureWebhookAddress(ctx context.Context, declared []oap.RequiredChannel) error {
	// A cluster whose kind this run could not read (see channelWiring.strat)
	// opens nothing. The read's failure was already surfaced where it happened;
	// re-deciding it here from a nil would be guessing.
	if w.strat == nil {
		return nil
	}
	if w.strat.InstallProfile().PublicEndpointPolicy() != cloud.PublicEndpointOnDemand {
		return nil
	}
	if !slices.ContainsFunc(declared, func(rc oap.RequiredChannel) bool {
		return registry.NeedsWebhook(rc.Kind)
	}) {
		return nil
	}

	timeout := w.endpointReadyTimeout
	if timeout <= 0 {
		timeout = publicEndpointReadyTimeout
	}
	return publicendpoint.EnsureWebdOnDemandReady(ctx, w.out, w.kube.Controller, w.strat, timeout, w.tunnelTokenPrompt())
}

// prepareWebhookAddress makes the on-demand endpoint decision without
// creating its Secret or PublicEndpoint. The returned value keeps any token
// private until the graph executes.
func (w channelWiring) prepareWebhookAddress(ctx context.Context, declared []oap.RequiredChannel) (publicendpoint.WebdOnDemandPlan, error) {
	if w.strat == nil || w.strat.InstallProfile().PublicEndpointPolicy() != cloud.PublicEndpointOnDemand {
		return publicendpoint.WebdOnDemandPlan{}, nil
	}
	if !slices.ContainsFunc(declared, func(rc oap.RequiredChannel) bool {
		return registry.NeedsWebhook(rc.Kind)
	}) {
		return publicendpoint.WebdOnDemandPlan{}, nil
	}
	return publicendpoint.PrepareWebdOnDemand(ctx, w.kube.Controller, w.strat, w.tunnelTokenPrompt())
}

// tunnelTokenPrompt asks for the tunnel provider's credential, or is nil when
// nobody is at stdin — the same signal wireOne uses to decide whether a channel
// flow can run at all.
//
// The question is asked here rather than left to the environment because this
// is the one moment the answer is needed: the address it buys has to exist
// before the next screen registers it with a provider, and an operator who is
// already answering questions should not have to restart the run with an env
// var set.
func (w channelWiring) tunnelTokenPrompt() publicendpoint.TokenPrompter {
	if w.driver == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		q := tui.NewSecret(w.theme.Caps, tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:    tunnelTokenKey,
				Label: "Tunnel",
				Key:   tunnelTokenKey,
				Title: "ngrok authtoken",
				Guidance: func(*tui.State) string {
					return "A GitHub App's webhook URL has to be reachable from the internet, and this " +
						"cluster is not. An authtoken opens a tunnel to it for the App to deliver through.\n\n" +
						"Get one at https://dashboard.ngrok.com/get-started/your-authtoken — it is stored " +
						"as a Secret in this cluster and never printed.\n\n" +
						"Leave it blank to skip: the channel is still set up, but its webhook has nowhere " +
						"to arrive until a token is supplied."
				},
			},
			// Optional so that declining is an answer. Without a token the
			// endpoint stays Pending and the run says so — better than a
			// question an operator cannot get past.
			Optional: true,
		})

		// No chrome: this runs before the channel plans exist, so the rail
		// those screens are framed in has not been built yet.
		st, err := tui.RunWith(ctx, []tui.Screen{q},
			tui.Options{Driver: w.driver, Theme: w.theme, In: w.in, Out: w.out}, tui.NewState())
		if err != nil {
			return "", err
		}
		return st.Get(tunnelTokenKey), nil
	}
}

// tunnelTokenKey names the answer within its own one-question run. It is not a
// channel kind's question, so it shares no namespace with one.
const tunnelTokenKey = "tunnel-authtoken"

// channelsChromeTitle is the title bar over every channel question this pass
// asks. It says CHANNELS rather than just naming the command, because that is
// the half of the install the operator is now in: the bundle's own questions
// are behind them, and these answers will wire the channels it declared.
//
// It is not `oap · agent install`, and that is the defect this constant fixes.
// The channel questions are presented over the driver the bundle questions
// built (they share one stdin — see installQuestionPresentation), and a shared
// driver carries the chrome it was built with, so every channel screen was
// framed as one more install question: install's title, install's empty rail,
// redrawn per screen with nothing to say where the operator was.
const channelsChromeTitle = "oap · agent install · channels"

// channelsChrome is the frame for the whole channels pass: this pass's title,
// and one rail step per DECLARED channel, in the order they are processed.
//
// The steps come from the plan rather than from the channels this run gets as
// far as asking about. channelplan.PlanChannels has already decided every one
// of them — including the ones nothing will be asked for, because they are
// already wired or because their name is held by somebody else's Channel — so
// a rail derived from it is complete and correct before the first question,
// and it stays a view of the single record rather than becoming a second one.
//
// Nil is returned for a pass that cannot draw a frame, and both reasons are
// real: driver is nil for a scripted install (nobody is at the terminal, so
// no channel flow runs at all), and theme is nil alongside it. tui.NewChrome
// dereferences its theme to find the terminal width, so a chrome built for a
// run with neither would panic on the first render it will never do.
func channelsChrome(plans []channelplan.ChannelPlan, theme *tui.Theme, driver tui.Driver) *tui.Chrome {
	if driver == nil || theme == nil || len(plans) == 0 {
		return nil
	}
	steps := make([]tui.Step, 0, len(plans))
	for _, p := range plans {
		steps = append(steps, tui.Step{
			ID: channelStepID(p),
			// Cut by the rail's own budget: a declared Channel name is
			// routinely longer than a question prompt (`demo-reviewbot-slack`
			// is twenty columns), and one unbounded label costs every step its
			// rail rather than just overflowing its own row.
			Label: tui.RailLabel(channelStepID(p)),
		})
	}
	return tui.NewChrome(channelsChromeTitle, steps, theme)
}

// channelStepID is the rail step one declaration occupies.
//
// It is the declared Channel name, which is what the operator is answering
// FOR — the name the bundle's other CRs were written against, and the name in
// this pass's own announce line and summary. The kind would be a worse choice:
// a bundle may declare two channels of one kind, and a rail with two steps
// called `slack` cannot say which one is current.
func channelStepID(p channelplan.ChannelPlan) string {
	return strings.TrimSpace(p.Required.Name)
}

// check refuses a wiring pass that cannot do its job, before it reports a
// single channel as unwired for a reason that is really this command's own
// wiring bug.
//
// The nil checks on Controller and Dynamic are worth their lines: both are
// INTERFACE fields, and the hazard this repo has had in production is a
// TYPED-nil pointer assigned into one — which compares non-nil here and then
// panics on the first method call, from inside a kind's wizard, attributed to
// the kind. A genuine nil interface is caught here and named.
func (w channelWiring) check() error {
	switch {
	case w.kube == nil || w.kube.Controller == nil || w.kube.Dynamic == nil:
		return errors.New("cannot wire the declared channels: this install has no cluster connection to create them with")
	case w.out == nil:
		return errors.New("cannot wire the declared channels: no output stream to report on")
	case w.driver != nil && w.theme == nil:
		return errors.New("cannot wire the declared channels: a presentation driver with no theme (see installQuestionPresentation, which returns the two together)")
	}
	return nil
}

// wireOne decides what to do about one declaration and does it.
//
// The order of the guards is the order of the questions: is this name taken by
// something that is not ours, is it already ours, do we actually KNOW either
// way, can this build create the kind at all, and is anyone here to answer.
//
// That order is load-bearing, not narrative. Only the last question produces a
// skip, and a skip exits 0 — so every verdict a prompt could not have changed
// has to be reached first, or "nobody is at the terminal" becomes a blanket
// that turns a name collision into a green install.
func (w channelWiring) wireOne(ctx context.Context, p channelplan.ChannelPlan) channelOutcome {
	o := channelOutcome{plan: p}
	name := o.name()

	switch {
	// A Channel of this name that is NOT ours is a conflict, not a re-run
	// (channelplan B-R11), and skipping it is the specific failure that ruling
	// exists to prevent: every CR healthy, and an agent that can never receive
	// anything because its Channel is bound to somebody else. The planner
	// leaves AlreadyWired false alongside it precisely so a consumer reading
	// only that flag creates and is refused loudly rather than skipping
	// quietly; this reads the field and refuses in words instead.
	case p.Conflict != nil:
		o.status = channelUnwired
		o.reason = errors.New(p.Conflict.Reason)
		o.remedy = fmt.Sprintf(
			"resolve the collision first — rename or delete that Channel, or install this agent into a namespace of its own — then: %s",
			w.finishCommand(p))
		return o

	case p.AlreadyWired:
		o.status = channelAlreadyWired
		return o

	// AlreadyWired=false means "not wired" only when the planner actually
	// looked. When it did not — see ChannelPlan.WiringKnown — creating one
	// would be creating a Channel that may already exist, so this stops and
	// says which question it could not answer.
	case !p.WiringKnown:
		o.status = channelUnwired
		o.reason = fmt.Errorf(
			"this install could not determine whether a Channel named %q already exists in namespace %q, so it did not create one",
			name, w.kube.Namespace)
		o.remedy = "check for it, then: " + w.finishCommand(p)
		return o
	}

	// A kind this build does not link is an error naming it, never a skip.
	// LintRequiredChannels has already refused the same declaration before the
	// install touched the cluster — but that lint is also what `oap agent lint`
	// runs, and a check that lives only where an operator may not have looked
	// is not a check. This is the backstop, and it is the only one of the two
	// that runs against the registry THIS binary linked at the moment the
	// wizard is wanted.
	kind, registered := registry.Get(p.Required.Kind)
	if !registered {
		o.status = channelUnwired
		o.reason = fmt.Errorf("%q is not a channel kind this build of oap has; it has %s",
			p.Required.Kind, strings.Join(registry.Names(), ", "))
		o.remedy = "install a build of oap that registers this kind, then wire the channel with `oap channel create`"
		return o
	}

	// NOBODY IS AT STDIN: skip, say so, and exit 0 (spec §2.2). A scripted
	// install cannot host a browser handoff or answer a bot token, so failing
	// would fail every unattended install of every channel-declaring bundle —
	// a red CI job whose only remedy is to stop declaring channels. The
	// channel is named, with the command that wires it, which is what keeps
	// this a stated gap rather than a silent one.
	//
	// THE SKIP IS DECIDED HERE, AND THE FLOW IS NEVER ENTERED. That is not an
	// implementation detail of the skip, it IS the skip, and both halves of
	// the alternative were measured rather than assumed:
	//
	//   - Handing the run down with a "nobody is watching" hint
	//     (tui.Options.NonInteractive) reaches channelwizard's fail-closed
	//     presentation, which refuses with `supply --answer <key>=<value>` or
	//     `--name <value>`, `or drop --non-interactive`. `oap agent install`
	//     registers no `--answer` and no `--non-interactive` at all — and it
	//     DOES register `--name`, which is the worse half: there it means the
	//     install instance name, and checkDeclaredChannels refuses it outright
	//     for a bundle that declares channels. So the advice names one flag
	//     this command does not have and one it rejects for this very bundle.
	//     TestAgentInstallDoesNotOfferTheChannelCreateFlags pins that.
	//   - Entering the flow WITHOUT that hint is worse still. A nil driver
	//     resolves through tui.DriverFor to the line-oriented one, over a
	//     stream nobody is writing to, and huh's accessible renderer turns
	//     end-of-input into each field's default with a NIL ERROR (see
	//     tui.readerCanAnswer). An unattended install would then create a
	//     Channel and its Secret from answers nobody gave — a schedule and an
	//     authz subject the bundle author never chose — and report it wired.
	//     This is the second sighting of that huh shape, not a one-off: see
	//     tui.NewSecret, which masks a credential only where the driver can
	//     honor masking because the same accessible form silently collects an
	//     empty string, and reports success, where it cannot.
	//
	// channelkinds.WizardInput.NonInteractive changes neither outcome: it is a
	// hint to the KIND, and the kinds that do not read it behave as above.
	// Deciding before the flow starts is what makes both unreachable, and
	// leaves this command's own sentence as the only thing the operator reads.
	// See drive and runOptions.
	//
	// LAST OF THE GUARDS, deliberately. Everything above is a verdict a prompt
	// could not have changed: a name held by somebody else's Channel, a wiring
	// question the planner could not answer, a kind this build does not link.
	// Ordering this first would relabel all three as "skipped" and exit 0 on
	// them, which is the failure mode this whole distinction exists to avoid.
	if w.driver == nil {
		o.status = channelSkipped
		o.reason = errors.New("this install cannot prompt — nobody is at this terminal to answer this channel's setup questions — so it was not created")
		o.remedy = "run it where you can answer: " + w.finishCommand(p)
		return o
	}

	if err := w.drive(ctx, kind, &o); err != nil {
		o.status = channelUnwired
		o.reason = err
		o.remedy = "once the cause is fixed: " + w.finishCommand(p)
		return o
	}
	o.status = channelWired
	return o
}

// drive runs one kind's flow and lands what it produced.
//
// Everything between the questions and the manifests is channelwizard's: a
// direct install runs the whole flow here, while a graph install only reads
// the sealed result resolved before any graph-owned mutation began.
//
// On failure it renders what the run had ALREADY DONE before returning the
// cause, because some of it outlived the run and nothing else records it — the
// slack kind's provisioning route creates a real app that no API will list
// afterwards. That is channelwizard.ReportUnfinished's whole job, and it
// returns the cause unchanged so this still fails.
func (w channelWiring) drive(ctx context.Context, kind channelkinds.Kind, o *channelOutcome) error {
	p := o.plan
	var (
		wizOut   channelkinds.WizardOutput
		answered *tui.State
		err      error
	)
	prepared := w.prepared[p.Required.Name]
	if prepared != nil {
		wizOut, answered, err = channelwizard.FinishPreparedRun(prepared.run)
		wizardrun.NormalizeOutputIdentity(&wizOut, w.kube.Namespace, prepared.channelName, prepared.credentialName, prepared.agentClass)
	} else {
		w.announce(p)
		wizIn := channelkinds.WizardInput{
			K8s:       w.kube.Controller,
			Namespace: w.kube.Namespace,
			// The SAME map the driver consults to decide which questions to drop,
			// so a kind that shapes its question set around what is already
			// answered sees exactly what the run will see.
			Seeded: p.Seeded,
			// NonInteractive is deliberately left false, and it stays false even
			// though this command HAS an unattended mode: wireOne skips before it
			// gets here, so a run that reaches this line always has a driver
			// somebody is at.
			//
			// "." and not os.Getwd(): a kind that writes beside the run wants the
			// operator's current directory, whatever it is.
			WorkingDir: ".",
			// This command IS the operator's shell — the process they typed it
			// into, on their machine, with their PATH and their terminal.
			OperatorShell: true,
		}

		// The declared role travels with the run rather than being stamped onto
		// its output here: wizardrun.Finish is where both this client and admind
		// converge, and a stamp on one side of that join is a Channel that
		// silently takes the CRD's `both` default on the other. See
		// wizardrun.Params.Role.
		wizOut, answered, err = channelwizard.Run(
			ctx, kind.Wizard(), wizIn, p.Required.Kind, tui.NewState(),
			p.Required.Role, channelWizardSeeds(p), w.runOptions(p))
	}
	if err != nil {
		return channelwizard.ReportUnfinished(w.out, w.theme, channelwizard.RunNotes(answered, wizOut), err)
	}

	if prepared != nil && prepared.tracked {
		if wizOut.SecretManifest != nil {
			install.StampGraphOwnership(wizOut.SecretManifest, prepared.rootName, w.kube.Namespace, prepared.path)
		}
		if wizOut.ChannelManifest != nil {
			install.StampGraphOwnership(wizOut.ChannelManifest, prepared.rootName, w.kube.Namespace, prepared.path)
		}
		var guards []wizardrun.ObjectGuard
		if prepared.credentialPreflight != nil && wizOut.SecretManifest != nil {
			guards = append(guards, prepared.credentialPreflight.ApplyGuard())
		}
		prepared.receipt, err = wizardrun.ApplyTrackedGuarded(ctx, w.kube.Controller, kube.InstalledByValue, wizOut, guards)
	} else {
		err = channelwizard.Apply(ctx, w.kube, wizOut)
	}
	if err != nil {
		return channelwizard.ReportUnfinished(w.out, w.theme, channelwizard.RunNotes(answered, wizOut), err)
	}

	// Past this point the Channel is on the cluster, so nothing below may turn
	// this outcome into a failure: a summary that could not be written does
	// not unmake an object that exists.
	if warn := channelplan.DeclaredNameWarning(p.Required, wizOut.ChannelManifest); warn != "" {
		o.warnings = append(o.warnings, warn)
	}
	if err := tui.RenderSummary(w.out, w.theme, channelwizard.RunNotes(answered, wizOut)); err != nil {
		o.warnings = append(o.warnings, "this channel's run summary could not be written: "+err.Error())
	}
	channelwizard.RenderNextSteps(w.out, w.theme, wizOut.Notes)
	return nil
}

// announce says which channel is about to be set up and, in the bundle
// author's own words, what it is for.
//
// Purpose is printed HERE rather than in the summary because that is what it
// is for: an operator about to be asked for a GitHub organization needs to
// know which of the agent's channels they are answering for.
func (w channelWiring) announce(p channelplan.ChannelPlan) {
	fmt.Fprintf(w.out, "\n%s\n", w.theme.Render(w.theme.Title,
		fmt.Sprintf("Setting up the %s channel %q this agent requires", p.Required.Kind, p.Required.Name)))
	if purpose := strings.TrimSpace(p.Required.Purpose); purpose != "" {
		fmt.Fprintf(w.out, "%s\n", w.theme.Render(w.theme.Subtle, purpose))
	}
}

// channelWizardSeeds is a plan's answers in the shape channelwizard.Run takes
// them.
//
// KEYS IS DELIBERATELY LEFT NIL, and that is not an omission. Seeded.Keys is
// what an unknown `--answer` key is checked against (checkAnswerKeys), and its
// refusal quotes the flag the user typed. These answers came from the bundle's
// own declaration and from the cluster, not from a flag — `oap agent install`
// has no `--answer` — so there is no typo to catch and no flag to quote. A key
// no kind asks for is simply never consulted, which is exactly right for
// `external-base-url` against a kind that does not want one.
func channelWizardSeeds(p channelplan.ChannelPlan) channelwizard.Seeded {
	return channelwizard.Seeded{Values: p.Seeded}
}

// runOptions is how this pass's wizard questions are presented.
//
// Driver is this command's own, shared with every other question it asks: they
// read one stdin, and the line-oriented driver buffers what it reads.
//
// NonInteractive IS NEVER SET, INCLUDING FOR AN UNATTENDED INSTALL. It selects
// channelwizard's fail-closed presentation, whose refusals name `--answer`,
// `--name` and `--non-interactive`. Two of those this command does not
// register; the third, `--name`, it does — meaning something else entirely,
// and refused outright for a channel-declaring bundle. An unattended install
// does not reach here at all:
// wireOne records the channel as skipped and returns, so the mode is
// unreachable rather than merely unused, and the operator reads this command's
// own sentence instead of advice for a command they are not running.
//
// TITLE AND INLINE ARE STILL NOT SET, and setting them would change nothing:
// tui.Options reads both only when Driver is nil, and it never is here. That
// is precisely why this pass looked like a different program — a run presented
// over somebody else's driver inherits somebody else's chrome, and the chrome
// this one inherited was the bundle questions' (title `oap · agent install`,
// no rail at all), redrawn from scratch around every screen batch.
//
// tui.Reframe is the fix, and it is a fix to the PRESENTATION rather than to
// the run. Merging this pass's several runs into one is not available: each
// channel interleaves a browser round trip and a network Resolve BETWEEN
// question batches (see channelwizard.Run), and a tui run held open across
// that is not a run. So one Chrome — built once from the plan, in
// channelsChrome — is shared across all of them instead, with this channel's
// step marked active. The driver underneath is still the very same one, which
// is what keeps the single stdin buffer intact.
func (w channelWiring) runOptions(p channelplan.ChannelPlan) tui.Options {
	return tui.Options{
		Theme:  w.theme,
		In:     w.in,
		Out:    w.out,
		Driver: tui.Reframe(w.driver, w.chrome, channelStepID(p)),
	}
}

// finishCommand is what the operator types to wire this channel by hand.
//
// The namespace is spelled out rather than left to the ambient context: an
// install that named one put the agent there, and a copy-pasted command that
// silently used a different default would create the Channel where nothing is
// looking for it.
//
// THE ROLE IS SPELLED OUT FOR THE SAME REASON, and its absence was a defect
// rather than a terseness. This command is what a scripted install prints for
// every channel it skipped, and it is what the bundle's README tells an
// operator to run. A declared `role: output` that the paste cannot express
// leaves the operator with the CRD's `both` default — which is deliberately
// not an output-binding candidate — so the agent's other Channel goes
// Valid=False and nothing the operator can type fixes it. Omitted when the
// declaration names no role: `oap channel create` then leaves the kind's own
// answer alone, which is what this run would have done too.
func (w channelWiring) finishCommand(p channelplan.ChannelPlan) string {
	cmd := fmt.Sprintf("oap channel create --kind %s --name %s --namespace %s",
		p.Required.Kind, strings.TrimSpace(p.Required.Name), w.kube.Namespace)
	if role := strings.TrimSpace(p.Required.Role); role != "" {
		cmd += " --role " + role
	}
	return cmd
}

// checkDeclaredChannels is this command's RENDERING of the one install-time
// gate over a bundle's requires.channels, run BEFORE anything touches the
// cluster.
//
// The verdict is channelplan.CheckDeclared's — the --name refusal and the
// declaration lint, in one call — because admind's install endpoint reaches
// the same decision from an HTTP handler and calling two functions in two
// clients is what let admind call only one of them. What is HERE is what a
// terminal needs and a 400 body does not: the findings laid out a line apiece,
// and the pointer at `oap agent lint`, which is the command an author of the
// bundle would run to see the same list.
//
// A bundle that declares no channel raises no finding, so this changes nothing
// for every bundle written before requires.channels existed. It still decodes
// every bundled CR for the credentials cross-check, and a bundle whose
// manifests do not decode is an error from here. That is unreachable today
// only because checkSkillsShape decodes the same CRs a few lines earlier and
// fails first — an ordering, not a property, and worth saying so rather than
// letting a future reader rely on the stronger claim.
func checkDeclaredChannels(b *oap.Bundle, instanceName string) error {
	err := channelplan.CheckDeclared(b, instanceName)
	var declErr *channelplan.DeclarationError
	if !errors.As(err, &declErr) {
		// Nil, the --name refusal, or a failure to read the bundle at all —
		// each already says what is wrong in words this command has nothing to
		// add to.
		return err
	}
	var msg strings.Builder
	fmt.Fprintln(&msg, "this bundle's requires.channels declaration cannot be installed — refused before touching the cluster:")
	for _, f := range declErr.Findings {
		fmt.Fprintln(&msg, f.String())
	}
	fmt.Fprint(&msg, "`oap agent lint <bundle>` reports the same findings against a bundle you are authoring")
	return errors.New(msg.String())
}
