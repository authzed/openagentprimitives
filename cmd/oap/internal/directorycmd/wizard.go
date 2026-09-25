package directorycmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// wizardTitle is the chrome's title bar for an interactive run.
const wizardTitle = "oap · directory configure"

// The tui.State keys the wizard's screens answer under. "kind" has no
// per-kind namespace collision to worry about: relsync kind names are
// registered under a flat, human-chosen string (github, onepassword,
// slack), and a kind's own ConfigScreens keys (e.g. github's "orgs") are
// defined by that kind, not by this file — a future kind choosing "kind",
// "credential" or "endpoint" as one of its own screen keys would collide,
// but that is the kind author's contract to respect, the same way a CRD
// field name must not shadow a sibling's.
const (
	keyKind       = "kind"
	keyCredential = "credential"
	keyEndpoint   = "endpoint"
)

// keyConfiguration is the rail step every one of the chosen kind's own
// ConfigScreens is presented under — see wizardChrome. It is deliberately
// not any relsync.ConfigScreen.Key a real kind defines (github's is
// "orgs"), so the rail step and a kind's own tui.State key can never
// collide the way keyKind/keyCredential/keyEndpoint could (see the const
// block above).
const keyConfiguration = "configuration"

// Selections is the wizard's normalized output — what the operator chose.
// The huh TUI and any non-interactive path both produce one.
type Selections struct {
	Kind       string
	Identity   string
	Credential string
	Endpoint   string
	Config     json.RawMessage
	Name       string // the CR name; reused on a re-run, derived on a first run
}

// Deps are RunWizard's dependencies on cluster state, factored as functions
// rather than a raw dynamic.Interface + namespace: existing.go and
// credentials.go already own the dynamic-client fixtures for ExistingFor and
// AvailableCredentials (existing_test.go, credentials_test.go), so a wizard
// test substitutes a fake here directly instead of rebuilding those
// fixtures a second time.
type Deps struct {
	// Existing returns what is already configured for kind — Task 3's
	// ExistingFor, bound to a client and namespace.
	Existing func(ctx context.Context, kind string) (relsync.ExistingConfig, error)
	// Credentials lists every credential a source in this namespace could
	// use — Task 4's AvailableCredentials, bound the same way.
	Credentials func(ctx context.Context) ([]CredentialRef, error)
	// StoreCredential persists a credential the chosen kind's setup flow just
	// minted, creating the AgentIdentity when it does not exist.
	//
	// Nil means this run cannot create one, and the wizard then offers only
	// credentials that already exist — a real state (a Deps built by hand in a
	// test, a caller with no typed client) rather than a failure, so it changes
	// what is OFFERED instead of producing an error nobody can act on.
	StoreCredential func(ctx context.Context, cred NewCredential, value builtins.StoreValue) error
}

// NewDeps builds Deps against a real cluster: ExistingFor and
// AvailableCredentials over dyn, the credential write over c, all bound to ns.
// The production entry point (the `oap directory configure` command) calls
// this; a test builds a Deps literal with fakes instead.
//
// c is a controller-runtime client because that is what setup.Store takes —
// the same chokepoint `oap identity setup` writes every credential through, so
// a credential created here is the same shape as one created there rather than
// a second implementation that drifts from it. A nil c leaves StoreCredential
// nil rather than producing a closure that panics on first use; see that
// field's own doc for what the wizard then offers.
func NewDeps(dyn dynamic.Interface, c client.Client, ns string) Deps {
	d := Deps{
		Existing: func(ctx context.Context, kind string) (relsync.ExistingConfig, error) {
			return ExistingFor(ctx, dyn, ns, kind)
		},
		Credentials: func(ctx context.Context) ([]CredentialRef, error) {
			return AvailableCredentials(ctx, dyn, ns)
		},
	}
	if c != nil {
		d.StoreCredential = func(ctx context.Context, cred NewCredential, v builtins.StoreValue) error {
			return setup.Store(ctx, c, setup.StoreRequest{
				Namespace:    ns,
				IdentityName: cred.Identity,
				Requirement: authkind.CredentialRequirement{
					SuggestedName: cred.Name,
					ProviderID:    cred.ProviderID,
				},
				Value:     v,
				SubjectID: cred.SubjectID,
			})
		}
	}
	return d
}

