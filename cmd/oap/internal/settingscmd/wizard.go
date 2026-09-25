// `oap settings wizard` — the re-runnable cluster security-defaults wizard.
//
// The wizard describes its screens and reads the chosen Selections back out of
// the answered State; everything around that is the lifecycle `oap channel
// create` uses:
//
//	detect terminal capabilities → theme → run the screens → read the result
//	→ resolve the model token → compose → preview → apply
//
// Questions and the run's summary are presented over the command's error
// stream while the ClusterAgentSettings preview goes to its output stream. An
// operator redirecting the preview to a file must still see what they are being
// asked, and must still get a file holding nothing but the manifest.
package settingscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/modeltoken"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingswizard"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/models"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/promptinjection"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard/kinds/urlallowlist"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// The cluster default model's fallback provider, token source and Secret
// coordinates. Named once so the --default-model* flag defaults, the
// interactive catalog screen's pre-filled fields and buildCatalogStepOpts's
// fallbacks converge on the same central Secret instead of repeating three
// string literals that can drift.
const (
	defaultModelProvider        = "anthropic"
	defaultModelTokenEnv        = "ANTHROPIC_API_KEY"
	defaultModelSecretName      = "model-default-token"
	defaultModelSecretNamespace = "agentprimitives-system"
	defaultModelSecretKey       = "token"
)

// DefaultModelOpts carries the --default-model* flag values (shared by
// `oap settings wizard` and `oap init` via RegisterDefaultModelFlags). An empty
// Model means "no default model" — the flow is skipped entirely.
type DefaultModelOpts struct {
	Model, Provider, TokenEnv, TokenFile   string
	SecretName, SecretNamespace, SecretKey string
}

func (o DefaultModelOpts) enabled() bool { return strings.TrimSpace(o.Model) != "" }

// RegisterDefaultModelFlags registers the seven --default-model* flags on cmd
// and returns the bound *DefaultModelOpts. Both `oap settings wizard`
// (newSettingsWizardCmd, below) and `oap init` call this same helper so the
// flag names/defaults/help text can't drift between the two commands.
func RegisterDefaultModelFlags(cmd *cobra.Command) *DefaultModelOpts {
	dm := &DefaultModelOpts{}
	cmd.Flags().StringVar(&dm.Model, "default-model", "", "Register a cluster default model (catalog entry marked default) — e.g. claude-sonnet-5")
	cmd.Flags().StringVar(&dm.Provider, "default-model-provider", defaultModelProvider, "Provider for the default model")
	cmd.Flags().StringVar(&dm.TokenEnv, "default-model-token-env", defaultModelTokenEnv, "Env var holding the model API token (required with a non-anthropic --default-model-provider, unless --default-model-token-file is given)")
	cmd.Flags().StringVar(&dm.TokenFile, "default-model-token-file", "", "File holding the model API token (wins over env)")
	cmd.Flags().StringVar(&dm.SecretName, "default-model-secret-name", defaultModelSecretName, "Central token Secret name")
	cmd.Flags().StringVar(&dm.SecretNamespace, "default-model-secret-namespace", defaultModelSecretNamespace, "Central token Secret namespace")
	cmd.Flags().StringVar(&dm.SecretKey, "default-model-secret-key", defaultModelSecretKey, "Key within the central token Secret")
	return dm
}

// decideCatalogTokenAction is the pure decision at the heart of
// applyDefaultModel's interactive token handling: given whether a default
// catalog entry already existed before this run (existing) and what the user
// entered at the token question (entered — trimmed internally), it decides
// whether to mint/rotate the central Secret.
//
//   - existing=false (first run / brand-new default): a blank entry is an
//     error — a new default requires a token, since there is no prior Secret
//     to fall back to. A non-blank entry mints it.
//   - existing=true (interactive re-run of an already-registered default): a
//     blank entry means "keep the existing Secret" (mint=false — no rotation,
//     and critically no silent consumption of whatever the token env var
//     names). A non-blank entry is an explicit, user-requested rotation
//     (mint=true).
func decideCatalogTokenAction(existing bool, entered string) (mint bool, value string, err error) {
	entered = strings.TrimSpace(entered)
	if entered != "" {
		return true, entered, nil
	}
	if existing {
		return false, "", nil
	}
	return false, "", fmt.Errorf("no model token entered")
}

// modelTokenAsker asks the operator for the model API token.
//
// optional reports that a blank answer is meaningful — "keep the token that is
// already stored" — which is true only on a re-run of an already-registered
// default. A nil asker is what "this run must never prompt" looks like: it is
// the whole non-interactive gate, so there is no second boolean saying the same
// thing that could disagree with it.
type modelTokenAsker func(ctx context.Context, o DefaultModelOpts, optional bool) (string, error)

// applyDefaultModel appends the default catalog entry to sel and, unless
// dryRun, resolves the token value and applies the adoption-labelled central
// Secret. A nil ask never prompts and fails closed when no file/env source
// yields a value.
//
// existingDefault marks an INTERACTIVE re-run of an already-registered
// default (the catalog entry / its Secret existed before the wizard's screens
// ran — see prepopulateSelections and catalogStepResult.existingDefault). On
// that path the token is OPTIONAL and the question is asked FIRST, before any
// file/env lookup: a blank entry keeps the existing Secret untouched (see
// decideCatalogTokenAction) rather than silently consulting the token env var
// and rotating a possibly-different stored token. First-run
// (existingDefault=false) always requires a token, resolved file → $env →
// question via modeltoken.Resolve; o.TokenEnv is $ANTHROPIC_API_KEY only for
// the Anthropic provider, since resolveDefaultModelOpts refuses that default
// paired with any other (see DefaultModelOpts.checkCoherent). The flag-driven
// (--default-model) path always calls this with existingDefault=false — see
// RunWizard.
func applyDefaultModel(ctx context.Context, dyn dynamic.Interface, sel *settingswizard.Selections, o DefaultModelOpts, ask modelTokenAsker, dryRun, existingDefault bool) error {
	sel.ModelCatalog = append(sel.ModelCatalog, settingswizard.ModelEntrySel{
		Name:                 o.Model,
		Provider:             o.Provider,
		Default:              true,
		TokenSecretName:      o.SecretName,
		TokenSecretNamespace: o.SecretNamespace,
		TokenSecretKey:       o.SecretKey,
	})
	if dryRun {
		return nil
	}

	ref := modeltoken.SecretRef{Namespace: o.SecretNamespace, Name: o.SecretName, Key: o.SecretKey}

	if ask != nil && existingDefault {
		entered, err := ask(ctx, o, true)
		if err != nil {
			return err
		}
		mint, value, err := decideCatalogTokenAction(true, entered)
		if err != nil {
			return err
		}
		if !mint {
			return nil
		}
		return modeltoken.EnsureSecret(ctx, dyn, ref, value)
	}

	var prompt func() (string, error)
	if ask != nil {
		prompt = func() (string, error) { return ask(ctx, o, false) }
	}
	value, err := modeltoken.Resolve(o.TokenFile, o.TokenEnv, prompt)
	if err != nil {
		return err
	}
	return modeltoken.EnsureSecret(ctx, dyn, ref, value)
}

// catalogStepResult carries the interactive "Model catalog" answers out of
// runWizardForm. It is intentionally kept separate from
// settingswizard.Selections.ModelCatalog: the Secret only gets minted (and the
// catalog entry only gets appended) once applyDefaultModel runs — which needs
// a dynamic client that doesn't exist yet while the screens are running — so
// the collected values pass through here instead of being written into sel
// directly. inputPerMTok/outputPerMTok are wizard-only fields (the
// --default-model* flags carry no price knobs) applied after the fact; opts
// never carries the token value — that is collected later by
// applyDefaultModel's own question.
type catalogStepResult struct {
	opts                        DefaultModelOpts
	inputPerMTok, outputPerMTok float64
	// existingDefault is true when a default catalog entry (and its Secret)
	// already existed — i.e. was pre-populated by prepopulateSelections —
	// before this interactive wizard run started. selectionsFromState sets it
	// from the initial Selections the run began with, never from a fresh
	// cluster read. applyDefaultModel uses it to make the interactive token
	// question optional on a re-run instead of silently consulting the token
	// env var — see decideCatalogTokenAction.
	existingDefault bool
}

