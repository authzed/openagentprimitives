package installcmd

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// Rail-step IDs for the unified init wizard's phases that are NOT keyed off an
// existing screen/flag constant. The routing screens reuse
// flagTrustedHostname/flagSandboxHostname/flagACMEEmail (routing_ask.go) and
// the config screens reuse keyDetected/keyWorkspaceClass/… (accept_existing.go),
// so only the two below are new.
//
// Defined here beside the screens that reference them. Task 11-B owns the full
// rail assembly (initRailSteps / railState / runInitWizard); it will place these
// IDs onto the rail. Keeping the IDs next to their screens means the screen and
// its rail slot cannot disagree on the string.
const (
	keyProceed  = "init.proceed"
	keySettings = "init.settings"
)

// newRoutingScreens builds the external-access questions — trusted hostname,
// artifact-viewer (sandbox) hostname, and the ACME certificate email — as
// tui.Screens so they join the unified init wizard's rail, keyed by the SAME
// flag constants the classic ASK uses (flagTrustedHostname/flagSandboxHostname/
// flagACMEEmail) and pre-filled from the detected settings d.
//
// This is the tui.Screen analogue of webdRoutingInputs (routing_ask.go), and it
// reproduces that function's gates exactly — not just its questions — because
// the wizard gathers routing BEFORE RunInstall runs, where the classic ASK
// gathers it as part of RunInstall itself. A question webdRoutingInputs
// deliberately never asks, if asked here anyway, collects an answer that
// applyRoutingAnswers folds into WebdRoutingOpts and only THEN validateWebdRouting
// refuses — after the build phase already ran. This function deliberately does
// NOT re-implement validateWebdRouting or the --hostname-suffix expansion:
// applyRoutingAnswers folds the gathered values into WebdRoutingOpts, and
// RunInstall's own gate (withHostnameSuffix + validateWebdRouting) still runs
// against them exactly as before — the wizard only moves WHERE the values come
// from, never the refusals. Because the gathered values land in r before
// RunInstall is called, RunInstall's internal askWebdRoutingInputs sees them
// non-empty and asks nothing (apply-only).
//
// p is the resolved cluster's InstallProfile and r is the routing opts already
// folded from flags (and, for a re-run mid-wizard, from State) — both needed to
// reproduce webdRoutingInputs' gates:
//
//   - r.manualWebdRouting -> no screens at all (routing_ask.go:58-60): the
//     operator is wiring routing themselves, so every question below is
//     contradictory rather than missing.
//   - trusted screen presented only when p.RequiresExternalHostname() or
//     something is already known about it (routing_ask.go:67): a kind that
//     tolerates no external hostname (local, desktop, the unmanaged default)
//     is not asked for one on a bare Enter — an empty answer is a legitimate
//     choice there, not a pending failure. A flag value still reaches State
//     (seeded by the orchestrator before this screen list is built) even when
//     the screen itself is omitted; a DETECTED hostname is worth presenting to
//     confirm/edit.
//   - sandbox screen Skips under r.disableViewer (routing_ask.go:82) in
//     addition to "no trusted origin yet" — that flag is the conscious choice
//     to install without a viewer, and an answer here would contradict it.
//
// Every question is Optional: a blank answer is legitimate, and
// applyRoutingAnswers folds back only non-empty answers, so a blank falls
// through to the same gate the classic path does.
//
// The sandbox and email screens Skip when there is no trusted origin — the same
// condition webdRoutingInputs encodes as AskWhen (a second origin, and a
// certificate, only mean something once there is a first origin to secure). The
// email screen additionally Skips when flagTLSIssuer is present in State, the
// same escape hatch the classic ASK reads: an operator who named their own TLS
// issuer is not asked for an ACME account. The orchestrator (Task 11-B) seeds
// flagTLSIssuer from r.tlsIssuer, mirroring askWebdRoutingInputs' seed.
func newRoutingScreens(p cloud.InstallProfile, r WebdRoutingOpts, d DetectedSettings) []tui.Screen {
	if r.manualWebdRouting {
		return nil
	}

	trustedHost := d.TrustedHostname
	sandboxHost := d.SandboxHostname
	acmeEmail := d.ACMEEmail

	var screens []tui.Screen
	if p.RequiresExternalHostname() || trustedHost != "" {
		screens = append(screens, tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        flagTrustedHostname,
				Label:     "Routing",
				Key:       flagTrustedHostname,
				Title:     "Hostname for the web UI, on a domain you control (e.g. webd.example.com)",
				NoteLabel: "Web UI hostname",
			},
			Optional: true,
			Default:  func() string { return trustedHost },
		}))
	}
	screens = append(screens,
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        flagSandboxHostname,
				Label:     "Routing",
				Key:       flagSandboxHostname,
				Title:     "A second hostname, for viewing content the agent generates (e.g. sandbox.example.com)",
				NoteLabel: "Artifact viewer hostname",
				Skip: func(st *tui.State) bool {
					return r.disableViewer || st.Get(flagTrustedHostname) == ""
				},
			},
			Optional: true,
			Default:  func() string { return sandboxHost },
		}),
		tui.NewText(tui.TextOpts{
			QuestionOpts: tui.QuestionOpts{
				ID:        flagACMEEmail,
				Label:     "Routing",
				Key:       flagACMEEmail,
				Title:     "Email address for the automatic HTTPS certificate (renewal notices go here)",
				NoteLabel: "HTTPS certificate email",
				Skip: func(st *tui.State) bool {
					return st.Get(flagTrustedHostname) == "" || st.Get(flagTLSIssuer) != ""
				},
			},
			Optional: true,
			Default:  func() string { return acmeEmail },
		}),
	)
	return screens
}

