package installcmd

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// State keys for the unified init wizard. Routing keys (ACME email, trusted
// hostname, sandbox hostname) reuse flagACMEEmail/flagTrustedHostname/
// flagSandboxHostname from routing_ask.go rather than duplicating them —
// those screens already key their answers by flag name.
const (
	keyDetected        = "init.detected-choice"
	keyExternalSpiceDB = "init.external-spicedb"
	keyWorkspaceClass  = "init.workspace-class"
	keyIdP             = "init.idp-kind"
	keyMonitoring      = "init.monitoring-choice"
)

// acceptMode is the resolved behavior of the init wizard's rail with respect
// to a detected existing install: review every screen, or short-circuit
// every detected answer as already-accepted.
type acceptMode int

const (
	// acceptReviewEach: no shortcut — every screen runs (though a screen may
	// still be pre-filled and confirm-only via seedStateFromDetected /
	// Question.Default).
	acceptReviewEach acceptMode = iota
	// acceptAllExisting: every detected answer is taken as final; screens for
	// those keys short-circuit (State.Has → nil group) rather than asking.
	acceptAllExisting
)

// resolveAcceptMode applies the documented precedence (spec §3) for the init
// wizard's "accept all existing" shortcut:
//
//   - --accept-existing wins for detected settings: it means "keep every
//     currently-detected value", and only makes sense against an existing
//     install. On a fresh cluster (d.Existing == false) there is nothing to
//     accept, so it fails loudly rather than silently falling back to
//     reviewing each screen — a silent no-op here would look like the flag
//     worked when it did nothing.
//   - --defaults supplies the baseline only where nothing was detected; it
//     does not mean "keep existing" and is handled by the screens
//     themselves (each Default closes over DetectedSettings), not here.
//   - -y (assume-yes) accepts offered installs (cert-manager, Gateway
//     controller); it never means "keep existing" either.
//
// So of the three flags, only acceptExisting changes the mode this function
// returns; defaults and assumeYes are accepted as parameters to keep the
// precedence explicit at the call site and are otherwise unused here.
func resolveAcceptMode(acceptExisting, defaults, assumeYes bool, d DetectedSettings) (acceptMode, error) {
	if acceptExisting {
		if !d.Existing {
			return acceptReviewEach, fmt.Errorf("--accept-existing: no existing install detected on this cluster; run `oap init` without it to configure one")
		}
		return acceptAllExisting, nil
	}
	return acceptReviewEach, nil
}

// seedStateFromDetected writes State for every GENUINELY DETECTED setting in
// d, so the matching downstream screen's Prepare sees its key answered
// (State.Has) and returns a nil group — the rail flies past it as already
// done. An undetected field (zero value) is deliberately left unseeded so
// its screen still runs and prompts: seeding a zero value here would look
// identical to "detected as empty" and silently short-circuit a screen that
// has nothing to go on, defeating the "prompt for the rest" half of spec §3.
//
// The trusted/sandbox hostnames are the one pair ALSO gated on r, not just on
// detection: under r.manualWebdRouting, newRoutingScreens (wizard_screens.go)
// presents no routing screens at all — the operator is wiring routing
// themselves — so seeding a detected trusted hostname here would still reach
// applyRoutingAnswers (nothing ever clears a seeded State key) and fold
// straight into WebdRoutingOpts, manufacturing RunInstall's post-build
// "--trusted-hostname and --manual-webd-routing are mutually exclusive"
// refusal (routing_validate.go) over a flag the operator never passed. The
// sandbox hostname is additionally gated on r.disableViewer for the same
// reason: that flag is the conscious choice to install without an artifact
// viewer, and validateWebdRouting refuses sandbox+disableViewer
// unconditionally too. Linear never hits this: askWebdRoutingInputs
// (routing_ask.go) skips entirely under either flag and never reads the
// cluster's routes to begin with.
//
// ExternalSpiceDB is seeded UNCONDITIONALLY, unlike every other field above.
// Detection positively determines the backend either way — the SpiceDB config
// ConfigMap is read either way (detectExternalSpiceDB, detect.go), so "not
// external" is not the same absence-of-evidence as an unset ACME email or a
// never-created IdP CR. Seeding only the true case left a bundled cluster's
// key unset, so accept-all's SpiceDB screen still presented (the seed guard
// below, State.Has, never fired) even though there was nothing left to ask —
// the common case, since bundled is the default backend. Seeding false keeps
// the screen from asking there too, via the same short-circuit; seeding true
// keeps the SpiceDB screen from asking under accept-all AND — via the
// wizard's fold-point guard (refuseExternalToBundledFlip) — stops a
// fabricated bundled default from silently overwriting the endpoint config
// and orphaning the external instance.
func seedStateFromDetected(st *tui.State, d DetectedSettings, r WebdRoutingOpts) {
	if d.ACMEEmail != "" {
		st.Set(flagACMEEmail, d.ACMEEmail)
	}
	if d.TrustedHostname != "" && !r.manualWebdRouting {
		st.Set(flagTrustedHostname, d.TrustedHostname)
	}
	if d.SandboxHostname != "" && !r.manualWebdRouting && !r.disableViewer {
		st.Set(flagSandboxHostname, d.SandboxHostname)
	}
	if d.WorkspaceClass != "" {
		st.Set(keyWorkspaceClass, d.WorkspaceClass)
	}
	if d.IdPKind != "" {
		st.Set(keyIdP, d.IdPKind)
	}
	if d.MonitoringChannel != "" {
		st.Set(keyMonitoring, monitoringKeepValue)
	}
	st.SetBool(keyExternalSpiceDB, d.ExternalSpiceDB)
}