// buildCatalogStepOpts maps the interactive "Model catalog" answers into a
// *catalogStepResult, applying the same secret-name/namespace/key fallbacks as
// RegisterDefaultModelFlags's flag defaults so an interactive run and a
// --default-model run converge on the same central Secret when the user
// clears the advanced fields. Returns nil when name is blank (defensive — the
// catalog screen's own validation already guarantees non-blank, so this path
// is unreachable from the wizard but keeps the mapping safe standalone).
func buildCatalogStepOpts(name, provider, secretName, secretNamespace, secretKey string, inputPerMTok, outputPerMTok float64) *catalogStepResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	o := DefaultModelOpts{
		Model:           name,
		Provider:        provider,
		TokenEnv:        defaultModelTokenEnv,
		SecretName:      strings.TrimSpace(secretName),
		SecretNamespace: strings.TrimSpace(secretNamespace),
		SecretKey:       strings.TrimSpace(secretKey),
	}
	if o.SecretName == "" {
		o.SecretName = defaultModelSecretName
	}
	if o.SecretNamespace == "" {
		o.SecretNamespace = defaultModelSecretNamespace
	}
	if o.SecretKey == "" {
		o.SecretKey = defaultModelSecretKey
	}
	return &catalogStepResult{opts: o, inputPerMTok: inputPerMTok, outputPerMTok: outputPerMTok}
}

// resolveDefaultModelOpts reconciles the --default-model* flags with the
// interactive "Model catalog" step. Exactly one of the two may ever feed
// applyDefaultModel — it is the sole place that appends to sel.ModelCatalog
// and mints the central token Secret, so this is a belt-and-braces guard on
// top of that structural single-writer guarantee. Returns ok=false when
// neither path opted in (dm is zero-value and the catalog control was left
// unchecked). Returns a fail-closed error when BOTH opted in — silently
// preferring one would create a catalog entry / Secret the user didn't expect
// from the path they thought they were using.
func resolveDefaultModelOpts(flagOpts DefaultModelOpts, formStep *catalogStepResult) (DefaultModelOpts, bool, error) {
	var opts DefaultModelOpts
	switch {
	case flagOpts.enabled() && formStep != nil:
		return DefaultModelOpts{}, false, fmt.Errorf(
			"a default model was requested both via --default-model flags and the interactive \"Model catalog\" step; use only one")
	case flagOpts.enabled():
		opts = flagOpts
	case formStep != nil:
		opts = formStep.opts
	default:
		return DefaultModelOpts{}, false, nil
	}
	if err := opts.checkCoherent(); err != nil {
		return DefaultModelOpts{}, false, err
	}
	return opts, true, nil
}

// checkCoherent refuses a (model, provider, token source) combination that
// cannot work, so it fails here rather than at the agent's first turn — the
// entry this produces is marked `default: true`, so every session that does
// not name its own model inherits it.
//
// Two incoherent combinations exist, both reachable from the documented
// flag invocation by substituting a non-Anthropic model into it:
//
//   - A model some OTHER provider's built-in table lists. models.ProviderFor
//     is the single implementation of that attribution question (the desktop
//     setup form asks it too); it stays quiet for an id no table knows, so a
//     model newer than those tables is still accepted as typed.
//   - The token env var still naming the Anthropic key while the provider is a
//     DIFFERENT upstream service. --default-model-token-env defaults to
//     $ANTHROPIC_API_KEY, which for another upstream would resolve an
//     Anthropic key and store it as that provider's token — a credential
//     mix-up no later layer can detect. Scoped to models.Providers() so the
//     "test" harness provider, which fronts no upstream and needs no real key,
//     is not asked to name a var for a credential it never uses. A token FILE
//     (what `oap desktop` passes) wins over the env in modeltoken.Resolve, so
//     it makes the env irrelevant and is exempt; any other explicitly-named
//     var is honored as given.
func (o DefaultModelOpts) checkCoherent() error {
	model := strings.TrimSpace(o.Model)
	if owner, known := models.ProviderFor(model); known && owner != o.Provider {
		return fmt.Errorf(
			"--default-model=%s is served by the %q provider, not %q: pass --default-model-provider=%s, or name a model %s serves",
			model, owner, o.Provider, owner, o.Provider)
	}
	needsOwnKey := o.Provider != defaultModelProvider && slices.Contains(models.Providers(), o.Provider)
	if needsOwnKey && o.TokenFile == "" && o.TokenEnv == defaultModelTokenEnv {
		return fmt.Errorf(
			"--default-model-provider=%s would read its token from $%s, the Anthropic key: pass --default-model-token-env with the var holding your %s key, or --default-model-token-file",
			o.Provider, defaultModelTokenEnv, o.Provider)
	}
	return nil
}

// defaultDetectorImage is the local image the wizard assigns to the
// prompt-injection detector, from the apimage catalog.
var defaultDetectorImage = apimage.Detector.LocalRef()

// detectorImageDefault returns the prompt-injection detector image the wizard
// assigns by default. Remote installs pin by digest when one is known (keyed by
// the detector's image Name), matching the first-party deployment pinning; else
// the registry-qualified :dev tag; else the local pi-detector:dev.
func detectorImageDefault(registry string, digests map[string]string) string {
	if registry == "" {
		return defaultDetectorImage
	}
	if d := digests[apimage.Detector.Name]; d != "" {
		return apimage.Detector.DigestRef(registry, d)
	}
	return apimage.Detector.RegistryRef(registry)
}

func newSettingsWizardCmd(g *apcmd.Globals) *cobra.Command {
	var defaults bool
	var wizard bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "wizard",
		Short: "Interactively configure cluster-wide agent security settings.",
		Long: `wizard walks you through enabling security controls (circuit breakers,
rate limits, data-volume budgets, dependency pinning, prompt-injection detection,
and URL allow-listing) and applies the resulting ClusterAgentSettings to your cluster.

Use --defaults for a non-interactive secure baseline, or --dry-run to preview
without applying.`,
	}
	dm := RegisterDefaultModelFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// --defaults wins over --wizard; if neither is set, interactive is implied.
		return RunWizard(cmd.Context(), cmd, g, defaults, dryRun, "" /*no registry*/, nil /*no build digests in standalone path*/, *dm)
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "Apply the recommended secure baseline without prompting (non-interactive)")
	cmd.Flags().BoolVar(&wizard, "wizard", false, "Force interactive wizard (default when neither --defaults nor --wizard is specified)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview the resulting ClusterAgentSettings without applying it")
	return cmd
}

// Presentation is the terminal a wizard run is presented over.
//
// The questions and the summary go to a stream separate from the one the
// ClusterAgentSettings preview is written to, because the preview is
// machine-readable output an operator may be redirecting to a file: sharing one
// stream would either hide the questions or shred the manifest.
type Presentation struct {
	in    io.Reader
	out   io.Writer
	theme *tui.Theme

	// driver presents every group of every run this command makes.
	//
	// It is built ONCE and shared, because this command asks its questions in
	// two runs — the wizard's screens, then the model token — over one stdin,
	// and the line-oriented driver buffers the stream it reads. A second driver
	// over that same stdin would start a second buffer over bytes the first had
	// already pulled in, and those bytes are then unreachable: the token
	// question would see EOF and take the answer a blank line stands for, which
	// on a re-run means "keep the stored token" — silently declining the
	// rotation the input asked for, and exiting 0.
	//
	// That is the same over-read tui's own line reader prevents WITHIN a run.
	// One driver is what extends it across the two.
	driver tui.Driver
}

// NewPresentation builds a Presentation over an explicit terminal. The command
// builds its own from the cobra streams; this is for the cross-command test
// that reads every oap surface's Inline answer side by side.
func NewPresentation(theme *tui.Theme, in io.Reader, out io.Writer) *Presentation {
	return &Presentation{theme: theme, in: in, out: out}
}

// useDriver settles which driver every run of this command presents over.
//
// steps are the whole command's screens, the token question included, so the
// rail is the same from the first question to the last — resolved by screen ID,
// which is what lets one chrome frame two runs.
func (p *Presentation) useDriver(steps []tui.Step) {
	// An injected/shared driver (RunWizardWithDriver, from oap init's unified
	// wizard) already frames these screens on the outer rail. Leave it in place:
	// building a fresh one here would open a second chrome that had never heard
	// of the phases before Settings, exactly the multi-driver bug directorycmd's
	// TestRunWizard_BuildsOneDriverSharedAcrossPhases guards against.
	if p.driver != nil {
		return
	}
	p.driver = tui.DriverFor(tui.DriverParams{
		Theme:  p.theme,
		Chrome: tui.NewChrome(settingsWizardTitle, steps, p.theme),
		In:     p.in,
		Out:    p.out,
	})
}

// runOptions is how every run of this command is presented.
//
// NOT inline: every answer this command collects can be given from what the
// form itself shows.
//
// Output IS written between two runs of this driver — runWizardForm renders a
// summary, and the model-token question then presents over the same driver. But
// the rule turns on whether an answer DEPENDS on that output, not on whether
// output exists: the token is read off the provider's console, not off the
// summary above it. So taking the screen costs the operator nothing, and the
// summary is still in scrollback when the run ends.
// (See tui.Options.Inline for the rule.)
func (p *Presentation) RunOptions() tui.Options {
	return tui.Options{
		Theme:  p.theme,
		Title:  settingsWizardTitle,
		In:     p.in,
		Out:    p.out,
		Driver: p.driver,
	}
}