// newProceedScreen is the unified init wizard's final go/no-go, presented as a
// tui.Screen so it occupies its own rail step (keyProceed). Default is true: a
// bare Enter proceeds, matching confirmPlan's "silence proceeds" rule. The plan
// summary is carried as Guidance (proceedGuidance) — a concise equivalent of
// what confirmPlan prints, not a duplicate of its full component list.
//
// The orchestrator (Task 11-B) reads proceedConfirmed after this screen: on a
// "no" it returns cleanly with nothing applied, and on a "yes" it sets
// WebdRoutingOpts.preconfirmed so confirmPlan does not re-ask this same question
// once RunInstall runs.
func newProceedScreen() tui.Screen {
	return tui.NewConfirm(tui.ConfirmOpts{
		QuestionOpts: tui.QuestionOpts{
			ID:        keyProceed,
			Label:     "Proceed",
			Key:       keyProceed,
			Title:     "Proceed with the install?",
			NoteLabel: "Proceed",
			Guidance:  proceedGuidance,
		},
		Default: true,
	})
}

// proceedGuidance summarizes the install plan for the Proceed screen. It reads
// the trusted origin out of State (set by the routing screens just above on the
// rail), so it names the HTTPS endpoint and its slower time estimate only when
// there will be one — mirroring confirmPlan's own two-way summary.
func proceedGuidance(st *tui.State) string {
	lines := []string{
		"oap will install the agent platform: the operator, authorization (SpiceDB), " +
			"memory (PostgreSQL), the web UI, and channel transports.",
	}
	if host := st.Get(flagTrustedHostname); host != "" {
		lines = append(lines,
			"Secure HTTPS at https://"+host,
			"Estimated time: ~3–7 minutes (the cloud load balancer is the slow part).")
	} else {
		lines = append(lines, "Estimated time: ~2–5 minutes.")
	}
	return strings.Join(lines, "\n")
}

// proceedConfirmed reports whether the Proceed screen resolved to yes. It is
// true unless the user explicitly said no — a Default:true confirm, so silence,
// and an absent key (a run that never reached the screen), both proceed.
func proceedConfirmed(st *tui.State) bool {
	return !st.Has(keyProceed) || st.Bool(keyProceed)
}

// applyRoutingAnswers folds the routing questions' State answers back into r —
// the tui.Screen mirror of askWebdRoutingInputs' fold-back (routing_ask.go).
// Only a non-empty answer is written, so a blank (a skipped conditional
// question, or a local install with no external origin) never overwrites a value
// a flag already supplied.
func applyRoutingAnswers(r WebdRoutingOpts, st *tui.State) WebdRoutingOpts {
	if v := st.Get(flagTrustedHostname); v != "" {
		r.trustedHostname = v
	}
	if v := st.Get(flagSandboxHostname); v != "" {
		r.sandboxHostname = v
	}
	if v := st.Get(flagACMEEmail); v != "" {
		r.acmeEmail = v
	}
	return r
}
