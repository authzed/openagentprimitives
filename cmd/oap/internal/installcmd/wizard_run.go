package installcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	idpregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// Rail-step IDs for the unified init wizard's long-running phases — the slots
// that are driven imperatively rather than by a tui.Question screen. The config
// screens key off the Task-4 constants (keyDetected/keyWorkspaceClass/…) and the
// routing screens off the flag constants (flagTrustedHostname/…); only Build,
// Install, and Status need their own IDs. Kept beside initRailSteps so the rail
// and StepIndex lookups cannot disagree on the string.
const (
	stepBuild   = "init.build"
	stepInstall = "init.install"
	stepStatus  = "init.status"
	// stepDirectory is the directory-sync configure offer's rail slot (M5). It
	// sits between Settings and Identity, matching the linear tail's order
	// (runInit: settings wizard offer, then runInitDirectoryScreenOffer, then
	// the IdP/monitoring offers — init.go).
	stepDirectory = "init.directory"
)

// initRailSteps is the canonical rail — one slot per phase, in order. The config
// screens (Detected → Proceed) each present under their own ID, which equals
// their rail step, so no reframing is needed. The composite post-install phases
// (Settings/Directory/Identity/Monitoring) occupy one slot each; the
// long-running phases (Build/Install/Status) are driven imperatively with the
// rail gutter.
var initRailSteps = []tui.Step{
	{ID: keyDetected, Label: "Detected"},
	{ID: keyWorkspaceClass, Label: "Workspace"},
	{ID: keyExternalSpiceDB, Label: "SpiceDB"},
	{ID: flagTrustedHostname, Label: "Routing"},
	{ID: keyProceed, Label: "Proceed"},
	{ID: stepBuild, Label: "Build"},
	{ID: stepInstall, Label: "Install"},
	{ID: stepStatus, Label: "Status"},
	{ID: keySettings, Label: "Settings"},
	{ID: stepDirectory, Label: "Directory"},
	{ID: keyIdP, Label: "Identity"},
	{ID: keyMonitoring, Label: "Monitoring"},
}

// railState is the shared active-step index the orchestrator advances as it
// moves through the imperative phases. The install checklist's RailProvider
// reads Steps()/Active() fresh on every redraw, so advancing active repaints the
// gutter with no extra plumbing. It implements progress.RailProvider
// structurally (Steps + Active), which is why it must be passed to RunInstall as
// a genuine non-nil *railState — NewWithRail's nil check is an interface compare.
type railState struct {
	chrome *tui.Chrome
	active int
}

func (r *railState) Steps() []tui.Step { return initRailSteps }
func (r *railState) Active() int       { return r.active }

// initWizardParams bundles everything runInitWizard consumes that runInit has
// already resolved by the time it dispatches (Task 12 wires the dispatch): the
// cluster strategy and image registry/platform (resolved in runInit's preflight
// block), the tag set, the routing opts, and the flags that steer the phases.
// The orchestrator resolves the kube.Bundle itself (newInitWizardBundleFn) so a
// test can inject a fake-backed one.
type initWizardParams struct {
	strat            cloud.Strategy
	registry         string
	platform         string
	tags             manifests.Tags
	r                WebdRoutingOpts
	pinningMode      string
	artifactStoreURL string
	imagePullSecret  string
	mirrorDeps       bool
	createRegistry   bool
	timeout          time.Duration

	skipBuild      bool
	acceptExisting bool
	defaults       bool
	noIdp          bool
	noMonitoring   bool
	dm             settingscmd.DefaultModelOpts

	externalSpiceDBEndpoint string
	externalSpiceDBToken    string
	externalSpiceDBInsecure bool
}

// Package-var seams so a same-package test drives the init flow without a
// cluster, mirroring runDirectoryConfigureFn (init.go) and directorycmd's
// driverForRun. Production points each at the real implementation; a test swaps
// in a spy that records inputs or returns fakes.
//
// runBuildFn/runInstallFn/checksFn are shared by BOTH paths: runInitWizard's
// imperative Build→Install→Status phases and the linear runInit tail (init.go)
// call through these same vars, so one test can drive either path's
// build/install/check without images or a live cluster. runInitWizardFn is the
// dispatch seam runInit uses to reach the wizard — spying it proves runInit
// routes --wizard / --accept-existing here and takes the linear path otherwise.
var (
	newInitWizardBundleFn = func(g *apcmd.Globals) (*kube.Bundle, error) { return g.Bundle() }
	driverForInitWizard   = tui.DriverFor
	runBuildFn            = runBuild
	runInstallFn          = RunInstall
	checksFn              = waitUntilChecksPass
	runInitWizardFn       = runInitWizard
	// stdinInteractiveFn reports whether the wizard may prompt (stdin is a TTY).
	// A package-var seam — defaulting to the real isInteractive — so a test can
	// force the interactive vs. non-interactive branch of the workspace fold
	// (a fabricated non-TTY pick must never become a billable override)
	// deterministically, independent of the test process's own stdin.
	stdinInteractiveFn = isInteractive
)