// present drives screens over the shared driver. A nil driver leaves the choice
// to the sequencer, which is the right answer for a caller that never went
// through useDriver.
func (p *Presentation) present(ctx context.Context, screens []tui.Screen, st *tui.State) (*tui.State, error) {
	return tui.RunWith(ctx, screens, p.RunOptions(), st)
}

// RunWizard is the testable core: --defaults skips every question; the
// interactive path runs the wizard's screens. Both paths compose → preview →
// optionally apply. dm carries the optional --default-model* flow (see
// applyDefaultModel); a zero-value dm is a no-op (dm.enabled() is false).
//
// It builds its own presentation driver from the cobra streams. A caller that
// needs the wizard's screens to present under an already-built driver/chrome —
// `oap init`'s unified wizard, which draws one rail across all its phases — uses
// RunWizardWithDriver instead.
func RunWizard(ctx context.Context, cmd *cobra.Command, g *apcmd.Globals, defaults, dryRun bool, registry string, digests map[string]string, dm DefaultModelOpts) error {
	return RunWizardWithDriver(ctx, cmd, g, defaults, dryRun, registry, digests, dm, nil)
}

// RunWizardWithDriver is RunWizard with an injectable presentation driver.
//
// A nil driver reproduces RunWizard exactly: the wizard builds its own from the
// cobra streams (see useDriver). A non-nil driver — passed by `oap init`'s
// unified wizard, reframed to the settings rail step — is used for every screen
// this command presents, so the settings questions join the same chrome/rail as
// the rest of init rather than opening a second, disconnected one. useDriver
// then leaves the injected driver in place instead of building a fresh one.
func RunWizardWithDriver(ctx context.Context, cmd *cobra.Command, g *apcmd.Globals, defaults, dryRun bool, registry string, digests map[string]string, dm DefaultModelOpts, driver tui.Driver) error {
	out := cmd.OutOrStdout()

	// Capabilities come from the stream the questions are written to rather
	// than from os.Stdout: taking over a screen this command is not actually
	// writing to would leave the wizard invisible, and a test or a shell
	// pipeline supplies a plain io.Writer. driver is the caller's shared driver
	// (nil for the standalone command, which builds its own in useDriver).
	pres := &Presentation{in: cmd.InOrStdin(), out: cmd.ErrOrStderr(), driver: driver}
	pres.theme = g.Theme(pres.out)

	var sel settingswizard.Selections
	var catalogStep *catalogStepResult
	if defaults {
		// --defaults re-asserts the recommended secure baseline, but a re-run —
		// a desktop VM bring-up, or `oap settings wizard --defaults` against an
		// already-configured cluster — must NOT drop the Cluster-tab edits the
		// wizard has no screen for. Compose emits an atomic modelCatalog under a
		// forced apply, so a baseline-only compose would delete every extra
		// catalog entry, the pinning ceiling and the content inspectors. Fold the
		// stored settings in first (the same round-trip the interactive path
		// does), then overlay the baseline controls on top.
		b, berr := g.Bundle()
		if berr != nil {
			return fmt.Errorf("connect to cluster: %w", berr)
		}
		existing, lerr := settingswizard.LoadExisting(ctx, b.Controller)
		if lerr != nil {
			return fmt.Errorf("load existing ClusterAgentSettings: %w", lerr)
		}
		merged, merr := mergeDefaultsWithExisting(existing, registry, digests, dm)
		if merr != nil {
			return fmt.Errorf("merge existing settings into baseline: %w", merr)
		}
		sel = merged
	} else {
		// Load existing to pre-populate the screens' defaults.
		b, err := g.Bundle()
		if err != nil {
			return fmt.Errorf("connect to cluster: %w", err)
		}
		existing, lerr := settingswizard.LoadExisting(ctx, b.Controller)
		if lerr != nil {
			return fmt.Errorf("load existing ClusterAgentSettings: %w", lerr)
		}
		initial, perr := prepopulateSelections(existing, registry, digests)
		if perr != nil {
			return fmt.Errorf("pre-populate wizard from existing settings: %w", perr)
		}
		var formErr error
		sel, catalogStep, formErr = runWizardForm(ctx, pres, initial, registry, digests)
		if formErr != nil {
			if errors.Is(formErr, huh.ErrUserAborted) {
				fmt.Fprintln(out, wizardCancelledMessage)
				return nil
			}
			// Returned as the screens wrote it: runWizardForm has already taken
			// this package's framing off, and re-wrapping it here would put a
			// stage name back in front of a sentence that already reads.
			return formErr
		}
	}

	// At most one of the --default-model flags / interactive "Model catalog"
	// step may request a default model — see resolveDefaultModelOpts.
	modelOpts, applyModel, err := resolveDefaultModelOpts(dm, catalogStep)
	if err != nil {
		return err
	}

	// existingDefault is set only when modelOpts came from the interactive
	// "Model catalog" step (catalogStep) AND the run started from a
	// pre-populated existing default (see catalogStepResult.existingDefault).
	// The flag-driven (--default-model) path and a brand-new interactive
	// default both leave it false, so applyDefaultModel keeps requiring a token
	// on those paths — see applyDefaultModel's doc.
	existingDefault := catalogStep != nil && catalogStep.existingDefault

	// applyDefaultModel needs a dynamic client to mint the central token Secret
	// BEFORE Compose builds the catalog-entry-bearing ClusterAgentSettings, so
	// build it early (and reuse it for Apply below) whenever a default model
	// was requested. --defaults implies non-interactive, so it gets a nil asker
	// and never prompts. dry-run never reaches the client (applyDefaultModel
	// returns before using dyn when dryRun is set), so skip the build entirely
	// in that case.
	var dyn dynamic.Interface
	if applyModel {
		if !dryRun {
			dyn, err = DynamicFactory(g)
			if err != nil {
				return fmt.Errorf("build dynamic client: %w", err)
			}
		}
		var ask modelTokenAsker
		if !defaults {
			ask = newModelTokenAsker(pres)
		}
		if err := applyDefaultModel(ctx, dyn, &sel, modelOpts, ask, dryRun, existingDefault); err != nil {
			// The token question is the last thing this command asks, and it is
			// asked BEFORE the Secret is minted and before the settings are
			// composed or applied — so a Ctrl+C here has changed nothing, and
			// saying so is worth more than a non-zero exit and "canceled".
			// tui.UserFacing keeps the chain, so the sentinel still matches
			// through the token asker and modeltoken.Resolve.
			if errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintln(out, wizardCancelledMessage)
				return nil
			}
			return err
		}
		// Prices are a wizard-only field (the --default-model flags carry no
		// price knobs) — patch them onto the entry applyDefaultModel just
		// appended, now that it exists.
		if catalogStep != nil && (catalogStep.inputPerMTok > 0 || catalogStep.outputPerMTok > 0) {
			last := &sel.ModelCatalog[len(sel.ModelCatalog)-1]
			last.InputPerMTok = catalogStep.inputPerMTok
			last.OutputPerMTok = catalogStep.outputPerMTok
		}
	}

	cas := settingswizard.Compose(sel)

	// Print a YAML preview so the user can inspect before applying.
	previewBytes, err := yaml.Marshal(cas)
	if err != nil {
		return fmt.Errorf("marshal ClusterAgentSettings for preview: %w", err)
	}
	fmt.Fprintln(out, "── ClusterAgentSettings preview ─────────────────────────────")
	fmt.Fprint(out, string(previewBytes))
	fmt.Fprintln(out, "─────────────────────────────────────────────────────────────")

	if dryRun {
		fmt.Fprintln(out, "(dry-run: no changes applied)")
		return nil
	}

	if dyn == nil {
		dyn, err = DynamicFactory(g)
		if err != nil {
			return fmt.Errorf("build dynamic client: %w", err)
		}
	}
	if err := settingswizard.Apply(ctx, dyn, cas); err != nil {
		return fmt.Errorf("apply ClusterAgentSettings: %w", err)
	}
	fmt.Fprintf(out, "applied ClusterAgentSettings/%s\n", cas.Name)
	return nil
}

