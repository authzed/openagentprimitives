package installcmd

import (
	"context"
	"io"

	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/initpipeline"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Flag names the ASK phase resolves. Constants because each one is written
// three times over — the Input that declares it, the seed that answers it from
// the command line, and the answer folded back into WebdRoutingOpts — and a
// typo in any one of them is silent: the input is asked, and its answer is
// dropped on the floor.
const (
	flagTrustedHostname = "trusted-hostname"
	flagSandboxHostname = "sandbox-hostname"
	flagACMEEmail       = "acme-email"
	flagTLSIssuer       = "tls-issuer"
)

// webdRoutingInputs declares the external-access values as the ASK phase's
// Inputs, so a terminal-attached install can ask for them.
//
// ONE rule decides every entry: an input is declared exactly when its absence
// is already a hard error, and only then. --trusted-hostname missing on a
// cluster kind that requires one fails in validateWebdRouting, before anything
// is applied; --sandbox-hostname missing beside a trusted origin fails there
// too; --acme-email missing fails much later, in the cert-manager TLS
// strategy, after the whole data plane is already up.
//
// Asking moves where those values come from. It does not move, soften, or
// duplicate the refusals: every Input here is Required:false on purpose, so a
// blank answer — or a run with nobody at the terminal — falls through to the
// existing gate and the operator reads the existing message. This phase is a
// route to answering a question, never a second way to be told no.
//
// Call it AFTER WebdRoutingOpts.withHostnameSuffix. --hostname-suffix is the
// convenience form of the two hostnames, so by the time this runs an operator
// who passed it has answered both, and both declarations below see a non-empty
// value and are not made.
//
// p decides only the first entry: a kind that tolerates an install with no
// external hostname (local, desktop, and the unmanaged default) must not be
// asked for one, because there the empty value is a legitimate choice rather
// than a pending failure.
func webdRoutingInputs(p cloud.InstallProfile, r WebdRoutingOpts) []initpipeline.Component {
	// --manual-webd-routing is the operator saying they wire routing
	// themselves. Every value below is one oap would use to do it for them, so
	// none of them is asked for: with the flag set, supplying any of them is
	// contradictory rather than missing.
	if r.manualWebdRouting {
		return nil
	}

	// Every input below configures a trusted origin, so none of them is
	// declared unless this install will have one — supplied already, or about
	// to be asked for because a cluster kind that requires one is missing it.
	// That early return is what keeps a local install, where no external
	// hostname is a legitimate outcome, from resolving a run at all.
	askTrusted := r.trustedHostname == "" && p.RequiresExternalHostname()
	if r.trustedHostname == "" && !askTrusted {
		return nil
	}

	var access []initpipeline.Input
	if askTrusted {
		access = append(access, initpipeline.Input{
			Flag:   flagTrustedHostname,
			Prompt: "Hostname for the web UI, on a domain you control (e.g. webd.example.com)",
		})
	}
	// The artifact viewer's own origin. Not declared under
	// --disable-artifact-viewer: that flag is the conscious choice to install
	// without a viewer, and an answer here would contradict it.
	if r.sandboxHostname == "" && !r.disableViewer {
		access = append(access, initpipeline.Input{
			Flag:   flagSandboxHostname,
			Prompt: "A second hostname, for viewing content the agent generates (e.g. sandbox.example.com)",
			// Only meaningful once there is a trusted origin to be a second
			// origin to — including one typed into the screen just above.
			AskWhen: func(a initpipeline.Resolved) bool { return a[flagTrustedHostname] != "" },
		})
	}

	var comps []initpipeline.Component
	if len(access) > 0 {
		comps = append(comps, initpipeline.Component{Name: "external access", Inputs: access})
	}
	if r.acmeEmail == "" {
		comps = append(comps, initpipeline.Component{
			Name: "https certificate",
			Inputs: []initpipeline.Input{{
				Flag:   flagACMEEmail,
				Prompt: "Email address for the automatic HTTPS certificate (renewal notices go here)",
				// The cluster's existing ClusterIssuer email, when one was
				// detected (seededACMEEmail). Empty otherwise. As a Default it
				// pre-fills the prompt and folds back on a bare-Enter, so a
				// re-install offers the current email without answering for the
				// operator — the question is still asked and still editable.
				Default: r.acmeEmailDetected,
				// The condition the certificate step itself applies, stated
				// once: an email is wanted only when there is a hostname to
				// certify and no existing issuer was named to certify it.
				AskWhen: func(a initpipeline.Resolved) bool {
					return a[flagTrustedHostname] != "" && a[flagTLSIssuer] == ""
				},
			}},
		})
	}
	return comps
}

// seededACMEEmail reads the cluster's existing Let's Encrypt ClusterIssuer and
// records its ACME email as r.acmeEmailDetected — the cert-email prompt's
// editable Default — when the flag left acmeEmail empty and this install will
// actually create that issuer (a trusted origin, no --tls-issuer, not
// --manual-webd-routing).
//
// It writes acmeEmailDetected, NOT acmeEmail, on purpose: acmeEmail is the
// flag value, and webdRoutingInputs declares the cert-email question only when
// acmeEmail is empty. Writing the detected value there would suppress the
// question and flow an unconfirmed, uneditable email straight to the apply.
// As a Default the value pre-fills the prompt instead — a bare-Enter keeps it,
// a typed answer replaces it — while --acme-email still skips the prompt.
//
// A detect failure is not fatal — a missing prompt default is not worth
// failing an install over, so it is surfaced as a warning (never silently
// dropped, per AGENTS.md) and r is returned unchanged: the ASK still runs
// and collects the email itself.
func seededACMEEmail(ctx context.Context, dyn dynamic.Interface, r WebdRoutingOpts, out io.Writer) WebdRoutingOpts {
	if r.acmeEmail != "" || r.manualWebdRouting || r.tlsIssuer != "" {
		return r
	}
	email, err := cloud.DetectLetsEncryptEmail(ctx, cloud.Clients{Dynamic: dyn})
	if err != nil {
		cliout.Warn(out, "could not detect existing ACME email for the prompt default: %v", err)
		return r
	}
	if email != "" {
		r.acmeEmailDetected = email
	}
	return r
}

// askWebdRoutingInputs runs the ASK phase over webdRoutingInputs and folds the
// answers back into r, which the caller then validates exactly as before.
//
// interactive is passed in rather than measured here so it is the SAME fact
// the caller measured about the SAME stream it hands to in — a prompt reading
// one stream while interactivity was decided about another is how a question
// ends up waiting on a terminal nobody is at. Which driver that resolves to is
// tui.DriverFor's decision, not this function's.
//
// --assume-yes suppresses the whole phase, and the check lives HERE rather
// than at the call site so a second caller cannot forget it. It means "do not
// prompt me", and it has no answer to "what hostname?" — it can accept an
// offer oap makes, not supply a value oap cannot invent. A scripted install that
// passes -y on a terminal and omits the flag used to exit non-zero naming it;
// asking anyway would turn that clean, actionable exit into a block on a
// question nobody is there to answer. Falling through to the gate keeps the
// message and the exit exactly as they were.
//
// That is the one place this phase parts company with the other value `oap
// install` prompts for mid-flow, the image registry, which gates on
// interactivity alone: that prompt has a derived suggestion to accept, and
// these questions have nothing to fill in.
func askWebdRoutingInputs(ctx context.Context, out io.Writer, in io.Reader, noColor, interactive bool, p cloud.InstallProfile, r WebdRoutingOpts) (WebdRoutingOpts, error) {
	// The unified init wizard gathers these same values through its own rail
	// screens and folds them into r before ever calling RunInstall; preconfirmed
	// is the orchestrator's signal that routing was already asked. Re-asking here
	// would prompt the same production TTY a second time for hostname/sandbox/
	// email — the exact double-ask this suppresses. Fold-back covers the common
	// case (every answered field is non-empty, so webdRoutingInputs declares
	// nothing), but a wizard field left blank — a trusted origin with no sandbox,
	// say — would still be re-declared here; short-circuiting on the orchestrator's
	// own signal makes suppression unconditional. It changes no applied value: the
	// folded values are already in r and still flow to validateWebdRouting and the
	// apply exactly as before, and the same gate still refuses a genuinely-missing
	// hostname. Every flag-driven caller leaves preconfirmed false and this phase
	// runs unchanged.
	if r.preconfirmed {
		return r, nil
	}
	if r.assumeYes {
		return r, nil
	}

	comps := webdRoutingInputs(p, r)
	if len(comps) == 0 {
		return r, nil
	}

	answers, err := initpipeline.Resolve(ctx, comps, map[string]string{
		flagTrustedHostname: r.trustedHostname,
		flagSandboxHostname: r.sandboxHostname,
		flagACMEEmail:       r.acmeEmail,
		// Seeded although no screen ever asks for it: it is what the
		// --acme-email question reads to decide it is unnecessary.
		flagTLSIssuer: r.tlsIssuer,
	}, initpipeline.AskOptions{
		Interactive: interactive,
		In:          in,
		Out:         out,
		Theme:       tui.NewTheme(apcmd.DetectCaps(out, noColor)),
	})
	if err != nil {
		return r, err
	}

	// Only a non-empty answer is folded back. An input that was declared but
	// never answered — a conditional question that declined, a blank line, a
	// run with no terminal — resolves to its empty Default, and writing that
	// over a value the command line supplied would undo the very flag that
	// made the question skip.
	if v := answers[flagTrustedHostname]; v != "" {
		r.trustedHostname = v
	}
	if v := answers[flagSandboxHostname]; v != "" {
		r.sandboxHostname = v
	}
	if v := answers[flagACMEEmail]; v != "" {
		r.acmeEmail = v
	}
	return r, nil
}