// Presentation is the terminal a wizard run is presented over — mirrors
// cmd/oap/internal/settingscmd's Presentation. RunWizard resolves one driver
// from these three fields (via driverForRun) and shares it across every
// phase — see wizardChrome — the same reason settingscmd pins one driver
// across its own two runs: the line-oriented driver buffers the stream it
// reads, so a second independently-resolved driver over the same stdin
// would lose every answer after the first.
type Presentation struct {
	In    io.Reader
	Out   io.Writer
	Theme *tui.Theme
}

// NewPresentation builds a Presentation over an explicit terminal.
func NewPresentation(theme *tui.Theme, in io.Reader, out io.Writer) *Presentation {
	return &Presentation{Theme: theme, In: in, Out: out}
}

// WizardOpts configures one run of RunWizard.
type WizardOpts struct {
	// Pres is the terminal this run prompts over. Nil is fine for a fully
	// non-interactive run: RunWizard falls back to an uncolored default
	// theme (tui.RunWith requires one) and leaves In/Out unset, which every
	// driver this run could resolve to either ignores (NonInteractive) or
	// treats as "read/write the process's own stdio".
	Pres *Presentation
	// Answers pre-seeds tui.State before any screen runs — how a caller
	// (flags, a scripted test) supplies answers ahead of time. Keyed by the
	// tui.State key each screen reads: keyKind, keyCredential, keyEndpoint,
	// and — for the chosen kind's own questions — whatever
	// relsync.ConfigScreen.Key that kind defines (e.g. github's "orgs").
	Answers map[string]string
	// NonInteractive is the --non-interactive flag: fail closed at the
	// first screen whose answer Answers did not supply, rather than
	// prompting for it.
	NonInteractive bool
}

func (o WizardOpts) theme() *tui.Theme {
	if o.Pres != nil && o.Pres.Theme != nil {
		return o.Pres.Theme
	}
	return tui.NewTheme(tui.Caps{})
}

func (o WizardOpts) in() io.Reader {
	if o.Pres != nil {
		return o.Pres.In
	}
	return nil
}

func (o WizardOpts) out() io.Writer {
	if o.Pres != nil && o.Pres.Out != nil {
		return o.Pres.Out
	}
	return io.Discard
}

// wizardChrome is the ONE frame every phase of RunWizard presents under: a
// fixed rail of four units — Kind, Credential, Endpoint, Configuration —
// regardless of which kind is chosen or how many questions its own
// Configurable asks.
//
// Kind, Credential and Endpoint are each exactly one screen whose own ID
// already equals its rail step (keyKind, keyCredential, keyEndpoint), so
// those phases present directly over the shared driver with no reframing —
// each screen's own ID resolves against this same chrome. Configuration is
// different: it fronts however many screens the CHOSEN KIND's own
// ConfigScreens returns, under whatever keys that kind defines (github's is
// "orgs") — ids this chrome has never heard of — so RunWizard pins every one
// of them to this one step with tui.Reframe instead.
//
// Built ONCE per run and reused (see driverForRun), rather than once per
// phase, is what fixes a real bug: three independently-resolved drivers,
// each from a Chrome built only from that phase's own screens, rendered
// three unrelated rails that shared one stdin buffer but forgot each
// other's progress — phase two's rail had never heard of "Kind", so it
// could not mark it done. That read to an operator as the wizard
// restarting mid-flow, not continuing. See pkg/cli/tui/reframe.go and
// cmd/oap/internal/agentcmd/install_channels.go's channelsChrome, which
// fixed the identical bug shape for `oap agent install`'s channel wiring —
// a caller with coarse, statically-known units of work (there: one per
// declared channel; here: these four) fronting a kind's dynamically-shaped
// sub-screens.
func wizardChrome(theme *tui.Theme) *tui.Chrome {
	return tui.NewChrome(wizardTitle, []tui.Step{
		{ID: keyKind, Label: "Kind"},
		{ID: keyCredential, Label: "Credential"},
		{ID: keyEndpoint, Label: "Endpoint"},
		{ID: keyConfiguration, Label: "Configuration"},
	}, theme)
}