// runInitWizard is the unified `oap init` wizard: one theme, one Chrome, and one
// driver span every phase, so the config questions, the install checklist, and
// the post-install setup flows all render under a single step rail. It is not
// yet dispatched from runInit — Task 12 wires that — but is fully callable.
func runInitWizard(ctx context.Context, out io.Writer, g *apcmd.Globals, p initWizardParams) error {
	b, err := newInitWizardBundleFn(g)
	if err != nil {
		return err
	}

	d, err := detectSettings(ctx, detectDeps{Dyn: b.Dynamic, Ctrl: b.Controller, Typed: b.Typed, Namespace: b.Namespace})
	if err != nil {
		return err
	}
	mode, err := resolveAcceptMode(p.acceptExisting, p.defaults, p.r.assumeYes, d)
	if err != nil {
		return err
	}

	// One theme + one Chrome + one driver + one shared Options for the whole run.
	theme := g.Theme(out)
	chrome := tui.NewChrome("oap init · agent platform installer", initRailSteps, theme)
	rail := &railState{chrome: chrome}
	// Inline: true — this driver spans the whole wizard (config screens, then
	// the post-install IdP/Monitoring screens), and the install checklist's
	// build/status output streams between those screens on the SAME terminal
	// buffer. Options.Inline's own rule (pkg/cli/tui/run.go) says the shared-driver
	// unit classifies inline the moment ANY run on it depends on surrounding
	// output — the post-install screens read the checklist output printed just
	// above them, so the alternate screen (which hides scrollback) must never
	// take over for any screen on this driver, config screens included.
	driver := driverForInitWizard(tui.DriverParams{Theme: theme, Chrome: chrome, In: os.Stdin, Out: out, Inline: true})
	runOpts := tui.Options{Theme: theme, Title: "oap init", In: os.Stdin, Out: out, Driver: driver}

	st := tui.NewState()
	if mode == acceptAllExisting {
		st.Set(keyDetected, detectedAccept)
		seedStateFromDetected(st, d, p.r)
	}
	// The ACME-email routing screen reads flagTLSIssuer to decide it is
	// unnecessary; seed it from r.tlsIssuer, mirroring askWebdRoutingInputs'
	// own seed (routing_ask.go). No screen ever asks for it.
	if p.r.tlsIssuer != "" {
		st.Set(flagTLSIssuer, p.r.tlsIssuer)
	}

	// Config phase, config-first. The Detected screen runs first; if it resolves
	// to "accept" (seeded or live-picked), re-seed so the remaining config
	// screens short-circuit. Each screen presents under its own ID == rail step.
	st, err = tui.RunWith(ctx, []tui.Screen{newDetectedScreen(d)}, runOpts, st)
	if err != nil {
		return err
	}
	if st.Get(keyDetected) == detectedAccept {
		seedStateFromDetected(st, d, p.r)
	}

	// A routing value supplied on the command line — --trusted-hostname,
	// --sandbox-hostname, --acme-email, or --hostname-suffix (expanded into the
	// two hostnames in runInit before dispatch) — is already the answer. Seed it
	// so its screen short-circuits, mirroring the linear askWebdRoutingInputs,
	// which declares an Input only for a still-empty field. Seeded after
	// detection so a flag wins over a detected value.
	if p.r.trustedHostname != "" {
		st.Set(flagTrustedHostname, p.r.trustedHostname)
	}
	if p.r.sandboxHostname != "" {
		st.Set(flagSandboxHostname, p.r.sandboxHostname)
	}
	if p.r.acmeEmail != "" {
		st.Set(flagACMEEmail, p.r.acmeEmail)
	}
	// --external-spicedb-endpoint is already the answer to the SpiceDB screen's
	// "use an existing external instance?" — seed it so the screen short-circuits
	// (State.Has → nil group) and the rail marks it answered, mirroring the
	// routing-flag seeds above. Without this the screen still asks and its answer
	// is discarded, since resolveExternalSpiceDBFromState gives the flag absolute
	// precedence. Seeded true because a supplied endpoint means "external".
	if p.externalSpiceDBEndpoint != "" {
		st.SetBool(keyExternalSpiceDB, true)
	}

	// A list failure must not break the wizard — it falls through to
	// RunInstall's own WorkspaceResolveOptions detection, same as an empty
	// result — but it must not vanish silently either (no-silent-errors).
	classes, cerr := cloud.ListRWXClasses(ctx, b.Typed)
	if cerr != nil {
		cliout.Warn(out, "list RWX-capable storage classes: %v", cerr)
	}
	recommended := ""
	// The recommendation only annotates the Workspace screen's menu, so compute
	// it (a cloud-API read on GKE) only when that screen will actually present:
	// it needs RWX classes to choose from AND an unseeded key. Accept-all seeds
	// the key and the screen short-circuits, discarding any recommendation — so
	// don't pay for it there.
	if len(classes) > 0 && !st.Has(keyWorkspaceClass) && p.strat != nil {
		recommended = detectRecommendedWorkspaceClass(ctx, b, p.strat, newCloudReporter(out))
	}
	// Workspace + SpiceDB present under their own IDs, which equal their rail
	// steps, so they run over the shared (un-reframed) driver.
	//
	// A Choice with no options is a fatal Prepare error even when its key is
	// seeded, so the Workspace screen is offered only when the cluster actually
	// has RWX classes to choose from. With none, the seeded/blank workspace key
	// falls through to RunInstall's own WorkspaceResolveOptions detection.
	var configPre []tui.Screen
	if len(classes) > 0 {
		configPre = append(configPre, newWorkspaceScreen(classes, d, recommended))
	}
	configPre = append(configPre, newSpiceDBScreen(d))
	st, err = tui.RunWith(ctx, configPre, runOpts, st)
	if err != nil {
		return err
	}

	// Routing is ONE rail slot. Its three screens carry distinct IDs
	// (trusted/sandbox/acme), but only trusted-hostname is a rail step, so
	// sandbox-hostname and acme-email would resolve to StepIndex == -1 under
	// their own IDs and blank every rail step while they present. Run them over a
	// driver reframed onto the Routing slot, mirroring how runSettingsReframed
	// pins the Settings phase. Seeded (accept-all) runs are unaffected: a
	// non-chrome/Plain driver is returned unchanged by tui.Reframe, and a seeded
	// routing key short-circuits before any driver is consulted.
	//
	// p.strat is resolved on every real dispatch into the wizard (same
	// invariant the IdP/Monitoring gate below documents), but the nil guard
	// matches that sibling's defensive shape rather than dereferencing an
	// interface this function does not itself construct.
	routingProfile := cloud.ProductionProfile
	if p.strat != nil {
		routingProfile = p.strat.InstallProfile()
	}
	st, err = tui.RunWith(ctx, newRoutingScreens(routingProfile, p.r, d), routingRunOptions(runOpts, driver, chrome), st)
	if err != nil {
		return err
	}

	// Proceed presents under its own ID (== its rail step), over the shared driver.
	st, err = tui.RunWith(ctx, []tui.Screen{newProceedScreen()}, runOpts, st)
	if err != nil {
		return err
	}
	if !proceedConfirmed(st) {
		// Mirrors confirmPlan's own decline line (install.go) — a decline here
		// must not exit silently just because this run never reached
		// confirmPlan at all.
		cliout.Info(out, "aborted; nothing was changed.")
		if e := tui.RenderSummary(out, theme, st.Notes()); e != nil {
			cliout.Warn(out, "render summary: %v", e)
		}
		return nil // declined at Proceed — nothing applied.
	}

	// Fold the gathered answers into RunInstall's inputs.
	p.r = applyRoutingAnswers(p.r, st)
	wsOpts := wizardWorkspaceResolveOptions(st.Get(keyWorkspaceClass), d.WorkspaceClass, classes, stdinInteractiveFn(), func(class string) bool {
		return confirmBillableWorkspaceClass(os.Stdin, out, class, p.r.assumeYes)
	})
	ext, err := resolveExternalSpiceDBFromState(os.Stdin, st, out, p, d)
	if err != nil {
		return err
	}
	// Fail closed rather than silently flip an external backend to the bundled
	// in-cluster one: if the cluster's detected SpiceDB is external but the
	// resolved decision is bundled, RunInstall's ensureSpiceDBEndpointConfig would
	// overwrite the shared endpoint ConfigMap to the in-cluster Service and orphan
	// the external instance (and every relationship in it). Refuse instead.
	if gErr := refuseExternalToBundledFlip(d, ext); gErr != nil {
		return gErr
	}
	p.r.preconfirmed = true // the Proceed step already confirmed; RunInstall must not re-ask.

	// Build → Install → Status, advancing the rail before each.
	rail.active = chrome.StepIndex(stepBuild)
	if !p.skipBuild {
		built, berr := runBuildFn(ctx, out, "all", "", false, p.registry, p.platform, p.mirrorDeps, p.createRegistry, false /*raw*/, g, rail)
		if berr != nil {
			return berr
		}
		// --no-digest-pin keeps remote images on their mutable tag; any other
		// remote install carries the build's digests into the install so it pins
		// by digest. Mirrors runInit's own gate.
		if !p.r.noDigestPin {
			p.tags.Digests = built
		}
	}

	// Wrap the install context exactly as the linear runInit does (init.go): a
	// SIGINT cancels sigCtx, which becomes RunInstall's recheckCtx so an exhausted
	// overall timeout does not turn the "keep waiting?" Y into a no-op, and
	// installCtx additionally bounds the install by p.timeout. Build above and the
	// Status checks below run on the plain ctx with their own deadline arg,
	// matching the linear path — only the install phase is timeout-bounded here.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	installCtx, cancel := context.WithTimeout(sigCtx, p.timeout)
	defer cancel()

	rail.active = chrome.StepIndex(stepInstall)
	if ierr := runInstallFn(installCtx, sigCtx, InstallConfig{
		Out:                 out,
		G:                   g,
		Tags:                p.tags,
		DryRun:              "",
		Routing:             p.r,
		PinningMode:         p.pinningMode,
		Strat:               p.strat,
		Ext:                 ext,
		Workspace:           wsOpts,
		Stateful:            StatefulResolveOptions{},
		Artifact:            ArtifactStoreOptions{ExplicitURL: p.artifactStoreURL},
		ImagePullSecret:     p.imagePullSecret,
		MirrorDeps:          p.mirrorDeps,
		ImagesPrebaked:      false,
		ClampUndersizedPVCs: false,
		WithoutBuilder:      true,
		BuilderStarters:     nil,
		Rail:                rail,
	}); ierr != nil {
		return ierr
	}

	// The same post-install SpiceDB schema tail the linear runInit runs (init.go).
	// An external SpiceDB has no startup bootstrap, so this applySchemaToEndpoint
	// is its ONLY schema-write path — the wizard used to skip it and come up
	// schema-less with green checks. Best-effort (warn, never fail), and run
	// BEFORE the status checks, matching the linear ordering. No live checklist
	// region is up here (RunInstall closed its reporter; checksFn opens its own),
	// so the helper's direct writes are safe.
	applyExternalSpiceDBSchemaTail(ctx, out, ext)

	rail.active = chrome.StepIndex(stepStatus)
	if cerr := checksFn(ctx, out, b, p.timeout, rail); cerr != nil {
		return cerr
	}

	// Post-install composite phases. Each is best-effort: a failure here does
	// NOT fail `oap init` — the install already succeeded — so it warns and the
	// run continues to the summary.

	// Settings mirrors the linear path's own non-TTY gate (decideWizard,
	// init.go): a piped/CI run with no --defaults must not run the
	// interactive settings form at all. huh's accessible renderer turns an
	// EOF into each field's FABRICATED default with a nil error
	// (pkg/cli/tui/driver.go), and RunWizardWithDriver composes and applies
	// the result with no final confirm — so an unguarded run here would write
	// a ClusterAgentSettings baseline nobody chose. --defaults instead takes
	// its own explicit non-interactive merge-with-existing path, which is a
	// deliberate baseline apply, not a fabrication, so it always runs.
	if p.defaults || stdinInteractiveFn() {
		rail.active = chrome.StepIndex(keySettings)
		if serr := runSettingsReframed(ctx, out, g, chrome, driver, settingsReframeParams{
			defaults: p.defaults, registry: p.registry, digests: p.tags.Digests, dm: p.dm,
		}); serr != nil {
			cliout.Warn(out, "settings wizard failed: %v", serr)
		}
	}

	// Directory-sync configure — restores parity with the pre-unification
	// `--wizard` flow, which ran runInitDirectoryScreenOffer right after the
	// settings offer (init.go). Gated exactly as the linear path gates it: a
	// non-TTY (piped/CI) run and a --defaults run both SKIP, matching
	// runInitDirectoryScreenOffer's own wizardSkip/wizardDefaults arms and the
	// deliberate safety mapping in its doc comment — a non-interactive install
	// must never fabricate a directory-sync answer (huh's accessible renderer
	// turns EOF into a chosen kind) against a credential it picked, and --defaults
	// has no recommended set of directories to sync. Only a real interactive TTY
	// without --defaults runs the configure. Non-fatal, like every other
	// post-install phase.
	rail.active = chrome.StepIndex(stepDirectory)
	dirMode := wizardSkip
	if !p.defaults && stdinInteractiveFn() {
		dirMode = wizardInteractive
	}
	if derr := runInitDirectoryScreenOffer(ctx, os.Stdin, out, g, dirMode); derr != nil {
		cliout.Warn(out, "directory-sync setup: %v", derr)
	}

	// installProfile feeds the same PromptsForIdentityProvider/
	// PromptsForMonitoring gates the linear path's shouldPromptIdp/
	// shouldPromptMonitoring read (init.go) — a dev-profile cluster (local,
	// desktop) must not present the Identity/Monitoring screens just because
	// !p.noIdp/!p.noMonitoring says nothing against it. The stdinInteractiveFn()
	// conjunct on each gate below completes the parity: shouldPromptIdp/
	// shouldPromptMonitoring also require isTTY, so a non-TTY (piped/CI) run must
	// not present either screen — otherwise huh's accessible renderer would
	// fabricate an answer on EOF and a stray piped line could select a real kind.
	// p.strat is resolved (a genuine non-nil interface) on every real dispatch
	// into the wizard — runInit only reaches here when there is install work to
	// do, which resolves strat first — but the nil guard mirrors init.go's own
	// defensive shape for a directly-constructed caller.
	installProfile := cloud.ProductionProfile
	if p.strat != nil {
		installProfile = p.strat.InstallProfile()
	}

	if !p.noIdp && installProfile.PromptsForIdentityProvider() && stdinInteractiveFn() {
		rail.active = chrome.StepIndex(keyIdP)
		st, err = tui.RunWith(ctx, []tui.Screen{newIdPScreen(nonFakeIdPKinds(), d)}, runOpts, st)
		switch {
		case err != nil:
			cliout.Warn(out, "identity provider selection: %v", err)
		default:
			// Run setup only for a real kind the operator picked that differs
			// from the one already configured — "none", blank, and the current
			// kind are all no-ops.
			if chosen := st.Get(keyIdP); chosen != "" && chosen != idpNone && chosen != d.IdPKind {
				if e := identitycmd.RunIdpSetup(ctx, os.Stdin, out, g, b.Controller, chosen); e != nil {
					cliout.Warn(out, "identity provider setup: %v", e)
				}
			}
		}
	}

	if !p.noMonitoring && installProfile.PromptsForMonitoring() && stdinInteractiveFn() {
		rail.active = chrome.StepIndex(keyMonitoring)
		// keyMonitoring is seeded ONLY to monitoringKeepValue (seedStateFromDetected),
		// and only when detectSettings already found a channel to set
		// d.MonitoringChannel from — so a seeded key here means that list already
		// ran once this invocation. Re-listing would be a pure duplicate, and the
		// screen's own Prepare short-circuits without presenting for a seeded key,
		// so nobody reads existingMon for display either way. But Apply still
		// validates the seeded "keep" against this list's options (applyChoice
		// fails closed on a value no option offered), so skipping straight to an
		// EMPTY list would turn a harmless duplicate read into a hard failure on
		// every accept-all run with an existing channel — hence one placeholder
		// entry (only its presence matters) instead of a second network round-trip.
		var existingMon []spiceboxv1alpha1.Channel
		if st.Has(keyMonitoring) {
			existingMon = []spiceboxv1alpha1.Channel{{}}
		} else {
			var lerr error
			existingMon, lerr = channelcmd.ListMonitoringChannels(ctx, b.Controller, b.Namespace)
			if lerr != nil {
				cliout.Warn(out, "check existing monitoring channels: %v", lerr)
				existingMon = nil
			}
		}
		st, err = tui.RunWith(ctx, []tui.Screen{newMonitoringScreen(existingMon, d)}, runOpts, st)
		switch {
		case err != nil:
			cliout.Warn(out, "monitoring channel selection: %v", err)
		case st.Get(keyMonitoring) == monitoringSetupValue:
			// Reuse runInitMonitoringOffer's kind-selection path rather than
			// inventing a picker.
			scanner := bufio.NewScanner(os.Stdin)
			if kindName := pickMonitoringKind(scanner, out); kindName != "" {
				if e := channelcmd.RunMonitoringCreate(ctx, os.Stdin, out, g, kindName); e != nil {
					cliout.Warn(out, "monitoring channel setup: %v", e)
				}
			}
		}
	}

	if e := tui.RenderSummary(out, theme, st.Notes()); e != nil {
		cliout.Warn(out, "render summary: %v", e)
	}
	return nil
}

