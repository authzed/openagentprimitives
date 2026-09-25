package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// errVerificationDeclined signals that the user saw a provider-rejected token
// and chose not to store it; the engine re-runs the flow so they can correct it.
var errVerificationDeclined = errors.New("setup: token rejected by provider; user declined to store it")

// maxVerifyAttempts bounds the decline→re-prompt loop.
const maxVerifyAttempts = 3

// RunRequest is what the CLI hands to Run.
type RunRequest struct {
	Namespace    string
	IdentityName string
	UserIntent   string
	// Targets are the prefix-typed match strings to iterate: "cli:<toolkit>",
	// "toolspec:<spec>", "mcp:<server>". Built by the CLI from the AgentClass spec
	// or from --toolkits/--mcps flags.
	Targets []string
	// Force skips the idempotency check.
	Force bool
	// OnlyMatch, when non-empty, filters Targets to entries that exactly equal it.
	OnlyMatch string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Theme styles every status line this engine writes, and is the same theme the
	// command's presenter renders its screens and summary with.
	//
	// Handed in rather than derived here: terminal capabilities are established
	// once per command, from the stream it was actually given. Nil is defaulted by
	// Run to the plain theme, so a caller that states nothing degrades to plain
	// text instead of panicking on the first Render.
	Theme *tui.Theme

	// Present drives one builtin flow's screens to completion, persists what they
	// collected, and writes the record of it. Supplied by the command, because
	// presentation is the command's business: which driver a terminal gets, what a
	// refusal says, and what survives in scrollback.
	//
	// Required for any requirement that resolves to a builtin flow; a nil Present
	// refuses those rather than inventing a presentation. The LLM-driven fallback
	// path does not use it.
	//
	// A presenter closes over its OWN streams, and they MUST be the same
	// Stdin/Stdout this request carries — nothing here can detect a mismatch, and a
	// presenter prompting on one terminal while the run narrates itself on another
	// is a split-brain run that reports no error at all. Wire both from one place.
	// They cannot be collapsed while the engine still writes status lines and reads
	// its store-anyway confirmation directly, between presentations.
	Present FlowPresenter

	// NonInteractive is the --non-interactive flag. It governs the two questions
	// this engine asks on its own account — the store-anyway confirmation and the
	// assisted setup agent — as well as the ones a flow's screens ask.
	//
	// Needed HERE as well as on the presenter because those two bypass the
	// sequencer entirely — the confirmation is asked from inside the Store
	// callback after the run ended, the agent asks through its own tools — so
	// without it they would sit waiting on a terminal nobody is watching.
	NonInteractive bool
}

// FlowRun is one credential-setup flow, described but not yet presented.
type FlowRun struct {
	// ProviderID names the provider whose flow this is, for the chrome's title.
	ProviderID string

	// CredentialName is the credential being set up, in the words the user's
	// own manifests use (the requirement's suggested name).
	CredentialName string

	// Screens is the sequence the flow described. Never empty: the Flow
	// contract requires at least one screen alongside a nil error.
	Screens []tui.Screen

	// Commit persists what the screens collected, by calling the flow's own
	// Result. It MUST be called exactly once, after the screens complete and
	// before the summary is written, and its error MUST be returned unchanged
	// (%w wrapping is fine): the engine matches sentinels on it to decide whether
	// to re-prompt.
	Commit func(ctx context.Context, st *tui.State) error
}

// FlowPresenter presents one flow and commits its result.
//
// Injected rather than registered: there is exactly one concrete presentation
// per binary, chosen at startup by the command that owns the terminal.
type FlowPresenter func(ctx context.Context, run FlowRun) error

// styles returns the theme this run renders with. The fallback is not
// decoration: helpers here are reachable from tests that build a RunRequest
// directly, and a nil *tui.Theme panics on first Render.
func (r RunRequest) styles() *tui.Theme {
	if r.Theme != nil {
		return r.Theme
	}
	return tui.NewTheme(tui.Caps{})
}

// step, done, caution and aside each write one status line to Stdout in the
// voice it carries: what is happening now, what succeeded, what the user should
// look at, what they can ignore. Named for intent rather than color, so a call
// site says what it means and the meaning-to-style mapping is made once here.
func (r RunRequest) step(text string)    { r.line(r.styles().Title, text) }
func (r RunRequest) done(text string)    { r.line(r.styles().Success, text) }
func (r RunRequest) caution(text string) { r.line(r.styles().Warn, text) }
func (r RunRequest) aside(text string)   { r.line(r.styles().Subtle, text) }