// mergeDefaultsWithExisting folds an already-stored ClusterAgentSettings into
// the recommended baseline the --defaults path applies, so a re-run re-asserts
// the secure controls WITHOUT dropping the Cluster-tab edits the wizard has no
// screen for. It reuses prepopulateSelections — the interactive path's own
// round-trip, which already carries the pinning ceiling, content inspectors,
// model-override / native-file-handling grants and the catalog entries verbatim
// — then overlays what --defaults MEANS: turn the recommended
// breaker/rate/data/pinning controls on regardless of what was stored.
//
// Two things need care beyond that overlay:
//
//   - toolGuard thresholds are not expressible as the on/off booleans
//     prepopulate derives, so a stored toolGuard policy is carried through
//     VERBATIM (Selections.PreservedToolGuard) rather than rebuilt from the
//     booleans, which would reset a customized failure threshold or byte budget
//     to the baseline. A fresh cluster with no stored toolGuard falls back to
//     the baseline the overlaid booleans compose.
//   - the model catalog: applyDefaultModel APPENDS the fresh default after this
//     returns whenever dm names one. prepopulateSelections puts the stored
//     default into ModelCatalog[0]; when a new default will be written that
//     stored default is cleared out of ModelCatalog so it does not become a
//     SECOND default — dropped when it shares the new name (it is being
//     rewritten), else demoted into PreservedModelCatalog as a non-default
//     entry. When no default will be written, the stored default stays as-is.
func mergeDefaultsWithExisting(existing *v1alpha1.ClusterAgentSettings, registry string, digests map[string]string, dm DefaultModelOpts) (settingswizard.Selections, error) {
	sel, err := prepopulateSelections(existing, registry, digests)
	if err != nil {
		return settingswizard.Selections{}, err
	}

	// Overlay the recommended baseline controls on top of the preserved state.
	sel.Breaker = true
	sel.RateLimit = true
	sel.DataLimit = true
	sel.Pinning = true

	// Preserve a stored toolGuard policy verbatim so custom thresholds survive
	// the re-run; only the on/off booleans above are round-trippable otherwise.
	if existing != nil && existing.Spec.Defaults != nil && existing.Spec.Defaults.ToolGuard != nil {
		sel.PreservedToolGuard = existing.Spec.Defaults.ToolGuard
	}

	if dm.enabled() {
		newDefault := strings.TrimSpace(dm.Model)
		for _, e := range sel.ModelCatalog {
			if e.Name == newDefault {
				continue // applyDefaultModel will rewrite this entry as the default
			}
			e.Default = false
			sel.PreservedModelCatalog = append(sel.PreservedModelCatalog, e)
		}
		sel.ModelCatalog = nil
	}
	return sel, nil
}

// prepopulateSelections maps an existing ClusterAgentSettings back to a
// Selections so the interactive screens start from current state. When the CAS
// has no spec the function returns BaselineSelections() so the run starts with
// the recommended secure defaults selected. It returns an error when a stored
// content-inspector's config fails to decode, rather than silently resetting
// that inspector's detail fields to defaults.
func prepopulateSelections(cas *v1alpha1.ClusterAgentSettings, registry string, digests map[string]string) (settingswizard.Selections, error) {
	// "No existing config" means every top-level spec section is unset — NOT
	// just Defaults. A CAS with only content inspectors or only a pinning
	// ceiling (no tool-guard rules) still has real config to round-trip;
	// checking Defaults alone would fall through to the baseline and silently
	// drop that config on re-run.
	if cas == nil || (cas.Spec.Defaults == nil && cas.Spec.Limits == nil && cas.Spec.ModelCatalog == nil) {
		// No existing config: start with the recommended baseline pre-selected.
		return settingswizard.BaselineSelections(), nil
	}

	sel := settingswizard.Selections{}

	// Infer which tool-guard knobs are active from the catch-all rule.
	if cas.Spec.Defaults != nil && cas.Spec.Defaults.ToolGuard != nil {
		for _, r := range cas.Spec.Defaults.ToolGuard.Rules {
			if r.Breaker != nil {
				sel.Breaker = true
			}
			if r.RateLimit != nil {
				sel.RateLimit = true
			}
			if r.DataLimit != nil {
				sel.DataLimit = true
			}
		}
	}

	// Pinning is active when the limits.pinning ceiling has any rules, and the
	// stored rules are carried verbatim: the wizard has no minStrength/mode
	// screen, so recomposing them from the checkbox alone would rewrite an
	// operator's ceiling with the baseline. See buildPinning.
	if cas.Spec.Limits != nil && cas.Spec.Limits.Pinning != nil && len(cas.Spec.Limits.Pinning.Rules) > 0 {
		sel.Pinning = true
		for _, r := range cas.Spec.Limits.Pinning.Rules {
			sel.PinningRules = append(sel.PinningRules, settingswizard.PinningRuleSel{
				Kind: r.Kind, MinStrength: r.MinStrength, Mode: r.Mode,
			})
		}
	}

	// Content inspectors: decode the stored typed config so detail fields
	// (threshold/action, allowlist domains) round-trip instead of resetting.
	if cas.Spec.Limits != nil && cas.Spec.Limits.ContentInspectors != nil {
		for _, ci := range *cas.Spec.Limits.ContentInspectors {
			switch ci.ID {
			case "prompt-injection":
				var cfg promptinjection.Config
				if err := json.Unmarshal(ci.Config.Raw, &cfg); err != nil {
					return settingswizard.Selections{}, fmt.Errorf("decode prompt-injection inspector config: %w", err)
				}
				piSel := &settingswizard.PromptInjectionSel{
					DetectorImage: cfg.DetectorImage,
					Action:        cfg.Action,
				}
				if piSel.DetectorImage == "" {
					piSel.DetectorImage = detectorImageDefault(registry, digests)
				}
				if cfg.Threshold != nil {
					piSel.Threshold = *cfg.Threshold
				} else {
					piSel.Threshold = defaultInjectionThreshold
				}
				if piSel.Action == "" {
					piSel.Action = injectionActionApprove
				}
				sel.PromptInjection = piSel
			case "url-allowlist":
				var cfg urlallowlist.Config
				if err := json.Unmarshal(ci.Config.Raw, &cfg); err != nil {
					return settingswizard.Selections{}, fmt.Errorf("decode url-allowlist inspector config: %w", err)
				}
				urlSel := &settingswizard.URLAllowlistSel{DefaultAction: cfg.DefaultAction}
				if urlSel.DefaultAction == "" {
					urlSel.DefaultAction = urlActionDeny
				}
				for _, r := range cfg.Rules {
					if r.Domain != "" {
						urlSel.Rules = append(urlSel.Rules, settingswizard.URLRuleSel{Domain: r.Domain, Action: r.Action})
					}
				}
				sel.URLAllowlist = urlSel
			}
		}
	}

	// AllowModelOverride round-trips independently of whether a catalog entry
	// is present, so re-running the wizard doesn't silently reset a
	// previously-granted override back to false.
	if cas.Spec.Limits != nil && cas.Spec.Limits.AllowModelOverride != nil {
		sel.AllowModelOverride = *cas.Spec.Limits.AllowModelOverride
	}

	// NativeFileHandling likewise round-trips passively: the wizard has no
	// interactive toggle for this Tier-2 grant, so preserving an out-of-band
	// grant here is the only thing that keeps a re-run from resetting it.
	if cas.Spec.Limits != nil && cas.Spec.Limits.NativeFileHandling != nil {
		sel.NativeFileHandling = *cas.Spec.Limits.NativeFileHandling
	}

	// buildModelEntrySel builds a ModelEntrySel from a catalog entry. forceDefault
	// is used by the no-explicit-default fallback below, which always marks the
	// first entry as the default so the run has exactly one.
	buildModelEntrySel := func(e v1alpha1.ModelCatalogEntry, forceDefault bool) settingswizard.ModelEntrySel {
		entry := settingswizard.ModelEntrySel{
			Name:          e.Name,
			Provider:      e.Provider,
			Default:       e.Default || forceDefault,
			InputPerMTok:  e.InputPerMTok,
			OutputPerMTok: e.OutputPerMTok,
		}
		if e.TokenRef != nil {
			entry.TokenSecretName = e.TokenRef.Name
			entry.TokenSecretNamespace = e.TokenRef.Namespace
			entry.TokenSecretKey = e.TokenRef.Key
		}
		return entry
	}

	// Model catalog. The wizard edits exactly ONE entry — the default — so
	// sel.ModelCatalog is pre-populated with that entry alone and the wizard's
	// screens read [0] from it (see prepopulateFields). Every other entry goes
	// to PreservedModelCatalog instead: the wizard never shows them, but
	// ModelCatalog on the CR is an atomic list under a forced apply, so
	// dropping them here deletes them from the cluster. See Compose.
	if cas.Spec.ModelCatalog != nil {
		defaultIdx := -1
		for i, e := range *cas.Spec.ModelCatalog {
			if e.Default {
				defaultIdx = i
				break
			}
		}
		// No entry claims the default: adopt the first, so the run still has
		// exactly one default (buildModelEntrySel's forceDefault).
		if defaultIdx < 0 && len(*cas.Spec.ModelCatalog) > 0 {
			defaultIdx = 0
		}
		for i, e := range *cas.Spec.ModelCatalog {
			if i == defaultIdx {
				sel.ModelCatalog = []settingswizard.ModelEntrySel{buildModelEntrySel(e, true)}
				continue
			}
			// Force Default=false: only the adopted entry may carry it, or a
			// re-run would compose a catalog with two defaults.
			preserved := buildModelEntrySel(e, false)
			preserved.Default = false
			sel.PreservedModelCatalog = append(sel.PreservedModelCatalog, preserved)
		}
	}

	return sel, nil
}

