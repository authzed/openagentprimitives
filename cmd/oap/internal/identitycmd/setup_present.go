// The presentation half of `oap identity setup` and `oap agent setup-identity`.
//
// A credential-setup flow describes its screens and stores what they collected;
// everything around that belongs here, in the same shape `oap channel create`
// uses for a channel wizard:
//
//	detect terminal capabilities → theme → run the screens → commit the result
//	→ render the summary that stays in the user's scrollback
//
// The setup engine drives the requirement loop — which credentials a run owes,
// which are already provisioned, whether a rejected token gets another try —
// and calls in here once per attempt. That split is what keeps every decision
// about HOW a run is presented in one place, and out of five provider flows.
package identitycmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/charmbracelet/huh"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelwizard"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
)

// SetupOptions is what a setup run was asked for, beyond which
// credentials it is for. Held as one struct so both identity commands take the
// same shape and cannot wire the same flag two different ways.
type SetupOptions struct {
	// nonInteractive is `--non-interactive`, and it means exactly one thing
	// here: every credential this run covers must ALREADY be provisioned.
	//
	// There is deliberately no way to answer a setup question from a flag.
	// Every answer a credential flow asks for either is a credential or mints
	// one, and a flag value lands in shell history and in the process table —
	// so the only non-interactive run this command offers is the one that asks
	// nothing at all: verify what is there, refuse what is not, and touch
	// nothing on the way. The engine's own idempotency check is what makes that
	// useful, since it detects a provisioned credential by name and skips its
	// flow before any screen, browser or network probe.
	nonInteractive bool

	// NoColor is the --no-color flag, carried through to capability detection.
	// The agent-side command sets it from its Globals, so it is exported.
	NoColor bool
}

// newIdentityFlowPresenter returns the presenter both identity commands hand to
// the setup engine. It is the whole presentation lifecycle for one flow run.
func newIdentityFlowPresenter(in io.Reader, out, errOut io.Writer, theme *tui.Theme, opts SetupOptions) setup.FlowPresenter {
	return func(ctx context.Context, run setup.FlowRun) error {
		return presentIdentityFlow(ctx, in, out, errOut, theme, opts, run)
	}
}

// SetupRunOptions is how this command's flows are presented.
//
// Split out because Inline is not visible in the output of any driver a test
// can build, so this is the only place the decision below can be read back.
//
// NOT inline: every answer a flow collects can be given from what the form
// itself shows. A flow that sends the user to a browser composes the address
// into its own guidance note rather than printing it to the stream, so there is
// no output above or between the questions for an answer to depend on — which
// is what the rule turns on, not whether the credential itself originates
// somewhere off-terminal. (See tui.Options.Inline.)
func SetupRunOptions(
	theme *tui.Theme,
	in io.Reader,
	out io.Writer,
	providerID string,
	nonInteractive bool,
) tui.Options {
	return tui.Options{
		Theme:          theme,
		Title:          identitySetupTitle(providerID),
		In:             in,
		Out:            out,
		NonInteractive: nonInteractive,
	}
}

// presentIdentityFlow runs one flow's screens, commits what they collected, and
// writes the record of it.
//
// The State is built fresh per call rather than reused across the engine's
// retries: presenting a screen mutates it, and a retry that found its answer
// already recorded would re-store the value the user was just asked to replace.
func presentIdentityFlow(
	ctx context.Context,
	in io.Reader, out, errOut io.Writer,
	theme *tui.Theme,
	opts SetupOptions,
	run setup.FlowRun,
) error {
	st := tui.NewState()

	runOpts := SetupRunOptions(theme, in, out, run.ProviderID, opts.nonInteractive)
	if opts.nonInteractive {
		// Asked before the driver is built, and before any screen runs, so a run
		// that cannot succeed says why up front. Without it the browser steps —
		// which ask nothing, and so are never offered to the fail-closed driver
		// — open a tab on their way to a refusal the user asked for.
		if err := channelwizard.CheckInteractionRequired(run.Screens, st); err != nil {
			return err
		}
		runOpts.Driver = notProvisioned{
			inner: tui.DriverFor(tui.DriverParams{
				Theme:          theme,
				In:             in,
				Out:            out,
				NonInteractive: true,
			}),
			credential: run.CredentialName,
		}
	}

	answered, err := tui.RunWith(ctx, run.Screens, runOpts, st)
	if err != nil {
		// Stripped HERE, at the one call site that produced the framing, rather
		// than left for the engine to wrap again: `tui: apply screen "token":`
		// in front of a message about a malformed token is this command's
		// plumbing showing through, and once the engine has added `setup:
		// <credential>:` on top there is no longer a reliable frame to remove.
		return tui.UserFacing(err)
	}

	if err := run.Commit(ctx, answered); err != nil {
		// Returned unchanged: the engine matches its own sentinels on this
		// error to decide whether to offer another attempt.
		return err
	}

	// The summary is the record that survives the alt-screen being released, so
	// it is written after the run, to the same stream the run used — which puts
	// it AFTER the point of no return, because Commit has already written the
	// credential to the AgentIdentity's Secret.
	//
	// Its failure therefore cannot fail the requirement. A closed pipe or a full
	// disk would otherwise make this attempt return an error, and since that
	// error is none of the engine's sentinels the retry loop turns it straight
	// into "setup: <credential>: …" — reporting a failure for a credential that
	// is durably stored, which is the one wrong answer here: the user re-runs,
	// or goes hunting for a credential that already exists.
	//
	// Rendering BEFORE Commit was the alternative and is worse: it would print a
	// summary of decisions that a refusing Commit never persisted.
	if err := tui.RenderSummary(out, theme, answered.Notes()); err != nil {
		// Not swallowed: the credential IS stored, and the operator should know
		// its confirmation did not print. Stderr rather than stdout, because
		// stdout is the stream that just failed.
		if errOut != nil {
			fmt.Fprintf(errOut, "%s\n", theme.Render(theme.Warn,
				"the credential was stored, but its summary could not be displayed: "+err.Error()))
		}
	}
	return nil
}

// identitySetupTitle is the chrome's title bar for one flow run. One function
// rather than a literal at each call site, so both identity commands cannot
// title the same flow differently.
func identitySetupTitle(providerID string) string { return "oap · identity setup · " + providerID }

// notProvisioned re-words the fail-closed driver's refusal for a setup run.
//
// It decorates rather than replaces: the driver underneath is whatever
// DriverFor chose, so the decision of WHICH driver a run gets stays in the one
// place that makes it.
//
// The re-wording is not cosmetic. The driver's own message — `screen "token"
// has no answer; supply it via flags or drop --non-interactive` — is wrong twice
// over on this command: it names a screen ID, which is our vocabulary and not
// the user's, and it points at flags that deliberately do not exist here. What
// reaching a question actually proves is that this credential is not set up
// yet, which is the one thing a run told not to prompt cannot fix.
type notProvisioned struct {
	inner tui.Driver

	// credential is the name the user's own manifests use, and the name the
	// run printed as it started on it — so a refusal in a multi-credential run
	// says which one stopped it.
	credential string
}

func (d notProvisioned) Present(ctx context.Context, screenID string, g *huh.Group) error {
	err := d.inner.Present(ctx, screenID, g)
	if err == nil || !errors.Is(err, tui.ErrUnanswered) {
		return err
	}
	return fmt.Errorf("%w: %s is not set up yet, and setting it up means answering questions "+
		"--non-interactive forbids. Drop the flag to set it up now", tui.ErrUnanswered, d.credential)
}