// wizardWorkspaceResolveOptions folds the Workspace screen's raw answer
// (chosen) into the WorkspaceResolveOptions RunInstall consumes.
// Preconfirmed is always true: by the time this runs, the wizard has either
// presented the Workspace screen and gathered chosen, or short-circuited it
// (accept-all, or no RWX classes to choose from) — either way RunInstall must
// not re-ask via its mid-install reselect picker.
//
// chosen == workspaceIsolatedValue (N3) is the "no shared RWX workspace"
// pick: it is folded into NoRWX, never into ExplicitClass — "isolated" is a
// synthetic sentinel, not a StorageClass name, and forwarding it as
// ExplicitClass would send resolveWorkspaceDecision off probing a class that
// does not exist. Every other chosen value (including "", meaning the screen
// was never asked or the answer was kept/declined/fabricated) defers to
// wizardWorkspaceExplicitClass's own interactive/billable-consent gating —
// see its doc comment for why only an actual, interactively-consented change
// of class becomes an explicit override RunInstall probes.
func wizardWorkspaceResolveOptions(chosen, detected string, classes []cloud.RWXClassInfo, interactive bool, confirmBillable func(class string) bool) WorkspaceResolveOptions {
	if chosen == workspaceIsolatedValue {
		return WorkspaceResolveOptions{NoRWX: true, Preconfirmed: true}
	}
	return WorkspaceResolveOptions{
		ExplicitClass: wizardWorkspaceExplicitClass(chosen, detected, classes, interactive, confirmBillable),
		Preconfirmed:  true,
	}
}