// settingsWizardTitle is the chrome's title bar for a wizard run. One constant
// rather than a literal per run, so the screens and the token question cannot
// title themselves differently mid-wizard.
const settingsWizardTitle = "oap · settings wizard"

// wizardCancelledMessage is what a Ctrl+C reads as. Named once because both
// places a cancellation can arrive — the wizard's screens and the token
// question — are before anything is written, so both owe the reader the same
// reassurance.
const wizardCancelledMessage = "Wizard cancelled — no changes applied."

// The State keys the wizard's answers land under. They are the vocabulary a
// caller would seed to answer a screen ahead of time, so they are named in the
// user's terms rather than after the CRD fields they end up in.
const (
	keyControls         = "controls"
	keyDetectorImage    = "detector-image"
	keyDetectorAction   = "detector-action"
	keyThreshold        = "detection-threshold"
	keyAllowedDomains   = "allowed-domains"
	keyUnlistedAction   = "unlisted-url-action"
	keyModelName        = "model"
	keyModelProvider    = "model-provider"
	keyModelSecretName  = "model-secret-name"
	keyModelSecretNS    = "model-secret-namespace"
	keyModelSecretKey   = "model-secret-key"
	keyModelInputPrice  = "model-input-price"
	keyModelOutputPrice = "model-output-price"
	keyModelToken       = "model-token"
)

// The values the controls checklist records, and the branch every later screen
// reads to decide whether it applies to this run.
const (
	guardBreaker   = "breaker"
	guardRateLimit = "ratelimit"
	guardDataLimit = "datalimit"
	guardPinning   = "pinning"
	guardInjection = "pi"
	guardURLList   = "urllist"
	guardCatalog   = "catalog"
)

// The answers the two choice questions accept, and the value each is pre-set
// to. Named so the rows, the validation of a pre-supplied answer, and
// prepopulateSelections's fallbacks cannot drift apart.
const (
	injectionActionApprove = "approve"
	injectionActionBlock   = "block"
	urlActionDeny          = "deny"
	urlActionApprove       = "approve"

	// defaultInjectionThreshold is the classifier confidence cut-off a run
	// starts from when nothing is stored.
	defaultInjectionThreshold = 0.75
)

var (
	injectionActions = []string{injectionActionApprove, injectionActionBlock}
	urlActions       = []string{urlActionDeny, urlActionApprove}
)

// guardOption is one row of the controls checklist: the value it records, the
// sentence the user reads, and the short name the summary lists it under.
type guardOption struct{ value, label, short string }

// guardOptions is the checklist, in the order it is offered.
//
// It is the single list the rows, the pre-selection, the answer validation and
// the summary all derive from — deriving rather than transcribing is what keeps
// a control added here from being offered but never applied, or applied but
// never named in the summary.
var guardOptions = []guardOption{
	{guardBreaker, "Circuit breaker (deny tool calls after repeated failures)", "circuit breaker"},
	{guardRateLimit, "Rate limit (cap tool calls per turn)", "rate limit"},
	{guardDataLimit, "Data-volume budget (cap egress/ingress bytes)", "data-volume budget"},
	{guardPinning, "Dependency pinning (warn on unpinned images/MCPs/skills)", "dependency pinning"},
	{guardInjection, "Prompt-injection detector (classifier sidecar)", "prompt-injection detector"},
	{guardURLList, "URL allow-list (gate outbound URLs)", "URL allow-list"},
	{guardCatalog, "Model catalog (a default model and its API token)", "model catalog"},
}

// settingsScreens is the wizard, in order. initial is what the run starts
// from — an existing ClusterAgentSettings mapped back by prepopulateSelections,
// or the recommended baseline when there is none.
func settingsScreens(initial settingswizard.Selections, registry string, digests map[string]string) []tui.Screen {
	return []tui.Screen{
		&controlsScreen{preselected: preselectedGuards(initial)},
		&injectionScreen{initial: initial.PromptInjection, imageDefault: detectorImageDefault(registry, digests)},
		&urlAllowlistScreen{initial: initial.URLAllowlist},
		&catalogScreen{initial: firstCatalogEntry(initial)},
	}
}

// settingsSteps is the rail every question of this command is framed by: the
// wizard's screens plus the model token, which is presented as its own run.
//
// The token step is listed whether or not it will be reached, exactly as the
// three conditional screens before it are — the rail is derived from what the
// command CAN ask, and a step that skips is already the norm here. Listing it
// is what lets the token question render inside the same rail, at its own
// position, instead of alone under a one-step chrome that discards where the
// operator had got to.
func settingsSteps(screens []tui.Screen) []tui.Step {
	return append(tui.Steps(screens), tui.Step{ID: tokenScreenID, Label: tokenScreenLabel})
}

// preselectedGuards maps the Selections a run starts from onto the checklist
// rows that begin ticked, in offer order.
func preselectedGuards(initial settingswizard.Selections) []string {
	on := map[string]bool{
		guardBreaker:   initial.Breaker,
		guardRateLimit: initial.RateLimit,
		guardDataLimit: initial.DataLimit,
		guardPinning:   initial.Pinning,
		guardInjection: initial.PromptInjection != nil,
		guardURLList:   initial.URLAllowlist != nil,
		guardCatalog:   len(initial.ModelCatalog) > 0,
	}
	out := make([]string, 0, len(guardOptions))
	for _, o := range guardOptions {
		if on[o.value] {
			out = append(out, o.value)
		}
	}
	return out
}

// firstCatalogEntry returns the default catalog entry a run starts from, or nil
// when there is none. prepopulateSelections stores at most one.
func firstCatalogEntry(initial settingswizard.Selections) *settingswizard.ModelEntrySel {
	if len(initial.ModelCatalog) == 0 {
		return nil
	}
	e := initial.ModelCatalog[0]
	return &e
}

// guardChosen reports whether the controls checklist ticked value. Every later
// screen branches on this rather than on a field of its own, so "which controls
// are on" has exactly one answer for the whole run.
func guardChosen(st *tui.State, value string) bool {
	return slices.Contains(st.All(keyControls), value)
}

// controlsScreen is the checklist every other screen branches on.
type controlsScreen struct {
	preselected []string

	// chosen is bound to the rows: the pre-selection on the way in, the
	// operator's answer on the way out.
	chosen []string
}

func (*controlsScreen) ID() string { return "controls" }

func (*controlsScreen) Label() string { return "Controls" }

// AnswerKeys declares the State key this screen asks for. Duck-typed by callers
// that map a missing answer back to what would have supplied it.
func (*controlsScreen) AnswerKeys() []string { return []string{keyControls} }

func (s *controlsScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	// State first: a choice already recorded is not re-asked. Apply still runs,
	// because it is what validates that choice and records it for the screens
	// that branch on it.
	if st.Has(keyControls) {
		return nil, nil
	}
	opts := make([]huh.Option[string], 0, len(guardOptions))
	for _, o := range guardOptions {
		opts = append(opts, huh.NewOption(o.label, o.value))
	}
	s.chosen = slices.Clone(s.preselected)
	return huh.NewGroup(
		huh.NewMultiSelect[string]().
			Key(keyControls).
			Title("Security controls to enable").
			Description("The recommended baseline is pre-selected.").
			Options(opts...).
			Value(&s.chosen),
	), nil
}

func (s *controlsScreen) Apply(_ context.Context, st *tui.State) error {
	answer := s.chosen
	if st.Has(keyControls) {
		// A pre-supplied answer never went through the rows, so it is checked
		// here on the same terms as a ticked one.
		answer = st.All(keyControls)
	}
	chosen, err := canonicalGuards(answer)
	if err != nil {
		return err
	}
	// Recorded even when empty, so a run that turned everything off is
	// distinguishable from one where this screen never ran.
	st.SetAll(keyControls, chosen)
	st.Note("Controls", guardSummary(chosen))
	return nil
}

// canonicalGuards reduces an answer to the controls it names, in the order they
// were offered.
//
// The membership check is what earns its place: an unrecognised value branches
// nothing and applies nothing, so it would silently leave a control off with
// nothing reported. Ticking a row cannot produce one — the rows ARE the
// options — so this guards a caller that seeds an answer instead.
func canonicalGuards(answer []string) ([]string, error) {
	offered := make(map[string]bool, len(guardOptions))
	// Named for the reader, not keyed for us: the refusal below is the one place
	// this list is read by a person, and the internal values it is matching
	// against mean nothing to them.
	names := make([]string, 0, len(guardOptions))
	for _, o := range guardOptions {
		offered[o.value] = true
		names = append(names, o.short)
	}

	want := make(map[string]bool, len(answer))
	for _, v := range answer {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !offered[v] {
			return nil, fmt.Errorf("%q is not one of the security controls this wizard offers; it offers: %s",
				v, strings.Join(names, ", "))
		}
		want[v] = true
	}

	out := make([]string, 0, len(want))
	for _, o := range guardOptions {
		if want[o.value] {
			out = append(out, o.value)
		}
	}
	return out, nil
}