func (r RunRequest) line(s lipgloss.Style, text string) {
	fmt.Fprintf(r.Stdout, "%s\n", r.styles().Render(s, text))
}

// Run iterates Targets, enumerates each one's CredentialRequirements, dispatches
// each to its builtin Go flow, and surfaces per-requirement results. Returns nil
// if every requirement succeeded or was skipped cleanly, otherwise the first
// hard error (cluster fault, panic in a builtin).
func Run(ctx context.Context, c client.Client, req RunRequest) error {
	// Resolved once so every status line below renders with the same theme. The
	// command supplies it; this only substitutes plain text for a caller with none.
	req.Theme = req.styles()

	// A single credential is often required by several targets. seen dedups by
	// credential across the whole run, so each one is provisioned — and printed —
	// exactly once, not once per referencing target.
	seen := map[string]bool{}
	for _, t := range req.Targets {
		if req.OnlyMatch != "" && t != req.OnlyMatch {
			continue
		}
		if err := runOne(ctx, c, req, t, seen); err != nil {
			return err
		}
	}
	return nil
}

func runOne(ctx context.Context, c client.Client, req RunRequest, matchStr string, seen map[string]bool) error {
	kind, suffix, err := registry.ParseBindingMatch(matchStr)
	if err != nil {
		return fmt.Errorf("setup: %s: %w", matchStr, err)
	}
	target, err := kind.ResolveTarget(ctx, c, req.Namespace, suffix)
	if err != nil {
		return fmt.Errorf("setup: resolve %s: %w", matchStr, err)
	}
	reqs := kind.SetupRequirements(ctx, target)
	if len(reqs) == 0 {
		req.aside("→ " + matchStr + ": no credentials required")
		return nil
	}
	for _, r := range reqs {
		if err := runRequirement(ctx, c, req, kind, target, matchStr, r, seen); err != nil {
			return err
		}
	}
	return nil
}

