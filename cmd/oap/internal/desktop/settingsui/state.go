// Package settingsui is the desktop settings server: a loopback-only,
// token-gated HTTP server that hosts the "settings" web UI app and exposes
// the desktop VM's runtime state over a small JSON/SSE API. It is
// platform-neutral (no darwin build tags, no import of the desktop command's
// own package) so it can be unit-tested without a VM, and is wired to a real
// desktop.Config/kube.Bundle by the darwin-only caller in a later task.
package settingsui

import (
	"context"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// State is the JSON shape served at GET /api/state and streamed over
// GET /api/events. It is a read-only snapshot of the desktop VM's lifecycle;
// Deps.State supplies the current value on every poll.
type State struct {
	// Running reports whether the desktop cluster is up and reachable.
	Running bool `json:"running"`
	// Phase is one of "stopped", "starting", "running", "failed" — mirrors
	// the desktop orchestrator's own lifecycle phases (wired in Task 8 via a
	// desktopState method of the same name).
	Phase string `json:"phase"`
	// Step is a short human-readable description of the current step within
	// Phase (e.g. "waiting for SpiceDB"), empty when there's nothing more
	// specific to say than the phase itself.
	Step string `json:"step,omitempty"`
	// KubectlCurrent reports whether the desktop cluster is the current
	// kubectl context. Deps.State never sets this itself — it is overlaid by
	// the server (see Server.currentState) from Deps.KubectlCurrent, so a nil
	// seam and a seam that returns false are indistinguishable on the wire,
	// which is the correct behavior: neither one means "pointing at this
	// cluster".
	KubectlCurrent bool `json:"kubectlCurrent"`
}

// ReapplyScope names which field groups of a PUT /api/config submission an
// individual ReapplyConfig call should push to the running cluster. It is what
// keeps a Model-only save from re-running the password-IdP provisioning (and
// vice-versa), and — critically — what keeps either from re-running the full
// security-defaults wizard, whose atomic model-catalog apply would wipe the
// operator's other cluster-tab edits (see run_darwin's targeted reapplyConfig).
type ReapplyScope struct {
	// Model requests re-applying the cluster default model: the central token
	// Secret and the default catalog entry's provider/name.
	Model bool
	// Password requests re-provisioning the local admin's password sign-in.
	Password bool
}

// Deps are the settings server's collaborators. New validates the seams a
// request path dereferences UNCONDITIONALLY — State, Logf, SupportDir, and
// Clients (the cluster routes call Clients with no nil guard) — failing closed
// at construction. The rest are optional and nil-guarded at their own call
// sites, each per the field's own doc: a nil ReapplyConfig or
// OnHealthNotificationsSaved makes the change report next-start/failed rather
// than silently succeeding; a nil KubectlUse/RevealConfig/RevealLogs is a 501;
// a nil KubectlCurrent overlays false.
type Deps struct {
	// SupportDir is the desktop app's per-user support directory (holds
	// config.json, logs, etc.) — required so the server can locate the same
	// on-disk state the CLI/menu-bar app read from.
	SupportDir string

	// State returns the current desktop lifecycle snapshot. Wired in Task 8
	// to a desktopState() method that reads the live orchestrator state.
	State func() State

	// Clients returns the live kube.Bundle (clients for the desktop cluster),
	// erroring while the cluster is down/unreachable. Required (validated by
	// New): the cluster routes — handleClusterSettingsGet/Put, install-info —
	// call it with no nil guard and treat its error as the "cluster not
	// running" state, so a nil seam would panic on the first such request.
	Clients func() (*kube.Bundle, error)

	// ReapplyConfig re-applies the parts of a validated desktop.Config named by
	// scope to the running cluster. handleConfigPut calls it AT MOST ONCE per
	// PUT, only for the field groups that actually changed (model and/or
	// password), so the re-apply touches exactly those cluster resources and
	// leaves every other cluster-tier setting — the model catalog's other
	// entries, the pinning ceiling, the content inspectors — untouched. A nil
	// scope (both false) is never passed; handleConfigPut only calls this when
	// at least one of the two changed.
	ReapplyConfig func(ctx context.Context, cfg desktop.Config, scope ReapplyScope) error

	// OnHealthNotificationsSaved is invoked when the health-notifications
	// toggle is saved from the settings UI. Wired in Task 8; unused by this
	// task's routes.
	OnHealthNotificationsSaved func(enabled bool)

	// KubectlCurrent reports whether the desktop cluster is the current
	// kubectl context. Wired in Task 8; read by Server.currentState to
	// overlay State.KubectlCurrent on every GET /api/state and SSE frame. A
	// nil seam overlays false rather than panicking.
	KubectlCurrent func() bool

	// KubectlUse switches the current kubectl context to the desktop cluster.
	// Invoked by POST /api/kubectl/use. Fire-and-forget: the darwin wiring
	// returns nil once it has KICKED OFF the switch and surfaces any real
	// failure through a desktop notification, not through this error — so the
	// HTTP layer treats a nil return as accepted-not-verified (204), and a
	// non-nil return (a nil seam is a 501) as the only thing worth reporting
	// back inline.
	KubectlUse func() error

	// RevealConfig opens the platform file browser on the config file.
	// Wired in Task 8; invoked by POST /api/reveal/config.
	RevealConfig func()

	// RevealLogs opens the platform file browser on the log directory.
	// Wired in Task 8; invoked by POST /api/reveal/logs.
	RevealLogs func()

	// AppVersion is the oap build's own version string, echoed verbatim by
	// GET /api/cluster/install-info. There is no exported "oap's own
	// version" helper reachable from this package today — see
	// installinfo.go's doc — so this field is the seam: the wiring task
	// supplies whatever the CLI binary considers its version (e.g. from
	// runtime/debug.BuildInfo, mirroring cmd/oap/internal/sessioncmd's
	// unexported oapVersion). Left "" here, it renders as an empty string,
	// never a placeholder value.
	AppVersion string

	// Logf logs a formatted line (mirrors log.Printf's signature) — used for
	// best-effort diagnostics that don't warrant failing a request (e.g. a
	// render error already reported to the caller as a 500).
	Logf func(format string, args ...any)
}