// guardSummary names the enabled controls for the post-run summary. "none" is
// spelled out rather than left blank: an empty value in a summary reads as a
// rendering fault rather than as the answer it is.
func guardSummary(chosen []string) string {
	shorts := make([]string, 0, len(chosen))
	for _, o := range guardOptions {
		if slices.Contains(chosen, o.value) {
			shorts = append(shorts, o.short)
		}
	}
	if len(shorts) == 0 {
		return "none"
	}
	return strings.Join(shorts, ", ")
}

// field is one line of a multi-question screen: what it was pre-filled with,
// and what the operator left in it.
//
// The two are kept apart because huh hands a validator the line as TYPED, not
// the bound value. A bare Enter on a pre-filled field arrives as "", which is
// how a user accepts what is already there — so a required-value check applied
// to it directly would reject that Enter, and the line-oriented renderer
// re-prompts on a rejection by reading again, silently consuming the NEXT
// answer. The pre-fill is also what a cleared field and an exhausted input
// stream both fall back to, which is why Apply resolves through here rather
// than reading the bound value alone.
type field struct{ dflt, value string }

// reset pre-fills the field. Called from Prepare, which must build a fresh
// group — and therefore a fresh binding — on every call.
func (f *field) reset(dflt string) { f.dflt, f.value = dflt, dflt }

// answer resolves what this field holds when an empty answer means "keep what
// is already there": a pre-supplied answer wins, then what was typed, then the
// pre-fill.
func (f *field) answer(st *tui.State, key string) string {
	if st.Has(key) {
		return strings.TrimSpace(st.Get(key))
	}
	if v := strings.TrimSpace(f.value); v != "" {
		return v
	}
	return strings.TrimSpace(f.dflt)
}

// clearableAnswer resolves what this field holds when an empty answer means
// EMPTY: a pre-supplied answer wins, otherwise whatever the field was left
// holding — with no fall back to the pre-fill.
//
// It is for the two optional prices, and the difference is not cosmetic.
// Falling back there makes the field ONE-WAY: an operator who blanks out a
// stale list price to drop it is handed the stale price straight back, so a
// price can be added and never removed. A required field has no such reading —
// there, an empty answer can only mean "keep it".
func (f *field) clearableAnswer(st *tui.State, key string) string {
	if st.Has(key) {
		return strings.TrimSpace(st.Get(key))
	}
	return strings.TrimSpace(f.value)
}

// typed wraps check so a bare Enter on a pre-filled field is accepted as
// "keep it" rather than re-prompted.
func (f *field) typed(check func(string) error) func(string) error {
	return func(in string) error {
		if strings.TrimSpace(in) == "" && strings.TrimSpace(f.dflt) != "" {
			return nil
		}
		return check(in)
	}
}

// injectionScreen configures the prompt-injection detector, and runs only when
// the controls checklist asked for it.
type injectionScreen struct {
	initial      *settingswizard.PromptInjectionSel
	imageDefault string

	image, threshold, action field
}

func (*injectionScreen) ID() string { return "injection" }

func (*injectionScreen) Label() string { return "Injection" }

// AnswerKeys declares the State keys this screen asks for. Duck-typed by
// callers that map a missing answer back to what would have supplied it.
func (*injectionScreen) AnswerKeys() []string {
	return []string{keyDetectorImage, keyThreshold, keyDetectorAction}
}