func runRequirement(
	ctx context.Context, c client.Client, req RunRequest,
	kind authkind.Kind, target authkind.Target, matchStr string,
	r authkind.CredentialRequirement, seen map[string]bool,
) error {
	// Skip silently on repeat, before the separator below, so a credential shared
	// by several targets is never printed or prompted twice.
	dedupKey := r.SuggestedName + "|" + r.ProviderID
	if seen[dedupKey] {
		return nil
	}
	seen[dedupKey] = true

	// Trailing blank line between requirements for visual separation.
	defer fmt.Fprintln(req.Stdout)

	// Idempotency check.
	if !req.Force {
		ok, why, err := isAlreadySetUp(ctx, c, req.Namespace, req.IdentityName, r)
		if err != nil {
			return err
		}
		if ok {
			req.aside("→ already set up: " + r.SuggestedName + " (" + why + ")")
			return nil
		}
		if why == "expired" {
			if req.NonInteractive {
				// A run that cannot prompt is asserting that everything this agent
				// needs is ready, and its exit code IS that answer. An expired token
				// is present and dead, so passing it off as a nudge would let a
				// readiness gate wave through an agent that cannot authenticate, and
				// the caution below has nobody to read it.
				return fmt.Errorf("setup: %s: the stored OAuth token has expired, so this credential is not ready; "+
					"run 'oap identity refresh %s', or re-run without asking to skip prompts to set it up again",
					r.SuggestedName, req.IdentityName)
			}
			req.caution("→ " + r.SuggestedName + ": OAuth token expired — run 'oap identity refresh " + req.IdentityName + "' for a faster path, " +
				"or pass --force to re-run full setup.")
			return nil
		}
	}

	// Resolve the provider (or fall through to inline-prompt path).
	if r.ProviderID == "" {
		if r.InlinePrompt == "" {
			return fmt.Errorf("setup: requirement %q has no provider and no inline prompt", r.SuggestedName)
		}
		// Inline-prompt path: no builtin provider, use the LLM-driven agent with
		// the toolkit's inline prompt as system-prompt context.
		if err := refuseAssistedSetup(req, r); err != nil {
			return err
		}
		req.step("→ " + r.SuggestedName + ": no builtin provider; using LLM-driven setup agent (inline prompt) …")
		if err := llmagent.Run(ctx, llmAgentRequestFor(r, nil, kind, target, matchStr, req, c)); err != nil {
			return handleLLMAgentErr(err, req, r, matchStr)
		}
		return nil
	}
	prov, ok := provider.ByID(r.ProviderID)
	if !ok {
		return fmt.Errorf("setup: requirement %q references unknown provider %q", r.SuggestedName, r.ProviderID)
	}
	flow, ok := builtins.Get(prov.Builtin)
	if !ok {
		// No builtin for this provider — use the LLM-driven agent with the
		// provider's prompt, docs URL, and run_shell allowlist as context.
		if err := refuseAssistedSetup(req, r); err != nil {
			return err
		}
		req.step("→ " + r.SuggestedName + ": provider \"" + prov.ID + "\" has no builtin; using LLM-driven setup agent …")
		if err := llmagent.Run(ctx, llmAgentRequestFor(r, prov, kind, target, matchStr, req, c)); err != nil {
			return handleLLMAgentErr(err, req, r, matchStr)
		}
		return nil
	}

	req.step("→ setting up " + r.SuggestedName + " via provider " + prov.ID + " …")
	flowReq := builtinsRequestFor(r, prov, kind, target, matchStr, req, c)

	// Live-verify the credential before it is persisted. The Store callback is the
	// one choke point every flow funnels through, so the warn/confirm UX lives
	// here once instead of in each flow.
	//
	// This closure replaces flowReq.Store outright rather than wrapping it and
	// delegating: res.SubjectID only exists here, and the delegated Store field's
	// signature (builtins.Request.Store) is fixed at func(ctx, StoreValue) error,
	// so there is no parameter to carry it through. Calling Store directly keeps
	// the already-computed VerifyResult from being thrown away and re-verified.
	flowReq.Store = func(ctx context.Context, v builtins.StoreValue) error {
		res := builtins.VerifyCredential(ctx, prov, v)
		switch res.Status {
		case builtins.VerifyValid:
			if res.Detail != "" {
				req.done("✓ verified: " + res.Detail)
			}
		case builtins.VerifyIndeterminate:
			req.caution("⚠ could not verify the token (" + res.Detail + "); storing anyway")
		case builtins.VerifyUnsupported:
			req.aside("→ no live verification available for this provider; stored")
		case builtins.VerifyRejected:
			req.caution("✗ " + res.Detail)
			if err := confirmStoreAnyway(req); err != nil {
				return err
			}
		case builtins.VerifyForbidden:
			// The provider took the credential and refused this one check. Not a
			// rejection, so not worded as one — but there is a human at the keyboard,
			// so ask rather than store on a verdict that confirmed nothing.
			req.caution("⚠ " + builtins.ForbiddenNotice(res))
			if err := confirmStoreAnyway(req); err != nil {
				return err
			}
		default:
			// Exhaustiveness backstop. Every VerifyStatus this build knows is named
			// above, so reaching here means a verdict was added without teaching this
			// wizard what it means. Ask the human rather than let the NEXT verdict
			// added fall open into an unattended store.
			req.caution("⚠ " + builtins.UnrecognizedNotice(res))
			if err := confirmStoreAnyway(req); err != nil {
				return err
			}
		}
		return storeCredential(ctx, c, req, r, v, res.SubjectID)
	}

	for attempt := 1; ; attempt++ {
		err := runFlow(ctx, flow, flowReq, req, prov.ID, r.SuggestedName)
		if err == nil {
			break
		}
		if errors.Is(err, errVerificationDeclined) && attempt < maxVerifyAttempts {
			req.step("→ token not stored — let's try again …")
			continue
		}
		return fmt.Errorf("setup: %s: %w", r.SuggestedName, err)
	}

	// Read the just-written credential and emit a masked confirmation.
	// Best-effort: a failed read still reports success.
	masked := readMasked(ctx, c, req.Namespace, req.IdentityName, r.SuggestedName)
	if masked != "" {
		req.done("✓ " + r.SuggestedName + " = " + masked)
	} else {
		req.done("✓ " + r.SuggestedName)
	}
	return nil
}

// runFlow drives one attempt at a builtin flow: ask it for its screens, hand
// them to the command's presenter, and let the flow store what they collected
// through the Commit the presenter calls.
//
// Screens are re-requested on every attempt rather than described once and
// re-run: a *huh.Group is stateful and presenting it mutates it in place, and
// screens carry per-run state, so a retry after a declined verification has to
// start from a fresh description of the flow.
//
// Nothing here touches a terminal — the presenter owns that; this function owns
// only the retry loop around it and the sentinel it matches on.
func runFlow(ctx context.Context, flow builtins.Flow, flowReq builtins.Request, req RunRequest, providerID, credentialName string) error {
	screens, err := flow.Screens(ctx, flowReq)
	if err != nil {
		return err
	}
	if req.Present == nil {
		// Fail closed rather than invent a presentation. A flow describes questions
		// and nothing else, so with nowhere to ask them the only alternative would
		// be storing whatever an unanswered State yielded.
		return errors.New("this credential has to be set up interactively, and this command supplied no way to ask")
	}
	return req.Present(ctx, FlowRun{
		ProviderID:     providerID,
		CredentialName: credentialName,
		Screens:        screens,
		Commit: func(ctx context.Context, st *tui.State) error {
			return flow.Result(ctx, flowReq, st)
		},
	})
}