// driverForRun resolves the driver wizardChrome's rail is drawn through.
//
// A package var — not a direct call to tui.DriverFor inside RunWizard —
// solely so a same-package test can substitute a spy and prove RunWizard
// resolves this ONCE per run and reuses (directly, or via tui.Reframe) it
// across every phase, rather than leaving Options.Driver nil per call and
// letting each phase's tui.RunWith resolve its own. See
// TestRunWizard_BuildsOneDriverSharedAcrossPhases.
var driverForRun = tui.DriverFor

// RunWizard walks an operator through configuring one directory-sync
// source: which kind, which credential, its endpoint (when the kind needs
// one), and whatever else that kind's own Configurable asks. Selecting no
// kind is a first-class, non-error answer — RunWizard returns (nil, nil).
//
// The run is kind-agnostic by construction: everything past kind selection
// is driven off relsync.Get(kind) and, where the kind implements
// relsync.Configurable, that kind's own ConfigScreens/BuildConfig — this
// file never branches on a kind's name.
//
// Config is always BuildConfig's freshly-computed output, never
// prior.Config echoed back — relsync.ExistingConfig's own doc names this as
// the reason a re-run is safe despite the dynamic client reordering object
// keys. A re-run that changes nothing still re-derives Config from the
// prefilled (or explicitly re-typed) answers, so the byte-identical case is
// byte-identical because BuildConfig is deterministic, not because the old
// bytes were carried through untouched.
func RunWizard(ctx context.Context, deps Deps, opts WizardOpts) (*Selections, error) {
	st := tui.NewState()
	for k, v := range opts.Answers {
		st.Set(k, v)
	}

	theme := opts.theme()
	chrome := wizardChrome(theme)
	driver := driverForRun(tui.DriverParams{
		Theme:          theme,
		Chrome:         chrome,
		In:             opts.in(),
		Out:            opts.out(),
		NonInteractive: opts.NonInteractive,
	})
	runOpts := tui.Options{
		Theme:          theme,
		Title:          wizardTitle,
		In:             opts.in(),
		Out:            opts.out(),
		NonInteractive: opts.NonInteractive,
		Driver:         driver,
	}

	// Phase 1: which kind. Nothing past this point can be known until the
	// answer is in hand — the credential screen needs to know what
	// AvailableCredentials found, the endpoint screen needs the chosen
	// kind's NeedsEndpoint, and the config screens are the chosen kind's
	// own. Screens exist so branching and I/O happen BETWEEN groups, in
	// ordinary Go (see pkg/cli/tui/screen.go) — so that I/O (Deps calls)
	// happens here, not inside a screen's Prepare/Apply.
	kScreen := &kindScreen{kinds: relsync.All()}
	st, err := tui.RunWith(ctx, []tui.Screen{kScreen}, runOpts, st)
	if err != nil {
		return nil, err
	}

	kind := strings.TrimSpace(st.Get(keyKind))
	if kind == "" {
		return nil, nil
	}

	k, ok := relsync.Get(kind)
	if !ok {
		// Defensive: kindScreen.Apply already refused any value that is not
		// "" or a registered kind, so a seeded/typed answer cannot reach
		// here unregistered. Kept so this function stays correct standalone
		// if that guard is ever loosened.
		return nil, fmt.Errorf("%q is not a registered directory-sync kind", kind)
	}

	prior, err := deps.Existing(ctx, kind)
	if err != nil {
		return nil, fmt.Errorf("load existing configuration for %q: %w", kind, err)
	}

	creds, err := deps.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("list available credentials: %w", err)
	}
	creds = namedCredentialsOnly(creds)

	setupFlow, setupIntent, setupScopes, err := setupFlowFor(k)
	if err != nil {
		return nil, err
	}
	// Whether a credential can be created ON THIS RUN. All three have to hold:
	// the kind must declare a flow, the run must have somewhere to write the
	// result, and there must be a human to paste a token to — obtaining a
	// credential means answering questions, and --non-interactive exists
	// precisely to promise that nothing will be asked.
	canSetup := setupFlow != "" && deps.StoreCredential != nil && !opts.NonInteractive

	if len(creds) == 0 && !canSetup {
		return nil, noCredentialError(kind, setupFlow, deps.StoreCredential != nil, opts.NonInteractive)
	}

	var configurable relsync.Configurable
	if c, ok := k.(relsync.Configurable); ok {
		configurable = c
	}
	needsEndpoint := configurable != nil && configurable.NeedsEndpoint()

	// Phase 2a: which credential — including, when this run can make one, the
	// option to make one. Its own ID already matches a step wizardChrome
	// declares, so it presents directly over the shared driver.
	st, err = tui.RunWith(ctx, []tui.Screen{
		&credentialScreen{available: creds, prior: prior, canSetup: canSetup},
	}, runOpts, st)
	if err != nil {
		return nil, err
	}

	// Phase 2b: make one, when that is what they chose. Presented over the
	// SAME driver, with its screens (a flow's "browser", "token", …) reframed
	// onto the Credential step — those IDs are not steps this chrome has heard
	// of, so without the reframe the rail would mark every step pending and
	// read to the operator as the wizard restarting mid-flow.
	if st.Get(keyCredential) == setupNewCredential {
		setupOpts := runOpts
		setupOpts.Driver = tui.Reframe(driver, chrome, keyCredential)
		ref, err := runCredentialSetup(ctx, credentialSetupRun{
			kind:     kind,
			flow:     setupFlow,
			intent:   setupIntent,
			scopes:   setupScopes,
			prior:    prior,
			deps:     deps,
			existing: creds,
			runOpts:  setupOpts,
			seeded:   opts.Answers,
		})
		if err != nil {
			return nil, err
		}
		// The rest of the run proceeds exactly as it would have for a
		// credential that already existed — the sentinel never survives this
		// point, so nothing downstream has to know a credential was created.
		st.Set(keyCredential, credentialValue(ref))
	}

	// Phase 2c: endpoint. Same shared driver, same reasoning as 2a.
	st, err = tui.RunWith(ctx, []tui.Screen{
		&endpointScreen{needsEndpoint: needsEndpoint, prior: prior, kind: kind},
	}, runOpts, st)
	if err != nil {
		return nil, err
	}

	// Phase 3: the chosen kind's own questions, if it has any. Their IDs
	// (github's is "orgs") are not steps wizardChrome knows, so every one of
	// them is reframed to the single "Configuration" step instead of each
	// resolving to no rail position at all.
	var configDefs []relsync.ConfigScreen
	if configurable != nil {
		configDefs = configurable.ConfigScreens(prior)
	}
	if len(configDefs) > 0 {
		configScreens := make([]tui.Screen, 0, len(configDefs))
		for i := range configDefs {
			configScreens = append(configScreens, &configFieldScreen{cs: configDefs[i]})
		}
		configOpts := runOpts
		configOpts.Driver = tui.Reframe(driver, chrome, keyConfiguration)
		st, err = tui.RunWith(ctx, configScreens, configOpts, st)
		if err != nil {
			return nil, err
		}
	}

	identity, credential, err := splitCredential(st.Get(keyCredential))
	if err != nil {
		return nil, err
	}

	sel := &Selections{
		Kind:       kind,
		Identity:   identity,
		Credential: credential,
		Endpoint:   strings.TrimSpace(st.Get(keyEndpoint)),
		Name:       sourceName(prior, kind),
	}

	if configurable != nil {
		answers := make(map[string]string, len(configDefs))
		for _, cs := range configDefs {
			answers[cs.Key] = st.Get(cs.Key)
		}
		cfg, err := configurable.BuildConfig(answers)
		if err != nil {
			return nil, err
		}
		sel.Config = cfg
	}

	return sel, nil
}