func (s *injectionScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if !guardChosen(st, guardInjection) {
		// Not part of this run at all: neither asked nor applied, so a control
		// left unticked cannot leave a half-configured inspector behind.
		return nil, tui.ErrSkip
	}
	s.image.reset(s.imageDefault)
	s.threshold.reset(fmt.Sprintf("%g", defaultInjectionThreshold))
	s.action.reset(injectionActionApprove)
	if s.initial != nil {
		if s.initial.DetectorImage != "" {
			s.image.reset(s.initial.DetectorImage)
		}
		s.threshold.reset(fmt.Sprintf("%g", s.initial.Threshold))
		if s.initial.Action != "" {
			s.action.reset(s.initial.Action)
		}
	}

	// State first, per field: only the questions still unanswered are asked, and
	// a screen with nothing left to ask presents nothing while still applying.
	fields := make([]huh.Field, 0, 3)
	if !st.Has(keyDetectorImage) {
		fields = append(fields, huh.NewInput().
			Key(keyDetectorImage).
			Title("Prompt-injection detector image").
			Description("Image for the zero-egress classifier sidecar.").
			Value(&s.image.value).
			Validate(s.image.typed(nonEmpty("detector image"))))
	}
	if !st.Has(keyThreshold) {
		fields = append(fields, huh.NewInput().
			Key(keyThreshold).
			Title("Detection threshold, between 0 and 1").
			Value(&s.threshold.value).
			Validate(s.threshold.typed(validateThreshold)))
	}
	if !st.Has(keyDetectorAction) {
		fields = append(fields, huh.NewSelect[string]().
			Key(keyDetectorAction).
			Title("Action on a detected injection").
			Options(
				huh.NewOption("approve - route to a human approver", injectionActionApprove),
				huh.NewOption("block - deny immediately", injectionActionBlock),
			).
			Value(&s.action.value))
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return huh.NewGroup(fields...), nil
}

func (s *injectionScreen) Apply(_ context.Context, st *tui.State) error {
	image := s.image.answer(st, keyDetectorImage)
	if err := nonEmpty("detector image")(image); err != nil {
		return err
	}
	threshold := s.threshold.answer(st, keyThreshold)
	// Re-checked rather than trusted: huh's accessible renderer keeps the last
	// REJECTED value bound when its input runs out mid-field, so a value
	// reaching here has not necessarily passed the field's own validator — and
	// a pre-supplied answer never saw it at all.
	if err := validateThreshold(threshold); err != nil {
		return err
	}
	action := s.action.answer(st, keyDetectorAction)
	if err := oneOf("action on a detected injection", injectionActions)(action); err != nil {
		return err
	}

	st.Set(keyDetectorImage, image)
	st.Set(keyThreshold, threshold)
	st.Set(keyDetectorAction, action)
	st.Note("Prompt-injection detector", fmt.Sprintf("%s, threshold %s, %s", image, threshold, action))
	return nil
}

// urlAllowlistScreen configures the outbound-URL allow-list, and runs only when
// the controls checklist asked for it.
type urlAllowlistScreen struct {
	initial *settingswizard.URLAllowlistSel

	domains, defaultAction field
}

func (*urlAllowlistScreen) ID() string { return "urls" }

func (*urlAllowlistScreen) Label() string { return "URLs" }

// AnswerKeys declares the State keys this screen asks for. Duck-typed by
// callers that map a missing answer back to what would have supplied it.
func (*urlAllowlistScreen) AnswerKeys() []string {
	return []string{keyAllowedDomains, keyUnlistedAction}
}

func (s *urlAllowlistScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if !guardChosen(st, guardURLList) {
		return nil, tui.ErrSkip
	}
	s.domains.reset("")
	s.defaultAction.reset(urlActionDeny)
	if s.initial != nil {
		parts := make([]string, 0, len(s.initial.Rules))
		for _, r := range s.initial.Rules {
			parts = append(parts, r.Domain)
		}
		s.domains.reset(strings.Join(parts, ","))
		if s.initial.DefaultAction != "" {
			s.defaultAction.reset(s.initial.DefaultAction)
		}
	}

	fields := make([]huh.Field, 0, 2)
	if !st.Has(keyAllowedDomains) {
		// The separator rule lives in the TITLE, not the description: an INPUT's
		// accessible rendering prompts with its title alone, so its description
		// is invisible off-TTY — exactly where the user has the least context.
		// (A note is the exception; its static description does get printed.)
		fields = append(fields, huh.NewInput().
			Key(keyAllowedDomains).
			Title("Allowed domains, separated by commas").
			Value(&s.domains.value).
			Validate(s.domains.typed(validateDomains)))
	}
	if !st.Has(keyUnlistedAction) {
		fields = append(fields, huh.NewSelect[string]().
			Key(keyUnlistedAction).
			Title("Action for an unlisted URL").
			Options(
				huh.NewOption("deny - block it", urlActionDeny),
				huh.NewOption("approve - route it to a human approver", urlActionApprove),
			).
			Value(&s.defaultAction.value))
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return huh.NewGroup(fields...), nil
}

func (s *urlAllowlistScreen) Apply(_ context.Context, st *tui.State) error {
	domains := s.domains.answer(st, keyAllowedDomains)
	if err := validateDomains(domains); err != nil {
		return err
	}
	action := s.defaultAction.answer(st, keyUnlistedAction)
	if err := oneOf("action for an unlisted URL", urlActions)(action); err != nil {
		return err
	}

	st.Set(keyAllowedDomains, domains)
	st.Set(keyUnlistedAction, action)
	parsed := parseURLDomains(domains)
	st.Note("URL allow-list", fmt.Sprintf("%s (unlisted: %s)", strings.Join(parsed, ", "), action))
	return nil
}

// catalogGuidance is the block shown above the catalog fields: why no token
// field appears among them.
//
// It is a note FIELD rather than an input's description because an INPUT's
// accessible rendering prompts with its title alone, so its description is
// invisible off-TTY. (A note is the exception; huh's accessible renderer does
// print a note's static description — which is why this is a note.) Its lines
// are kept inside the note's column budget; TestWizardNotesFitTheNoteWidth
// measures that against the same Chrome the form is sized with, because huh
// wraps an over-long note line with no indication that the second half belongs
// to the first.
const catalogGuidance = "The API token is asked for separately, right before\n" +
	"it is saved. It is never shown here and never kept\n" +
	"in this form."

// catalogScreen registers the cluster default model, and runs only when the
// controls checklist asked for it. It deliberately collects no token — see
// catalogGuidance.
type catalogScreen struct {
	initial *settingswizard.ModelEntrySel

	name, provider                  field
	secretName, secretNS, secretKey field
	inputPrice, outputPrice         field
}

func (*catalogScreen) ID() string { return "model" }

func (*catalogScreen) Label() string { return "Model" }

// AnswerKeys declares the State keys this screen asks for. Duck-typed by
// callers that map a missing answer back to what would have supplied it.
func (*catalogScreen) AnswerKeys() []string {
	return []string{
		keyModelName, keyModelProvider,
		keyModelSecretName, keyModelSecretNS, keyModelSecretKey,
		keyModelInputPrice, keyModelOutputPrice,
	}
}

func (s *catalogScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if !guardChosen(st, guardCatalog) {
		return nil, tui.ErrSkip
	}
	s.name.reset("")
	s.provider.reset(defaultModelProvider)
	s.secretName.reset(defaultModelSecretName)
	s.secretNS.reset(defaultModelSecretNamespace)
	s.secretKey.reset(defaultModelSecretKey)
	s.inputPrice.reset("")
	s.outputPrice.reset("")
	if e := s.initial; e != nil {
		s.name.reset(e.Name)
		if e.Provider != "" {
			s.provider.reset(e.Provider)
		}
		if e.TokenSecretName != "" {
			s.secretName.reset(e.TokenSecretName)
		}
		if e.TokenSecretNamespace != "" {
			s.secretNS.reset(e.TokenSecretNamespace)
		}
		if e.TokenSecretKey != "" {
			s.secretKey.reset(e.TokenSecretKey)
		}
		if e.InputPerMTok > 0 {
			s.inputPrice.reset(fmt.Sprintf("%g", e.InputPerMTok))
		}
		if e.OutputPerMTok > 0 {
			s.outputPrice.reset(fmt.Sprintf("%g", e.OutputPerMTok))
		}
	}

	fields := make([]huh.Field, 0, 8)
	if !st.Has(keyModelName) {
		fields = append(fields, huh.NewInput().
			Key(keyModelName).
			Title("Model name").
			Description("Identifier agents ask for, e.g. claude-opus-4-8.").
			Value(&s.name.value).
			Validate(s.name.typed(nonEmpty("model name"))))
	}
	if !st.Has(keyModelProvider) {
		fields = append(fields, huh.NewSelect[string]().
			Key(keyModelProvider).
			Title("Provider").
			Options(
				huh.NewOption(defaultModelProvider, defaultModelProvider),
				huh.NewOption("test (harness only — not for production)", "test"),
			).
			Value(&s.provider.value))
	}
	if !st.Has(keyModelSecretName) {
		fields = append(fields, huh.NewInput().
			Key(keyModelSecretName).
			Title("Name for the stored API token").
			Value(&s.secretName.value).
			Validate(s.secretName.typed(nonEmpty("token name"))))
	}
	if !st.Has(keyModelSecretNS) {
		fields = append(fields, huh.NewInput().
			Key(keyModelSecretNS).
			Title("Namespace to store the token in").
			Value(&s.secretNS.value).
			Validate(s.secretNS.typed(nonEmpty("token namespace"))))
	}
	if !st.Has(keyModelSecretKey) {
		fields = append(fields, huh.NewInput().
			Key(keyModelSecretKey).
			Title("Key the token is stored under").
			Value(&s.secretKey.value).
			Validate(s.secretKey.typed(nonEmpty("token key"))))
	}
	if !st.Has(keyModelInputPrice) {
		fields = append(fields, huh.NewInput().
			Key(keyModelInputPrice).
			Title("Input price per million tokens, USD (optional)").
			Value(&s.inputPrice.value).
			Validate(validateOptionalNonNegFloat))
	}
	if !st.Has(keyModelOutputPrice) {
		fields = append(fields, huh.NewInput().
			Key(keyModelOutputPrice).
			Title("Output price per million tokens, USD (optional)").
			Value(&s.outputPrice.value).
			Validate(validateOptionalNonNegFloat))
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return huh.NewGroup(append([]huh.Field{huh.NewNote().Title(catalogGuidance)}, fields...)...), nil
}

func (s *catalogScreen) Apply(_ context.Context, st *tui.State) error {
	name := s.name.answer(st, keyModelName)
	if err := nonEmpty("model name")(name); err != nil {
		return err
	}
	provider := s.provider.answer(st, keyModelProvider)
	if err := nonEmpty("provider")(provider); err != nil {
		return err
	}
	secretName := s.secretName.answer(st, keyModelSecretName)
	if err := nonEmpty("token name")(secretName); err != nil {
		return err
	}
	secretNS := s.secretNS.answer(st, keyModelSecretNS)
	if err := nonEmpty("token namespace")(secretNS); err != nil {
		return err
	}
	secretKey := s.secretKey.answer(st, keyModelSecretKey)
	if err := nonEmpty("token key")(secretKey); err != nil {
		return err
	}
	inputPrice := s.inputPrice.clearableAnswer(st, keyModelInputPrice)
	if err := validateOptionalNonNegFloat(inputPrice); err != nil {
		return err
	}
	outputPrice := s.outputPrice.clearableAnswer(st, keyModelOutputPrice)
	if err := validateOptionalNonNegFloat(outputPrice); err != nil {
		return err
	}

	st.Set(keyModelName, name)
	st.Set(keyModelProvider, provider)
	st.Set(keyModelSecretName, secretName)
	st.Set(keyModelSecretNS, secretNS)
	st.Set(keyModelSecretKey, secretKey)
	st.Set(keyModelInputPrice, inputPrice)
	st.Set(keyModelOutputPrice, outputPrice)
	st.Note("Default model", fmt.Sprintf("%s (%s)", name, provider))
	return nil
}

// modelTokenScreen collects the model API token. It is presented on its own,
// after the wizard's screens, because it is asked only once the token's
// destination is settled and only when no file or environment variable already
// supplied one.
type modelTokenScreen struct {
	model, provider string

	// optional accepts a blank answer, which means "keep the token already
	// stored". True only on a re-run of an already-registered default.
	optional bool

	value string
}

// The token screen names itself through constants because settingsSteps has to
// place it in the rail without holding an instance of it — the two must agree,
// or the driver would frame the question as a step it has never heard of.
const (
	tokenScreenID    = "token"
	tokenScreenLabel = "Token"
)

func (*modelTokenScreen) ID() string { return tokenScreenID }

func (*modelTokenScreen) Label() string { return tokenScreenLabel }

// AnswerKeys declares the State key this screen asks for. Duck-typed by callers
// that map a missing answer back to what would have supplied it.
func (*modelTokenScreen) AnswerKeys() []string { return []string{keyModelToken} }

// tokenKeptGuidance is what a re-run reads above the field: that leaving it
// blank changes nothing. Its lines are kept inside the note's column budget —
// see catalogGuidance.
const tokenKeptGuidance = "A token for this model is already stored.\n" +
	"Leave this blank to keep it, or type a new one\n" +
	"to replace it."

func (s *modelTokenScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	// State first: a token already recorded is not re-asked. Apply still runs,
	// because it is what carries the value back out to the caller.
	if st.Has(keyModelToken) {
		return nil, nil
	}
	fields := make([]huh.Field, 0, 2)
	if s.optional {
		fields = append(fields, huh.NewNote().Title(tokenKeptGuidance))
	}
	// No EchoModePassword. That mode needs a reader with a terminal file
	// descriptor, and huh's accessible renderer discards the error when there is
	// none: off-TTY the field would ask nothing, accept nothing and report
	// nothing, leaving an empty credential behind.
	fields = append(fields, huh.NewInput().
		Key(keyModelToken).
		Title(fmt.Sprintf("API token for %s (%s)", s.model, s.provider)).
		Value(&s.value).
		Validate(s.validateTyped))
	return huh.NewGroup(fields...), nil
}

// validateTyped rejects an empty line as it is typed on a run that requires a
// token, so the operator is re-prompted rather than told after the fact. On a
// re-run an empty line is the answer that keeps the stored token.
func (s *modelTokenScreen) validateTyped(in string) error {
	if s.optional || strings.TrimSpace(in) != "" {
		return nil
	}
	return s.missing()
}

func (s *modelTokenScreen) Apply(_ context.Context, st *tui.State) error {
	v := strings.TrimSpace(s.value)
	if st.Has(keyModelToken) {
		v = strings.TrimSpace(st.Get(keyModelToken))
	}
	if v == "" && !s.optional {
		return s.missing()
	}
	// Recorded in State but never noted: State lives as long as this one
	// question, while a summary line is written into the operator's scrollback,
	// where a token would outlive the terminal it was pasted into.
	st.Set(keyModelToken, v)
	return nil
}

// missing is the refusal for a token that never arrived. It names the model
// rather than the State key, and repeats nothing the operator typed — this
// screen collects a credential, and an error is one of the places a credential
// must never appear.
func (s *modelTokenScreen) missing() error {
	return fmt.Errorf("no API token was supplied for %s", s.model)
}

// newModelTokenAsker returns the question applyDefaultModel asks when nothing
// else supplied a token. Presented as its own run rather than as a screen of
// the wizard because it is reached only after the wizard's answers have been
// resolved against the --default-model flags, and only when the file and
// environment sources came up empty.
func newModelTokenAsker(pres *Presentation) modelTokenAsker {
	return func(ctx context.Context, o DefaultModelOpts, optional bool) (string, error) {
		screens := []tui.Screen{&modelTokenScreen{model: o.Model, provider: o.Provider, optional: optional}}
		answered, err := pres.present(ctx, screens, tui.NewState())
		if err != nil {
			// Stripped here, at the call site that produced the framing: `tui:
			// apply screen "token":` in front of a sentence about a model the
			// operator named is this command's plumbing showing through, and the
			// screen ID names nothing they can act on.
			return "", tui.UserFacing(err)
		}
		return answered.Get(keyModelToken), nil
	}
}

// runWizardForm presents the wizard's screens and reads the chosen Selections
// out of the answered State, plus the "Model catalog" answers when that control
// was ticked (nil otherwise — see catalogStepResult for why that isn't folded
// into Selections here). Returns an error wrapping huh.ErrUserAborted when the
// user presses Ctrl-C.
func runWizardForm(ctx context.Context, pres *Presentation, initial settingswizard.Selections, registry string, digests map[string]string) (settingswizard.Selections, *catalogStepResult, error) {
	screens := settingsScreens(initial, registry, digests)
	// Settled here, before the first question, because the token question that
	// may follow this run has to be presented over the SAME driver — see
	// Presentation.driver.
	pres.useDriver(settingsSteps(screens))

	answered, err := pres.present(ctx, screens, tui.NewState())
	if err != nil {
		return initial, nil, tui.UserFacing(err)
	}
	sel, step, err := selectionsFromState(initial, answered)
	if err != nil {
		return initial, nil, err
	}
	// The summary is the record that survives the alt-screen being released, so
	// it is written to the same stream the run used. It goes out BEFORE anything
	// is applied, so failing on it costs the user nothing they cannot re-run.
	if err := tui.RenderSummary(pres.out, pres.theme, answered.Notes()); err != nil {
		return initial, nil, err
	}
	return sel, step, nil
}

// selectionsFromState derives what a run decided from its answers.
//
// Splitting this out of the screens is what makes the whole wizard's outcome
// testable from a State alone, with no driver and no terminal: the screens
// collect and validate, this maps.
func selectionsFromState(initial settingswizard.Selections, st *tui.State) (settingswizard.Selections, *catalogStepResult, error) {
	sel := initial
	sel.Breaker = guardChosen(st, guardBreaker)
	sel.RateLimit = guardChosen(st, guardRateLimit)
	sel.DataLimit = guardChosen(st, guardDataLimit)
	sel.Pinning = guardChosen(st, guardPinning)

	sel.PromptInjection = nil
	if guardChosen(st, guardInjection) {
		// The screen's Apply already validated this; a parse failure here would
		// mean the answer changed shape between the two, which is a defect
		// rather than a bad answer — so it is surfaced, not defaulted away.
		threshold, err := strconv.ParseFloat(st.Get(keyThreshold), 64)
		if err != nil {
			return settingswizard.Selections{}, nil, fmt.Errorf("detection threshold %q is not a number: %w", st.Get(keyThreshold), err)
		}
		sel.PromptInjection = &settingswizard.PromptInjectionSel{
			DetectorImage: st.Get(keyDetectorImage),
			Threshold:     threshold,
			Action:        st.Get(keyDetectorAction),
		}
	}

	sel.URLAllowlist = nil
	if guardChosen(st, guardURLList) {
		domains := parseURLDomains(st.Get(keyAllowedDomains))
		rules := make([]settingswizard.URLRuleSel, 0, len(domains))
		for _, d := range domains {
			rules = append(rules, settingswizard.URLRuleSel{Domain: d, Action: "allow"})
		}
		sel.URLAllowlist = &settingswizard.URLAllowlistSel{
			Rules:         rules,
			DefaultAction: st.Get(keyUnlistedAction),
		}
	}

	// The catalog entry itself is NOT written into sel here — sel.ModelCatalog
	// is populated exactly once, later, by applyDefaultModel (which also mints
	// the token Secret and asks for the token value). Writing it here too would
	// risk a second default:true entry if the --default-model flags are ALSO
	// set; see resolveDefaultModelOpts.
	sel.ModelCatalog = nil
	var step *catalogStepResult
	if guardChosen(st, guardCatalog) {
		inputPrice, err := parseOptionalPrice(st.Get(keyModelInputPrice))
		if err != nil {
			return settingswizard.Selections{}, nil, fmt.Errorf("input price: %w", err)
		}
		outputPrice, err := parseOptionalPrice(st.Get(keyModelOutputPrice))
		if err != nil {
			return settingswizard.Selections{}, nil, fmt.Errorf("output price: %w", err)
		}
		step = buildCatalogStepOpts(st.Get(keyModelName), st.Get(keyModelProvider),
			st.Get(keyModelSecretName), st.Get(keyModelSecretNS), st.Get(keyModelSecretKey),
			inputPrice, outputPrice)
		if step != nil {
			// A default entry existed at the start of the run iff the run was
			// pre-populated with one (see prepopulateSelections) — that's the
			// only signal used, deliberately not a fresh cluster read.
			step.existingDefault = len(initial.ModelCatalog) > 0
		}
	}
	return sel, step, nil
}

// nonEmpty builds the validator for a required line, naming the question in the
// user's words rather than by its State key, which is ours.
func nonEmpty(what string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s must not be empty", what)
		}
		return nil
	}
}

// oneOf builds the validator for a choice question. It runs on the ANSWER as
// well as on the rows: a pre-supplied answer never went through the rows, and
// an unrecognised one would be stored verbatim and rejected only much later, by
// the cluster.
func oneOf(what string, allowed []string) func(string) error {
	return func(s string) error {
		if slices.Contains(allowed, strings.TrimSpace(s)) {
			return nil
		}
		return fmt.Errorf("%s must be one of: %s", what, strings.Join(allowed, ", "))
	}
}

// validateThreshold rejects a classifier cut-off that is not a number in [0,1].
func validateThreshold(s string) error {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fmt.Errorf("threshold must be a number: %v", err)
	}
	if v < 0 || v > 1 {
		return fmt.Errorf("threshold must be between 0 and 1, got %g", v)
	}
	return nil
}

// validateDomains rejects an allow-list that names nothing, which would deny
// every outbound URL while reading like a configured list.
func validateDomains(s string) error {
	if len(parseURLDomains(s)) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	return nil
}

// validateOptionalNonNegFloat validates an optional numeric answer: blank is
// valid (means "no price"); a non-blank value must parse as a non-negative
// float.
func validateOptionalNonNegFloat(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("must be a number: %v", err)
	}
	if v < 0 {
		return fmt.Errorf("must not be negative, got %g", v)
	}
	return nil
}

// parseOptionalPrice turns an optional price answer into its value, with blank
// meaning "no price" (0) rather than a parse failure.
func parseOptionalPrice(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return v, nil
}

// parseURLDomains splits a comma- or newline-separated domain list into
// non-empty trimmed domain strings.
func parseURLDomains(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == ','
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