// refuseAssistedSetup stops a run that was told not to prompt before it reaches
// the LLM-driven setup agent, or returns nil when the run may proceed.
//
// That agent works BY asking: it opens pages, poses questions, and reads what
// the user pastes back. No flag stands in for any of it, so a run with nobody at
// the keyboard would spend an LLM budget opening tabs at an empty chair and fail
// anyway. Refused before the first token is spent, and before the first tab.
func refuseAssistedSetup(req RunRequest, r authkind.CredentialRequirement) error {
	if !req.NonInteractive {
		return nil
	}
	return fmt.Errorf("setup: %s: there is no built-in flow for this credential, so setting it up means working "+
		"through it with the assisted setup agent — which asks questions and opens pages. Re-run without asking "+
		"to skip prompts", r.SuggestedName)
}

// confirmStoreAnyway asks the human whether to store a credential the live check
// did not bless. Shared by every arm of the Store wrapper that needs a decision,
// so those arms cannot drift on what "no" or "I can't ask you" means: both return
// errVerificationDeclined, which the run loop turns into a re-prompt (up to
// maxVerifyAttempts) and then a hard failure.
func confirmStoreAnyway(req RunRequest) error {
	if req.NonInteractive {
		// Not merely "nobody answered": asking at all would block on a terminal the
		// user told us not to use. Declining is the fail-closed reading of a verdict
		// that confirmed nothing.
		return fmt.Errorf("%w (this run was told not to prompt, so the warning above could not be put to anyone)",
			errVerificationDeclined)
	}
	ok, cerr := confirmYN(req.Stdin, req.Stdout, "Store it anyway?")
	if cerr != nil {
		return fmt.Errorf("%w (confirmation unavailable: %v)", errVerificationDeclined, cerr)
	}
	if !ok {
		return errVerificationDeclined
	}
	return nil
}

// confirmYN prints "<prompt> [y/N] " to w and reads one line from r, a byte at
// a time so a call never over-reads into bytes belonging to a later call
// sharing the same reader — a bufio.Reader would buffer past the newline and
// discard those bytes when dropped, and this reader is the same stdin the next
// question is asked over.
//
// Returns true only for y/yes (case-insensitive); anything else, an empty line
// included, is false, so the default is no. EOF before any line is an ERROR
// rather than a no: "the user declined" and "there was nobody to ask" both stop
// the store, but only the second is worth telling the user about.
//
// Line-oriented rather than a tui.Screen because it is asked from inside the
// Store callback, after the run that collected the credential has ended and
// released the screen. Presenting a wizard would take the terminal back over to
// ask one word, wiping the warning the question is being asked about.
//
// A near-twin lives in cmd/oap/internal/identitycmd/verify_gate.go. When the two
// are collapsed, keep THIS one: it checks EOF with errors.Is rather than
// comparing to io.EOF directly, so a reader that wraps its EOF (which io.Reader
// permits, and bufio does) is still recognised as "nobody there" rather than
// reported as a read fault.
func confirmYN(r io.Reader, w io.Writer, prompt string) (bool, error) {
	fmt.Fprintf(w, "%s [y/N] ", prompt)
	var b [1]byte
	var line []byte
	read := false
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			read = true
			if b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return false, fmt.Errorf("read confirmation: %w", err)
		}
	}
	if !read {
		return false, fmt.Errorf("read confirmation: %w", io.EOF)
	}
	switch strings.ToLower(strings.TrimSpace(string(line))) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// handleLLMAgentErr converts ErrUserAborted and ErrBudgetExhausted into soft
// skips: it prints an informative message to stdout and returns nil so the
// engine continues to the next requirement. Any other error is a hard failure.
func handleLLMAgentErr(err error, req RunRequest, r authkind.CredentialRequirement, matchStr string) error {
	switch {
	case errors.Is(err, llmagent.ErrUserAborted):
		req.caution("→ skipped: " + r.SuggestedName + " (user declined or aborted)")
		return nil
	case errors.Is(err, llmagent.ErrBudgetExhausted):
		req.caution("→ skipped: " + r.SuggestedName + " (agent budget exhausted; re-run with --only " + matchStr + " to retry)")
		return nil
	default:
		return fmt.Errorf("setup: %s: %w", r.SuggestedName, err)
	}
}