// setupFlowFor reads the chosen kind's credential-setup declaration, or
// ("", "", nil) for a kind that declares none.
//
// A kind that implements the interface and then names nothing is a contract
// violation rather than a kind with no flow, and is refused here: silently
// treating it as "declares none" would degrade to the old dead end for a kind
// whose whole point was to avoid it, and the operator would be told to go make
// a credential by hand for a kind that shipped a flow to make one.
func setupFlowFor(k relsync.Kind) (flow, intent string, scopes []string, err error) {
	cs, ok := k.(relsync.CredentialSetup)
	if !ok {
		return "", "", nil, nil
	}
	flow, intent = cs.SetupFlow()
	if strings.TrimSpace(flow) == "" {
		return "", "", nil, fmt.Errorf(
			"directory-sync kind %q implements relsync.CredentialSetup but names no flow", k.Name())
	}
	// Nil scopes are legitimate and common: a kind whose flow already derives
	// the right recommendation from its own call surface says so by declaring
	// nothing here, rather than by keeping a second copy of that list.
	return flow, intent, cs.SetupScopes(), nil
}

// noCredentialError is the refusal for a run that has no credential to offer
// and no way to make one. Which of the three reasons applies decides what the
// operator is told to do next, because they are three different situations and
// only one of them is fixed by running a different command.
func noCredentialError(kind, flow string, canStore, nonInteractive bool) error {
	base := fmt.Sprintf("no credential is available in this namespace to configure %q with", kind)
	switch {
	case flow == "":
		return fmt.Errorf("%s, and %q declares no credential setup flow — "+
			"create one first with `oap identity setup`", base, kind)
	case !canStore:
		return fmt.Errorf("%s, and this run has nowhere to write a new one — "+
			"create one first with `oap identity setup`", base)
	case nonInteractive:
		// The one case where re-running the SAME command differently is the
		// fix, so say so rather than sending the operator to another command
		// they do not need.
		return fmt.Errorf("%s. --non-interactive requires a credential that already exists: "+
			"setting one up means pasting a token, which must never come from a flag. "+
			"Re-run without --non-interactive to set one up now, "+
			"or create one first with `oap identity setup`", base)
	default:
		// Unreachable: with a flow, somewhere to write, and a human, canSetup
		// is true and this function is not called. Kept so a future edit to
		// that condition fails loudly here rather than returning nil.
		return fmt.Errorf("%s", base)
	}
}