// confirmBillableWorkspaceClass warns that class is billable and asks for
// explicit consent before the wizard pins it. It mirrors the linear cost-confirm
// (decisionToChoice's WorkspaceCostConfirmBeforeUse arm): the Workspace screen
// labels a Filestore class but never surfaces the bill, so a user who picks one
// must still consent, exactly as the flag-driven install does. It runs in the
// config phase (no live checklist region yet), so it prompts on out/in directly.
// assumeYes (from -y) auto-accepts, matching the linear consent under -y.
func confirmBillableWorkspaceClass(in io.Reader, out io.Writer, class string, assumeYes bool) bool {
	cliout.Warn(out, "%s", billableWorkspaceWarning)
	return apcmd.Confirm(in, out, billableWorkspaceConfirmPrompt(class), assumeYes, stdinInteractiveFn())
}

// resolveExternalSpiceDBFromState resolves the external-SpiceDB connection for
// the install. An --external-spicedb-endpoint flag fully specifies it and wins
// outright (mirroring resolveExternalSpiceDB's own flag-first precedence).
// Otherwise, when the SpiceDB screen (or a detected-external seed) resolved to
// "use external", the endpoint and token are collected interactively; when it
// did not, the bundled SpiceDB is installed (the zero value).
//
// Crucially it collects the endpoint/token DIRECTLY (collectExternalSpiceDBEndpoint),
// never through resolveExternalSpiceDB — that would re-ask the very "use
// external?" confirm the screen just answered, and its default No would reverse
// a Yes on a bare Enter. And when external was chosen but there is no terminal
// to collect an endpoint on, it fails LOUDLY rather than silently falling back
// to the bundled SpiceDB against the user's stated intent.
//
// d supplies the detected endpoint (R1): when keeping an already-external
// cluster, d.ExternalSpiceDBEndpoint pre-fills the prompt so a bare Enter
// keeps the connection the cluster is already using instead of being
// re-typed. The token is never pre-filled — it is a secret detection never
// read — so it is still prompted for regardless.
//
// in is threaded explicitly (rather than reading os.Stdin internally, as it
// did before) so a test can drive the interactive collection path with a
// fake reader; the sole production call site (runInitWizard) still passes
// os.Stdin.
func resolveExternalSpiceDBFromState(in io.Reader, st *tui.State, out io.Writer, p initWizardParams, d DetectedSettings) (ExternalSpiceDB, error) {
	if p.externalSpiceDBEndpoint != "" {
		return resolveExternalSpiceDB(in, out, p.externalSpiceDBEndpoint, p.externalSpiceDBToken, p.externalSpiceDBInsecure, stdinInteractiveFn()), nil
	}
	if !st.Bool(keyExternalSpiceDB) {
		return ExternalSpiceDB{}, nil
	}
	if !stdinInteractiveFn() {
		return ExternalSpiceDB{}, fmt.Errorf("the wizard chose an external SpiceDB but no endpoint can be collected without a terminal; pass --external-spicedb-endpoint (and --external-spicedb-token) to supply it non-interactively")
	}
	return collectExternalSpiceDBEndpoint(in, out, true /*isTTY: gated just above*/, d.ExternalSpiceDBEndpoint), nil
}