// storeCredential builds the StoreRequest common to every call site that
// persists a credential and calls Store, so the field list is written once.
// subjectID is the provider account the credential was verified against —
// empty when verification produced none — and lands on the Secret's
// attestation annotations via Store → useridentity.SetAttestation.
func storeCredential(ctx context.Context, c client.Client, req RunRequest, r authkind.CredentialRequirement, v builtins.StoreValue, subjectID string) error {
	return Store(ctx, c, StoreRequest{
		Namespace:    req.Namespace,
		IdentityName: req.IdentityName,
		Requirement:  r,
		Value:        v,
		SubjectID:    subjectID,
	})
}

// builtinsRequestFor builds a builtins.Request for either a builtin flow or the
// LLM-fallback agent, so the Store closure is constructed in one place.
//
// Its Store closure below is reached only by the LLM-agent path: the builtin-flow
// path (runRequirement) replaces flowReq.Store with its own verify-then-store
// closure before any flow can call it, so this one never runs there. On the
// LLM-agent path, verification already happened in llmagent's guardStore before
// this is called; guardStore's subject id rides in on ctx (see
// llmagent.SubjectIDFromContext, set by agent.go's wrappedStore right before it
// calls Store) rather than as a parameter here — Store's signature is
// builtins.Request.Store, func(ctx, StoreValue) error, shared verbatim by the
// llmagent/tools callbacks, so it has no room to grow one.
func builtinsRequestFor(
	r authkind.CredentialRequirement,
	prov *provider.Provider,
	kind authkind.Kind,
	target authkind.Target,
	matchStr string,
	req RunRequest,
	c client.Client,
) builtins.Request {
	return builtins.Request{
		Provider:     prov,
		UserIntent:   strings.TrimSpace(req.UserIntent + " " + target.Intent()),
		Requirement:  r,
		Kind:         kind,
		Target:       target,
		Namespace:    req.Namespace,
		IdentityName: req.IdentityName,
		K8s:          c,
		Store: func(ctx context.Context, v builtins.StoreValue) error {
			return storeCredential(ctx, c, req, r, v, llmagent.SubjectIDFromContext(ctx))
		},
	}
}

// llmAgentRequestFor is builtinsRequestFor plus the terminal. That agent is not
// a builtin flow: it drives tools that prompt the user directly, so it is handed
// the streams rather than describing screens for the sequencer to present.
func llmAgentRequestFor(
	r authkind.CredentialRequirement,
	prov *provider.Provider,
	kind authkind.Kind,
	target authkind.Target,
	matchStr string,
	req RunRequest,
	c client.Client,
) llmagent.Request {
	return llmagent.Request{
		Request: builtinsRequestFor(r, prov, kind, target, matchStr, req, c),
		Stdin:   req.Stdin,
		Stdout:  req.Stdout,
		Stderr:  req.Stderr,
		Theme:   req.styles(),
	}
}

// readMasked best-effort reads the just-stored credential value and returns its
// masked form for status display. Returns "" on any error.
func readMasked(ctx context.Context, c client.Client, namespace, identityName, credName string) string {
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityName}, &ai); err != nil {
		return ""
	}
	var cred *spiceboxv1alpha1.AgentCredential
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == credName {
			cred = &ai.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return ""
	}
	resolved, err := credresolve.ResolveSecretValue(ctx, c, namespace, *cred)
	if err != nil {
		return ""
	}
	return credmask.Mask(string(resolved.UnderlyingValue()))
}

// isAlreadySetUp reports whether ai.Spec.Credentials already holds a credential
// named r.SuggestedName that resolves cleanly: (true, "credential present and
// valid", nil) when set up, (false, reason, nil) when not, (false, "", err) on a
// cluster fault.
func isAlreadySetUp(ctx context.Context, c client.Client, ns, identityName string, r authkind.CredentialRequirement) (bool, string, error) {
	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: identityName}, &ai); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return false, "", err
		}
		return false, "", nil
	}

	var found *spiceboxv1alpha1.AgentCredential
	for i := range ai.Spec.Credentials {
		if ai.Spec.Credentials[i].Name == r.SuggestedName {
			found = &ai.Spec.Credentials[i]
			break
		}
	}
	if found == nil {
		return false, "no credential named " + r.SuggestedName, nil
	}
	if _, rerr := credresolve.ResolveSecretValue(ctx, c, ns, *found); rerr != nil {
		if errors.Is(rerr, credresolve.ErrExpired) {
			return false, "expired", nil
		}
		return false, fmt.Sprintf("incomplete: %v", rerr), nil
	}
	return true, "credential present and valid", nil
}