// namedCredentialsOnly drops a credential whose name is empty.
// AgentCredential.Name carries no MinLength marker, so a CR may legitimately
// have one, and AvailableCredentials returns it verbatim (with an empty
// Credential field) rather than refusing. Selecting it here would write a
// RelationshipSource whose spec.auth.credential names nothing.
func namedCredentialsOnly(refs []CredentialRef) []CredentialRef {
	out := make([]CredentialRef, 0, len(refs))
	for _, r := range refs {
		if r.Credential == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sourceName is the RelationshipSource's name: prior.Name on a re-run — so
// the wizard edits the same CR instead of creating a second one — or, on a
// first run, the kind name itself. The kind name is stable and derived
// solely from the operator's kind choice, never a timestamp or a random
// suffix, so a re-apply of an unchanged answer set stays a byte-identical
// SSA no-op.
func sourceName(prior relsync.ExistingConfig, kind string) string {
	if prior.Name != "" {
		return prior.Name
	}
	return kind
}

// splitCredential splits a credentialScreen answer ("<identity>/<credential>")
// back into its two parts. credentialScreen.Apply validates that the value
// is one of the credentials AvailableCredentials returned, and every one of
// those is joined the same way, so a value reaching here that does not
// split cleanly means the screen's own contract was violated rather than
// the operator having typed something bad.
func splitCredential(v string) (identity, credential string, err error) {
	identity, credential, ok := strings.Cut(v, "/")
	if !ok {
		return "", "", fmt.Errorf("malformed credential answer %q", v)
	}
	return identity, credential, nil
}

// kindScreen asks which relsync kind to configure, offering "none" as a
// real, first-class option — selecting it is an answer, not a cancelled
// run.
type kindScreen struct {
	kinds []relsync.Kind

	chosen string
}

func (*kindScreen) ID() string { return keyKind }

func (*kindScreen) Label() string { return "Kind" }

func (s *kindScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(keyKind) {
		return nil, nil
	}
	opts := make([]huh.Option[string], 0, len(s.kinds)+1)
	opts = append(opts, huh.NewOption("none — configure nothing", ""))
	for _, k := range s.kinds {
		opts = append(opts, huh.NewOption(k.Name(), k.Name()))
	}
	return huh.NewGroup(
		huh.NewSelect[string]().
			Key(keyKind).
			Title("Directory-sync kind to configure").
			Options(opts...).
			Value(&s.chosen),
	), nil
}

func (s *kindScreen) Apply(_ context.Context, st *tui.State) error {
	kind := s.chosen
	if st.Has(keyKind) {
		kind = st.Get(keyKind)
	}
	kind = strings.TrimSpace(kind)
	if kind != "" {
		if _, ok := relsync.Get(kind); !ok {
			return fmt.Errorf("%q is not one of the registered directory-sync kinds", kind)
		}
	}
	st.Set(keyKind, kind)
	if kind == "" {
		st.Note("Kind", "none — nothing configured")
	} else {
		st.Note("Kind", kind)
	}
	return nil
}

// credentialScreen offers every named credential AvailableCredentials
// found, encoded as "<identity>/<credential>" so a single select answer
// carries both halves of Selections.Identity/Selections.Credential — plus,
// when this run can make one, the option to make one.
type credentialScreen struct {
	available []CredentialRef
	prior     relsync.ExistingConfig
	// canSetup reports whether "set up a new credential" is a real option on
	// this run: the kind declares a flow, there is somewhere to write the
	// result, and there is a human to paste a token to. See RunWizard.
	canSetup bool

	chosen string
}

func (*credentialScreen) ID() string { return keyCredential }

func (*credentialScreen) Label() string { return "Credential" }

// credentialValue is the encoded answer one CredentialRef offers/matches.
func credentialValue(r CredentialRef) string { return r.Identity + "/" + r.Credential }

func (s *credentialScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(keyCredential) {
		return nil, nil
	}
	dflt := ""
	if s.prior.Identity != "" && s.prior.Credential != "" {
		dflt = s.prior.Identity + "/" + s.prior.Credential
	}
	if st.NonInteractive() {
		// No explicit answer. A re-run of an already-configured source
		// names a credential already — entering through means accepting
		// it, so seed it as the answer directly rather than presenting
		// (which the fail-closed driver would refuse regardless of
		// whether a default exists). A first run with nothing prior falls
		// through to Apply's own "a credential is required" refusal, since
		// dflt stays "".
		st.Set(keyCredential, dflt)
		return nil, nil
	}
	s.chosen = dflt
	if s.chosen == "" && len(s.available) > 0 {
		s.chosen = credentialValue(s.available[0])
	}
	opts := make([]huh.Option[string], 0, len(s.available)+1)
	for _, r := range s.available {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s (identity %s)", r.Credential, r.Identity), credentialValue(r)))
	}
	if s.canSetup {
		// Offered LAST, and offered always — not only when the list is empty.
		// An operator who has a credential for another kind still routinely
		// wants a fresh one for this one, and burying that behind "delete
		// everything first" is how the dead end this option exists to remove
		// grows back in a narrower form.
		opts = append(opts, huh.NewOption(setupNewCredentialLabel, setupNewCredential))
		if s.chosen == "" {
			// Nothing else to select: with no credentials in the namespace this
			// is the ONLY option, so it is also what the cursor starts on.
			s.chosen = setupNewCredential
		}
	}
	return huh.NewGroup(
		huh.NewSelect[string]().
			Key(keyCredential).
			Title("Credential to sync with").
			Options(opts...).
			Value(&s.chosen),
	), nil
}

func (s *credentialScreen) Apply(_ context.Context, st *tui.State) error {
	v := s.chosen
	if st.Has(keyCredential) {
		v = strings.TrimSpace(st.Get(keyCredential))
	}
	if v == "" {
		if st.NonInteractive() {
			// The old wording — "a credential is required" — is true and
			// useless here: it names a requirement without naming either way
			// to meet it, and the one this run cannot take is the one the
			// operator most needs to know about.
			return fmt.Errorf("a credential is required, and --non-interactive cannot set one up: " +
				"obtaining one means pasting a token, which must never come from a flag or an answer file. " +
				"Name an existing one with --answer credential=<identity>/<name>, " +
				"or re-run without --non-interactive to set one up now")
		}
		return fmt.Errorf("a credential is required")
	}
	if v == setupNewCredential {
		if st.NonInteractive() {
			// Reachable only from --answer credential=<the sentinel>. Refused
			// rather than attempted: the flow behind it asks questions, and a
			// run told not to prompt cannot answer them.
			return fmt.Errorf("--non-interactive cannot set up a new credential: " +
				"obtaining one means pasting a token, which must never come from a flag. " +
				"Name an existing one instead, or re-run without --non-interactive")
		}
		if !s.canSetup {
			// Likewise only reachable from a seeded answer — the option was
			// not offered, so nothing the operator could select produced it.
			return fmt.Errorf("%q cannot set up a new credential on this run", v)
		}
		st.Set(keyCredential, v)
		st.Note("Credential", "setting up a new one")
		return nil
	}
	if !credentialAvailable(s.available, v) {
		return fmt.Errorf("%q is not one of the credentials available in this namespace", v)
	}
	st.Set(keyCredential, v)
	st.Note("Credential", v)
	return nil
}

func credentialAvailable(available []CredentialRef, v string) bool {
	for _, r := range available {
		if credentialValue(r) == v {
			return true
		}
	}
	return false
}

// endpointScreen asks for spec.baseURL. Offered for EVERY kind, not only
// one whose NeedsEndpoint is true: GitHub's own NeedsEndpoint is false
// (github.com needs none), but a GitHub Enterprise Server operator must
// still be able to supply a base URL on a first run — GHES support is the
// entire reason spec.baseURL and the controller's credhost.Check exist.
// Blank means "the vendor default"; blank is refused only when the kind
// needs an endpoint.
type endpointScreen struct {
	needsEndpoint bool
	prior         relsync.ExistingConfig
	kind          string

	value string
}

func (*endpointScreen) ID() string { return keyEndpoint }

func (*endpointScreen) Label() string { return "Endpoint" }

func (s *endpointScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(keyEndpoint) {
		return nil, nil
	}
	if st.NonInteractive() {
		// Blank is a legitimate answer unless this kind requires an
		// endpoint; Apply enforces that, so seed the (possibly blank)
		// default directly rather than presenting.
		st.Set(keyEndpoint, s.prior.Endpoint)
		return nil, nil
	}
	s.value = s.prior.Endpoint
	title := "Endpoint (base URL), blank for the vendor default"
	if s.needsEndpoint {
		title = "Endpoint (base URL) — required, this kind has no vendor default"
	}
	return huh.NewGroup(
		huh.NewInput().
			Key(keyEndpoint).
			Title(title).
			Value(&s.value),
	), nil
}

func (s *endpointScreen) Apply(_ context.Context, st *tui.State) error {
	v := s.value
	if st.Has(keyEndpoint) {
		v = st.Get(keyEndpoint)
	}
	v = strings.TrimSpace(v)
	if v == "" && s.needsEndpoint {
		return fmt.Errorf("%s requires an endpoint (base URL); it has no vendor default", s.kind)
	}
	st.Set(keyEndpoint, v)
	if v != "" {
		st.Note("Endpoint", v)
	}
	return nil
}

// configFieldScreen presents one relsync.ConfigScreen — the chosen kind's
// own question. RunWizard builds one of these per entry
// Configurable.ConfigScreens returned, in order, so a kind that asks for N
// things gets N screens without this file needing to know what any of them
// mean.
type configFieldScreen struct {
	cs relsync.ConfigScreen

	value string
}

func (s *configFieldScreen) ID() string { return s.cs.Key }

func (s *configFieldScreen) Label() string { return s.cs.Prompt }

func (s *configFieldScreen) Prepare(_ context.Context, st *tui.State) (*huh.Group, error) {
	if st.Has(s.cs.Key) {
		return nil, nil
	}
	if st.NonInteractive() {
		// No explicit answer: entering through means keeping whatever this
		// kind prefilled from prior.Config (relsync.ConfigScreen.Default),
		// which is "" on a genuine first run.
		st.Set(s.cs.Key, s.cs.Default)
		return nil, nil
	}
	s.value = s.cs.Default
	input := huh.NewInput().
		Key(s.cs.Key).
		Title(s.cs.Prompt).
		Value(&s.value)
	if s.cs.Help != "" {
		input = input.Description(s.cs.Help)
	}
	return huh.NewGroup(input), nil
}

func (s *configFieldScreen) Apply(_ context.Context, st *tui.State) error {
	v := s.value
	if st.Has(s.cs.Key) {
		v = st.Get(s.cs.Key)
	}
	st.Set(s.cs.Key, v)
	if v != "" {
		st.Note(s.cs.Prompt, v)
	}
	return nil
}