// refuseExternalToBundledFlip fails closed when the cluster's detected SpiceDB
// backend is external but the wizard resolved to install the bundled in-cluster
// one. Left unchecked, RunInstall's ensureSpiceDBEndpointConfig overwrites the
// shared endpoint ConfigMap to the in-cluster Service and installs the bundled
// stack, silently orphaning the external instance and every relationship in it —
// which accept-all's fabricated bundled default (on a bare Enter or a non-TTY
// EOF) would otherwise trigger.
//
// The wizard has no safe "keep the current external config" path today: it
// cannot re-derive the stored bearer token or the TLS/insecure setting without a
// terminal, and RunInstall re-applies the base component Deployments, which reset
// SPICEDB_INSECURE to the bundled plaintext default. So keeping means
// re-supplying the endpoint (which resolveExternalSpiceDBFromState collects on a
// TTY, or --external-spicedb-endpoint supplies non-interactively), and this
// refusal is the fail-closed alternative to a silent flip for every other path.
func refuseExternalToBundledFlip(d DetectedSettings, ext ExternalSpiceDB) error {
	if d.ExternalSpiceDB && !ext.enabled() {
		return fmt.Errorf("this cluster's SpiceDB is external (%s), but the install resolved to the bundled in-cluster SpiceDB — refusing to overwrite the endpoint config and silently orphan the external instance and every relationship in it. Re-run with --external-spicedb-endpoint=%s [--external-spicedb-token=…] to keep pointing at it, or `oap init` without --wizard/--accept-existing to switch backends deliberately", d.ExternalSpiceDBEndpoint, d.ExternalSpiceDBEndpoint)
	}
	return nil
}

// routingRunOptions returns base with its driver reframed onto the single
// Routing rail slot (flagTrustedHostname). The three routing screens carry
// distinct IDs but share one rail step; without the reframe, sandbox-hostname
// and acme-email resolve to StepIndex == -1 and blank the whole rail while they
// present. tui.Reframe returns a non-chrome (Plain/fail-closed) driver
// unchanged, so this is a no-op on those drivers — including the Plain driver
// the accept-all tests drive the run over.
func routingRunOptions(base tui.Options, driver tui.Driver, chrome *tui.Chrome) tui.Options {
	base.Driver = tui.Reframe(driver, chrome, flagTrustedHostname)
	return base
}

// nonFakeIdPKinds returns the registered identity-provider kinds minus "fake",
// mirroring the loop in runInitIdpOffer (init.go).
func nonFakeIdPKinds() []string {
	var kinds []string
	for _, name := range idpregistry.Names() {
		if name == "fake" {
			continue
		}
		kinds = append(kinds, name)
	}
	return kinds
}
