//go:build darwin && arm64

package desktopcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/demo"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons/assets"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/settingsui"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/setupui"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/vz"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/installcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/modeltoken"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/portforward"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingswizard"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
	xbrowser "github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// menuIconInterval is the per-frame dwell for animated menu-bar states
// (Setup / ShuttingDown / Uninstalling). 12 frames × 120ms ≈ 1.4s per loop.
const menuIconInterval = 120 * time.Millisecond

// rootfsImageName is the k3s-baked raw disk image shipped in the .app's
// Contents/Resources (see mage desktop:rootfs / desktop:app, Phase 2/6 of
// the desktop bundle plan). It is staged into the writable Application
// Support directory on first run — see ensureDiskImage.
const rootfsImageName = "rootfs.img"

// rootfsStagedFromMarker sits next to the staged rootfs.img in the support
// directory and records the identity (bundledRootfsIdentity) of the .app's
// bundled rootfs that produced the current staged copy. ensureDiskImage
// compares the CURRENTLY-bundled rootfs's identity against this marker to
// decide whether a relaunched/rebuilt .app needs a re-stage — see
// shouldRestageDiskImage for why the staged disk's own mtime can't be used.
const rootfsStagedFromMarker = "rootfs.staged-from"

// bundledRootfsIdentity fingerprints the .app's bundled rootfs.img by size +
// modtime. The bundle's copy is written once at build time and never touched
// again, so its mtime is stable — unlike the staged copy, which is the live
// VM's writable disk whose mtime advances on every VM write. Recording THIS
// (not the staged copy's stats) in rootfsStagedFromMarker at stage time, and
// comparing it on the next launch, is what makes re-stage detection survive a
// running VM.
func bundledRootfsIdentity(bundled os.FileInfo) string {
	return fmt.Sprintf("size=%d mtime=%d", bundled.Size(), bundled.ModTime().UnixNano())
}

// hostShareTag is the virtio-fs mount tag the guest's fstab (Phase 2/4) uses
// to mount the persistent-memory host share at /var/lib/ap.
const hostShareTag = "apmem"

// sshKeyResourceName is the PRIVATE half of the ed25519 keypair `mage
// desktop:rootfs` bakes fresh into every build (desktopSSHKeyName in
// magefiles/desktop.go — MUST match; the two can't share a Go constant
// because this file and the mage target compile under different build
// tags). Desktop.App stages it at Contents/Resources/ap-vm-key, mode 0600;
// the app reads it directly from there (buildVMConfig/SSHKeyPath points
// straight at resourcesDir). Additionally, ensureVMKey stages a copy into
// the writable support dir so a standalone oap outside the .app bundle can
// auto-discover it during image reconcile — the app itself never reads that
// staged copy.
// See cmd/oap/internal/desktop/vz/provider_darwin.go's Kubeconfig, which
// uses it to fetch the k3s kubeconfig over SSH.
const sshKeyResourceName = desktop.KeyFileName

// desktopWebdPortBase is the starting point desktop.PickStablePort scans
// from to choose the local loopback port webd is reached through (both the
// port-forward in openDashboard AND the spicebox-webd-external-url
// ConfigMap patched in configureHook — see webdPort's doc on desktopState
// for why the two MUST agree). A fixed base means an ordinary relaunch with
// nothing else bound to it picks the SAME port every time, so the
// ConfigMap value stays valid across restarts instead of drifting to a
// fresh random port each launch.
const desktopWebdPortBase = 17080

// kubeconfigFileName / modelTokenFileName are staged under the Application
// Support directory so the in-process runInstall/runSettingsWizard calls
// (which read a kubeconfig PATH, not bytes — see kube.ClientOpts) have
// something to point at.
const (
	kubeconfigFileName = "kubeconfig"
	modelTokenFileName = "model-token" // 0600, overwritten each Configure run
)

// kubectlCheckmarkInterval is how often watchKubectlContext polls the user's
// kubeconfig to keep the "Use oap cluster for kubectl" menu item's checkmark
// in sync with the outside world (e.g. the user running `kubectl config
// use-context` themselves). The file is tiny, so polling it this often is
// cheap; it's only read, never held open.
const kubectlCheckmarkInterval = 5 * time.Second

// maxSessionMenuItems bounds the "Active sessions" submenu. systray's menu
// structure is fixed at build time — items can be shown/hidden/retitled but
// not added/removed at runtime — so onReady pre-creates exactly this many
// submenu slots (all hidden) and refreshSessions rewires the live ones (see
// applySessions). Ten is plenty for a single-user local desktop; extra live
// sessions beyond the cap simply don't appear in the menu (the full list is
// always reachable in the web chat / admin console).
const maxSessionMenuItems = 10

// sessionRefreshInterval is how often watchSessions re-lists the live chat
// AgentSessions to keep the "Active sessions" submenu current while the
// cluster is running. A single namespaced List with a label selector is
// cheap; this cadence trades a little staleness for negligible load.
const sessionRefreshInterval = 12 * time.Second

// sessionRefreshTimeout bounds a single refreshSessions List so a wedged
// apiserver can never hang the refresh goroutine (which would then miss every
// subsequent tick) — a timed-out list is logged and the last-known submenu is
// left in place (see refreshSessions).
const sessionRefreshTimeout = 10 * time.Second

// runDesktop is the darwin/arm64 entry point for `oap desktop`. It resolves
// the local config, starts the setupui HTTP server + Timeline and spawns
// the native setup window (a child process — see spawnSetupWindow) either
// showing the first-run config form (no valid config yet) or the live
// bring-up timeline (config already valid, bring-up starts immediately),
// then hands the process's main thread to fyne.io/systray for the life of
// the menubar app.
//
// g (the root command's --namespace/--kubeconfig/--context/--no-color flags)
// is accepted for signature parity with desktop_stub.go's runDesktop, but
// deliberately NOT propagated into the install/configure hooks below: the
// desktop app always targets its own private local VM cluster via a
// kubeconfig staged from the VM itself (see writeKubeconfig), never whatever
// cluster the user's ambient --kubeconfig/--context might point at.
func runDesktop(ctx context.Context, out io.Writer, _ *apcmd.Globals) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	support, err := desktopSupportDir()
	if err != nil {
		return fmt.Errorf("desktop: resolve Application Support directory: %w", err)
	}

	resources, err := desktopResourcesDir()
	if err != nil {
		return fmt.Errorf("desktop: resolve bundle Resources directory: %w", err)
	}

	logFile, err := openDesktopLogFile(support)
	if err != nil {
		return fmt.Errorf("desktop: open log file: %w", err)
	}
	defer logFile.Close()

	st := &desktopState{
		ctx:       ctx,
		out:       out,
		logFile:   logFile,
		support:   support,
		resources: resources,
	}

	// Load the on-disk config, if any. A missing or invalid config is no
	// longer a hard failure that exits the process (that was the old
	// Terminal-only first-run flow): the setup window spawned below shows
	// the config form instead, and bring-up starts once the user submits it
	// via onConfigSubmit — see the config-gate comment on startBringUp.
	cfg, loadErr := desktop.Load(support)
	if loadErr == nil {
		if verr := cfg.Validate(); verr != nil {
			loadErr = verr
		}
	}
	st.needsConfigAtStart = loadErr != nil
	if st.needsConfigAtStart {
		st.logf("desktop: no valid configuration at %s/config.json (%v); opening setup window", support, loadErr)
		cfg = desktop.Config{}
	}
	st.initialConfig = cfg

	st.eng = st.newEngine()

	// --- setup UI Timeline ---
	//
	// The Timeline is seeded up front (not lazily on first Progress call) so
	// /progress and /events have a valid, fully-pending snapshot to hand the
	// setup page the instant it loads, before bring-up has even started. It is
	// created BEFORE the orphan-cleanup goroutine below so that cleanup can
	// surface live sub-status ("Checking for existing VM…", …) onto the
	// "Initializing" row via Timeline.Detail — the row that bringUp marks active
	// up front (see bringUp / bringUpStepNames).
	st.timeline = setupui.NewTimeline(bringUpStepNames(cfg))
	st.timeline.SetNeedsConfig(st.needsConfigAtStart)

	// Find + terminate any orphaned VM process left behind by a prior crashed
	// `oap desktop` run before provisioning a new one — see killOrphanVMs's doc
	// comment for the identification rule and the Docker-VM guard it's built
	// around (a false positive here has crashed Docker Desktop in development;
	// see shouldKillOrphan). It shells out to a full-system `lsof` scan (bounded
	// by orphanLSOFTimeout), far too slow to block the menubar icon on, so run
	// it OFF the startup critical path: the icon comes up and starts animating
	// immediately, and bringUp waits on orphanCleanupDone before provisioning a
	// new VM (so a leftover VM is still killed first). Its progress is surfaced
	// as sub-status on the "Initializing" timeline row — the step is still
	// pending here (bringUp begins it), so Detail is stored and shown once active.
	st.orphanCleanupDone = make(chan struct{})
	timeline := st.timeline // captured for the cleanup goroutine's status callback
	go func() {
		killOrphanVMs(ctx, filepath.Join(support, rootfsImageName), st.logf, func(status string) {
			timeline.Detail("Initializing", status)
		})
		close(st.orphanCleanupDone)
	}()

	srv := setupui.New(st.timeline, st.onConfigSubmit)
	srv.SetOnOpenDashboard(func() error {
		st.openDashboard()
		return nil
	})
	if err := srv.Start(0); err != nil {
		return fmt.Errorf("desktop: start setup UI server: %w", err)
	}
	st.setupSrv = srv
	go func() {
		select {
		case serveErr, ok := <-srv.Errors():
			if ok {
				st.logf("setupui: server error: %v", serveErr)
			}
		case <-ctx.Done():
		}
	}()

	// The setup window is a SEPARATE CHILD PROCESS re-invoking this same `oap`
	// binary as the hidden `desktop-window` subcommand: systray (this
	// process's menubar) and webview (the child's native window) both need
	// to own their process's main thread and cannot share one. A failure to
	// spawn it is surfaced (log + notification) but not fatal — the setupui
	// HTTP server still works from a plain browser tab, and bring-up still
	// proceeds normally once a valid config exists.
	if err := st.spawnSetupWindow(srv.URL()); err != nil {
		st.logf("setupui: spawn setup window: %v", err)
		notify("OAP Desktop — setup", fmt.Sprintf("Could not open the setup window (%v). Configure OAP Desktop by writing %s directly, then relaunch.", err, filepath.Join(support, "config.json")))
	}
	defer st.stopSetupUI()
	// settingsSrv/settingsWindowCmd don't exist yet at this point (openSettings
	// starts them lazily on first click), but this defer is the catch-all for
	// every path OUT of runDesktop — not just the quit()/onExit() paths that
	// already call it — so it belongs here too; stopSettingsUIOnce makes the
	// no-op case (never opened) and the redundant-call case both safe.
	defer st.stopSettingsUI()

	// Ensure the VM is stopped on every path OUT of runDesktop, not just the
	// menubar Quit / SIGINT path below (st.quit(), which already calls
	// eng.Down with its own bounded context): a hard bring-up failure
	// (failBringUp) leaves the menubar up in an Error state WITHOUT ever
	// calling eng.Down, so if this process then exits some other way (the
	// error branches below, a future early return, the VM's own operator
	// force-quitting the app while it's sitting in Error state) without a
	// user-driven Quit, the VM — a separate macOS-owned
	// Virtualization.framework process, not just a goroutine — would
	// otherwise be orphaned exactly like killOrphanVMs above exists to clean
	// up. eng.Down (ultimately vz.Provider.Stop) is idempotent and silent on
	// a repeat call once the VM is already stopped, so this is safe to run
	// even when st.quit() already stopped the VM earlier in this same exit.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), quitShutdownTimeout)
		defer cancel()
		if err := st.eng.Down(shutdownCtx); err != nil {
			st.logf("runDesktop: stop-on-exit failed: %v", err)
		}
	}()

	// Quit cleanly on SIGINT/SIGTERM too (e.g. `oap desktop` run from a
	// Terminal and Ctrl-C'd) — systray only reacts to menu-driven Quit()
	// otherwise. Route through st.quit() (the SAME path as menu-driven Quit)
	// rather than calling systray.Quit() directly, so a Ctrl-C also stops the
	// VM gracefully (systray.Quit() alone only runs onExit, which stops the
	// port-forwarder but never calls eng.Down).
	go func() {
		<-ctx.Done()
		st.quit()
	}()

	systray.Run(st.onReady, st.onExit)

	st.mu.Lock()
	defer st.mu.Unlock()
	return st.runErr
}

// desktopState carries everything the systray callbacks + bring-up loop need.
// mu guards every field that's read/written from more than one goroutine
// (menu item click handlers, the bring-up loop, and onExit all run
// concurrently with systray's own event-dispatch goroutine).
type desktopState struct {
	ctx       context.Context
	out       io.Writer
	logFile   *os.File
	support   string
	resources string
	eng       *desktop.Engine

	// initialConfig is the config loaded from disk at process start; valid
	// (usable) only when needsConfigAtStart is false. onReady starts
	// bring-up with it directly when startup already had a valid config; the
	// alternate path (first-run / invalid config) starts bring-up from
	// onConfigSubmit instead, with the just-submitted config. Neither field
	// is mutated after runDesktop hands off to systray.Run, so both are safe
	// to read from onReady without s.mu.
	initialConfig      desktop.Config
	needsConfigAtStart bool
	bringUpOnce        sync.Once

	// orphanCleanupDone is closed when the background orphan-VM cleanup (a slow
	// full-system lsof scan, run off the startup critical path so it doesn't
	// delay the menubar icon) finishes. bringUp waits on it before provisioning
	// a new VM. Set once in runDesktop before systray.Run.
	orphanCleanupDone chan struct{}

	// timeline + setupSrv + windowCmd back the native setup UI (config form
	// + live bring-up timeline): see setupui.Timeline/Server and
	// spawnSetupWindow/stopSetupUI below. Set once in runDesktop before
	// systray.Run and never reassigned, so — like initialConfig above —
	// they're safe to read from any goroutine without s.mu.
	timeline        *setupui.Timeline
	setupSrv        *setupui.Server
	windowCmd       *exec.Cmd
	stopSetupUIOnce sync.Once

	// settingsSrv + settingsWindowCmd back the native settings UI window —
	// same shape as setupSrv/windowCmd above, but LAZILY started (on the
	// first "Settings…" click, not before systray.Run) and re-creatable
	// across the process's lifetime (closing the window and clicking
	// "Settings…" again), so — unlike setupSrv/windowCmd, which are set
	// once and never reassigned — these need their own mutex rather than
	// being safe to read from any goroutine unguarded. settingsMu guards
	// both fields plus the liveness check openSettings uses to decide
	// "start a new window" vs. "re-front the existing one". settingsItem
	// (the "Settings…" menu item itself) is set once in onReady like the
	// other systray.MenuItem fields below, so it lives with those instead.
	settingsSrv        *settingsui.Server
	settingsWindowCmd  *exec.Cmd
	settingsMu         sync.Mutex
	stopSettingsUIOnce sync.Once

	mu          sync.Mutex
	dg          *apcmd.Globals // Kubeconfig-bearing apcmd.Globals for the running VM cluster; set once Up succeeds
	pf          *portforward.PortForwarder
	runErr      error  // set by failBringUp on a hard bring-up failure; read by runDesktop after systray.Run returns
	currentStep string // last step name passed to progressHook; names timeline.Fail() on a hard bring-up failure

	// webdPort is the stable loopback port chosen (once, via
	// desktop.PickStablePort) at the start of bringUp, BEFORE Install/Configure
	// run. It is used for two things that MUST agree: configureHook patches
	// the spicebox-webd-external-url ConfigMap's trusted-url/sandbox-url keys
	// to http://127.0.0.1:<webdPort>, and openDashboard's port-forward binds
	// its LOCAL side to that exact same port. webd rejects any request whose
	// Host doesn't match its configured base URL (a 404, not an error page —
	// see the task's root-cause note), so a mismatch between the two would
	// silently break the dashboard again. Zero means PickStablePort failed
	// (surfaced via s.fail at bring-up time); both configureHook and
	// openDashboard treat zero as "no usable port" and fail loudly rather than
	// falling back to a random port that would only reintroduce the mismatch.
	webdPort int

	// running tracks whether bring-up has completed and the cluster is up
	// (set true at the end of bringUp, false in stopCluster). watchSessions
	// only lists AgentSessions while this is true; the "Active sessions"
	// parent item's enabled state mirrors it.
	running bool

	// sessionPool is the bounded set of "Active sessions" submenu items,
	// pre-created (all hidden) in onReady and never resized — refreshSessions
	// shows/hides + retitles them (see maxSessionMenuItems / applySessions).
	// sessionEmptyItem is the single disabled "No active sessions" line shown
	// in their place when there are zero live sessions. sessionRefs maps each
	// pool slot to the "<namespace>/<name>" ref its click should open — the
	// form /sessions takes in its ?session= query, so the namespace must be
	// carried here and not re-derived at click time; it is always
	// len(sessionPool) long (unused slots hold ""), rewritten under mu on every
	// refresh, and read under mu by each slot's click handler.
	sessionPool      []*systray.MenuItem
	sessionEmptyItem *systray.MenuItem
	sessionRefs      []string

	// health submenu: a bounded pool of disabled per-component lines (pre-created
	// hidden in onReady, shown/retitled by applyHealth), the "Notify on health
	// issues" checkbox, and the "View full health…" entry. healthUnhealthy is
	// the last poll's REQUIRED-unhealthy set (name->detail), guarded by mu, used
	// to fire transition-only notifications (see refreshHealth/healthDiff).
	healthPool       []*systray.MenuItem
	healthNotifyItem *systray.MenuItem
	healthViewItem   *systray.MenuItem
	healthUnhealthy  map[string]string

	statusItem       *systray.MenuItem
	iconAnim         *menubaricons.Animator
	sessionsItem     *systray.MenuItem
	newChatItem      *systray.MenuItem
	dashboardItem    *systray.MenuItem
	installAgentItem *systray.MenuItem
	kubectlItem      *systray.MenuItem
	stopItem         *systray.MenuItem
	settingsItem     *systray.MenuItem
	viewLogsItem     *systray.MenuItem
	uninstallItem    *systray.MenuItem
	quitItem         *systray.MenuItem
}

// desktopSupportDir is ~/Library/Application Support/oap — the writable
// per-user directory holding config.json, the staged rootfs disk image, the
// EFI variable store, the persistent-memory virtiofs share, the staged
// kubeconfig, and logs.
func desktopSupportDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "oap"), nil
}

// desktopResourcesDir locates the running .app bundle's Contents/Resources
// directory (Contents/MacOS/oap -> ../Resources), where the Phase 6 `mage
// desktop:app` assembly places the baked rootfs image. Outside a bundle (a
// plain `go build ./cmd/oap` during development) this still returns a path —
// ensureDiskImage fails closed with an actionable error if nothing is there,
// rather than this function guessing.
func desktopResourcesDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	macOSDir := filepath.Dir(exe) // .../Contents/MacOS in a packaged .app
	return filepath.Join(filepath.Dir(macOSDir), "Resources"), nil
}

// shouldRestageDiskImage is the pure decision behind ensureDiskImage: given
// whether a staged copy already exists, the identity marker recorded when it
// was staged (rootfsStagedFromMarker; "" if none), and the os.Stat of the
// currently-bundled rootfs in Contents/Resources, should the staged copy be
// overwritten?
//
// Re-stage when there is no staged copy yet, or when the bundled rootfs's
// identity (bundledRootfsIdentity) differs from the marker recorded at stage
// time — that means a rebuilt/updated .app (`mage desktop:app`) shipped a
// different rootfs.img (and, since each build mints a fresh SSH key, a
// different guest key) than what's staged. An empty marker — e.g. a copy
// staged before this marker existed — also forces one re-stage. Otherwise
// reuse the staged copy, preserving the running VM's disk state (installed
// cluster, persistent memory, etc.) across ordinary relaunches.
//
// We compare against the BUNDLED rootfs (via the marker), NOT the staged
// disk's own size/mtime: the staged disk is the live VM's writable disk, so
// its mtime advances on every VM write and is essentially always newer than
// the bundled rootfs. The previous mtime comparison therefore NEVER re-staged
// a rebuilt .app — it silently kept booting the stale image, which (per fresh
// per-build keys) made guest SSH fail after every rebuild.
//
// bundled == nil (the bundled copy is missing entirely) never triggers a
// re-stage here — that is a hard error the caller's os.Open surfaces with an
// actionable message, not a silent "keep the old one."
func shouldRestageDiskImage(stagedExists bool, stagedMarker string, bundled os.FileInfo) bool {
	if !stagedExists {
		return true
	}
	if bundled == nil {
		return false
	}
	return stagedMarker != bundledRootfsIdentity(bundled)
}

// ensureDiskImage stages the bundled rootfs image into the writable support
// directory: on first run, or whenever a rebuilt .app's bundled rootfs.img
// differs from the already-staged copy (see shouldRestageDiskImage) — a
// rebuilt bundle whose rootfs.img was reused unconditionally would silently
// keep booting the STALE staged image, which cost several manual test
// cycles before this check existed. Reuse (no copy) only when the staged
// and bundled copies already match, so a plain relaunch keeps the running
// VM's disk state.
func ensureDiskImage(resourcesDir, supportDir string) (string, error) {
	dst := filepath.Join(supportDir, rootfsImageName)
	src := filepath.Join(resourcesDir, rootfsImageName)
	markerPath := filepath.Join(supportDir, rootfsStagedFromMarker)

	stagedExists := false
	if _, err := os.Stat(dst); err == nil {
		stagedExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat staged rootfs image %q: %w", dst, err)
	}

	var bundledInfo os.FileInfo
	if fi, err := os.Stat(src); err == nil {
		bundledInfo = fi
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat bundled rootfs image %q: %w", src, err)
	}

	// Best-effort marker read: a missing/unreadable marker (a legacy staged
	// copy, or an interrupted prior stage) reads as "" and forces one re-stage,
	// which is safe.
	stagedMarker := ""
	if b, err := os.ReadFile(markerPath); err == nil {
		stagedMarker = strings.TrimSpace(string(b))
	}

	if !shouldRestageDiskImage(stagedExists, stagedMarker, bundledInfo) {
		return dst, nil
	}

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open bundled rootfs image %q (build the .app via `mage desktop:app` first): %w", src, err)
	}
	defer in.Close()

	if err := os.MkdirAll(supportDir, 0o700); err != nil {
		return "", fmt.Errorf("create %q: %w", supportDir, err)
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("stage rootfs image: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("stage rootfs image: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("stage rootfs image: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("stage rootfs image: %w", err)
	}
	// Record the identity of the bundled rootfs we just staged, so the NEXT
	// launch can distinguish a rebuilt/updated .app from an ordinary relaunch
	// (the staged disk's own stats can't — see shouldRestageDiskImage).
	// bundledInfo is non-nil on any path reaching here: a nil bundled means the
	// os.Open(src) above already failed. Surfaced loudly rather than dropped —
	// an unrecorded stage would re-copy the 10GB image every launch.
	if bundledInfo != nil {
		if err := os.WriteFile(markerPath, []byte(bundledRootfsIdentity(bundledInfo)), 0o600); err != nil {
			return "", fmt.Errorf("record staged rootfs marker %q: %w", markerPath, err)
		}
	}
	return dst, nil
}

// ensureVMKey stages the bundled VM SSH private key (resourcesDir/ap-vm-key)
// into the writable support dir (supportDir/ap-vm-key, 0600) so a *standalone*
// `oap` — one not running from inside the .app bundle — can auto-discover it via
// desktop.VMKeyPath()'s support-dir candidate during image reconcile against the
// oap-desktop context. The app itself does NOT read this staged copy (buildVMConfig
// points SSHKeyPath straight at resourcesDir); this exists purely for the external
// CLI. Idempotent: a byte-identical existing copy is left alone; a missing or
// rotated copy is (re)written atomically, so a .app rebuilt with a new keypair
// self-heals on next launch. Unlike ensureDiskImage (a 10 GB image guarded by a
// restage marker) the key is tiny, so a plain content compare is enough here.
func ensureVMKey(resourcesDir, supportDir string) error {
	src := filepath.Join(resourcesDir, sshKeyResourceName)
	dst := filepath.Join(supportDir, sshKeyResourceName)

	want, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read bundled VM SSH key %q: %w", src, err)
	}

	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, want) {
		return nil // already staged and current
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read staged VM SSH key %q: %w", dst, err)
	}

	if err := atomicWriteFile(dst, want, 0o600); err != nil {
		return fmt.Errorf("stage VM SSH key: %w", err)
	}
	return nil
}

// buildVMConfig assembles the desktop.VMConfig for the local VM: the staged
// rootfs disk, an EFI store + persistent-memory share under supportDir
// (created on demand by the vz.Provider / guest fstab), the bundled SSH key
// (read straight out of resourcesDir — see sshKeyResourceName), and a fixed
// CPU/memory budget. TODO: make CPU/memory user-configurable (first-run
// config has no such knob today).
func buildVMConfig(diskImage, supportDir, resourcesDir string) desktop.VMConfig {
	return desktop.VMConfig{
		DiskImagePath: diskImage,
		EFIStorePath:  filepath.Join(supportDir, "efi", "efivars"),
		CPUCount:      4,
		MemoryBytes:   4 << 30, // 4 GiB
		HostShareDir:  filepath.Join(supportDir, "memory"),
		HostShareTag:  hostShareTag,
		SSHKeyPath:    filepath.Join(resourcesDir, sshKeyResourceName),
	}
}

// newEngine constructs the desktop.Engine: a vz.Provider over a
// VMConfig built from the (as yet unstaged) rootfs image, wired to
// EngineHooks that call the SAME in-process runInstall / runSettingsWizard
// code paths `oap install` / `oap settings wizard --defaults` use — no
// separate "desktop install" implementation to drift from the CLI's.
func (s *desktopState) newEngine() *desktop.Engine {
	// The disk image is staged lazily (on first Up, not at construction) so
	// engine construction itself never touches the filesystem beyond reading
	// config — see bringUp.
	vmCfg := buildVMConfig(filepath.Join(s.support, rootfsImageName), s.support, s.resources)
	provider := vz.New(vmCfg)
	hooks := desktop.EngineHooks{
		Install:   s.installHook,
		Configure: s.configureHook,
		InitLocal: s.initLocalHook,
		Progress:  s.progressHook,
	}
	return desktop.NewEngine(provider, hooks)
}

// writeKubeconfig stages kc under supportDir/kubeconfig (0600, atomic
// rename) and returns a *apcmd.Globals pointed at it. kube.ClientOpts.Kubeconfig
// is a file path, not raw bytes (cmd/oap/internal/kube.New), so every
// in-process call into runInstall / runSettingsWizard needs this on disk.
// The desktop app's OWN Kubeconfig always targets the private local VM
// cluster — the outer *apcmd.Globals' --kubeconfig/--context flags (if any) are
// deliberately NOT propagated here.
func (s *desktopState) writeKubeconfig(kc []byte) (*apcmd.Globals, error) {
	path := filepath.Join(s.support, kubeconfigFileName)
	if err := atomicWriteFile(path, kc, 0o600); err != nil {
		return nil, fmt.Errorf("stage kubeconfig: %w", err)
	}
	dg := &apcmd.Globals{Kubeconfig: path}
	s.mu.Lock()
	s.dg = dg
	s.mu.Unlock()
	return dg, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// installHook adapts Engine.Up's Install step to installcmd.RunInstall,
// running DESKTOP semantics: passes cloud.MustFor(cloud.KeyDesktop), whose
// dev profile sets MEMORY_BACKEND=sqlite + injects the --allow-shared-origin
// webd debug flag (see setOperatorMemoryBackend in
// cmd/oap/internal/installcmd/install.go), no trusted/sandbox hostname
// (loopback-only VM), and --no-digest-pin (the desktop VM always runs the
// baked-in :dev images, never a remote registry).
// The desktop kind differs from `local` (--local's public ngrok tunnel) in
// exactly one respect — InstallProfile().ServesLocalWebChat() is true — and
// nothing reads that answer today; see pkg/platform/cloud/desktop for what that leaves
// open. `oap desktop` is the ONLY caller that selects cloud.KeyDesktop.
// runInstall's own non-interactive guards (isInteractive checks os.Stdin;
// this process runs detached from a TTY) mean confirmPlan and friends
// proceed without prompting.
//
// env (the model API key / NGROK_AUTHTOKEN from cfg.EnvForInstall) is
// intentionally unused here: runInstall itself never consumes those — the
// model token flows through configureHook (staged to a file, passed via
// settingscmd.DefaultModelOpts.TokenFile) and NGROK_AUTHTOKEN would flow through
// initLocalHook once that's implemented.
//
// imagesPrebaked=true tells runInstall's preflight() to skip its `docker` on
// PATH check even though tags.Registry == "" would otherwise read as "local
// image build/load mode" — the desktop VM's k3s already has the :dev images
// baked in via the Phase 2 air-gapped rootfs image, so no host Docker is
// involved at all. Without this, a desktop user without Docker installed
// would fail preflight for a check this install path never needed.
func (s *desktopState) installHook(ctx context.Context, kubeconfig []byte, _ map[string]string) error {
	dg, err := s.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}
	// Wait for the guest's Kubernetes API to actually SERVE before installing.
	// The staged rootfs can persist a prior cluster, so the kubeconfig is
	// fetchable the instant k3s writes it — but the apiserver may still be
	// initializing, in which case install's very first call (cloud.Detect's
	// Nodes().List) fails with "...: starting". Poll it ready rather than
	// racing it (which non-deterministically failed bring-up).
	if err := s.waitClusterAPIReady(ctx, dg); err != nil {
		return err
	}
	// k3s is serving. Unlock ONLY the "Use oap cluster for kubectl" item now, so
	// the user can point kubectl at the VM and inspect the cluster even while
	// the rest of the install pipeline is still running — or stalled (e.g. the
	// pipeline's "PostgreSQL — agent memory store" gate, which the desktop
	// doesn't even use, running MEMORY_BACKEND=sqlite). Dashboard, install
	// agent, new chat, sessions, and status all need the control plane the
	// pipeline still has to bring up, so they stay disabled until bring-up
	// completes (enableRunningMenu).
	s.enableKubectlMenuItem()
	r := installcmd.UnattendedRoutingOpts()
	w := &installDetailWriter{underlying: s.logWriter(), timeline: s.timeline}
	// The desktop VM has no --builder-starters equivalent and no notion of
	// "the installer's own identity" to default one to, so it skips the
	// builder rather than guessing an identity that would silently lock the
	// class to nobody real (mirrors `oap init`'s same call — see there).
	return installcmd.RunInstall(ctx, ctx, installcmd.InstallConfig{
		Out:                 w,
		G:                   dg,
		Tags:                manifests.Tags{},
		DryRun:              "",
		Routing:             r,
		PinningMode:         "",
		Strat:               cloud.MustFor(cloud.KeyDesktop),
		Ext:                 installcmd.ExternalSpiceDB{}, // no external SpiceDB for `oap desktop`
		Workspace:           installcmd.WorkspaceResolveOptions{},
		Stateful:            installcmd.StatefulResolveOptions{},
		Artifact:            installcmd.ArtifactStoreOptions{},
		ImagePullSecret:     "",
		MirrorDeps:          false,
		ImagesPrebaked:      true,
		ClampUndersizedPVCs: false,
		WithoutBuilder:      true,
		BuilderStarters:     nil,
		Rail:                nil, // `oap desktop` installs unattended, no wizard rail
	})
}

// waitClusterAPIReady polls the guest's Kubernetes API until a Nodes().List
// succeeds (the same call install's cloud.Detect makes first), or the timeout
// elapses. See installHook for why this race exists. Surfaces the wait in the
// setup timeline and, on timeout, returns a loud error (never a silent hang).
func (s *desktopState) waitClusterAPIReady(ctx context.Context, dg *apcmd.Globals) error {
	b, err := dg.Bundle()
	if err != nil {
		return fmt.Errorf("build cluster client: %w", err)
	}
	const timeout = 3 * time.Minute
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		if _, e := b.Typed.CoreV1().Nodes().List(waitCtx, metav1.ListOptions{Limit: 1}); e == nil {
			return nil
		} else {
			lastErr = e
		}
		if s.timeline != nil {
			s.timeline.Detail("Installing components", "waiting for the Kubernetes API to start…")
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("kubernetes API not ready after %s: %w", timeout, lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

// waitCIdPValid polls the singleton ClusterIdentityProvider "default" until its
// Valid condition is True — the exact gate webd's in-process IdP loader
// consults before serving sign-in (see identityd.handlePasswordVerify's
// s.idp.Current). configureHook applies the CIdP but its controller stamps
// Valid asynchronously; without this wait bring-up reports "done" (enabling the
// dashboard menu item) while /admin still 500s for the reconcile's duration.
// Surfaced to the setup timeline so the wait is visible, not a silent stall.
func (s *desktopState) waitCIdPValid(ctx context.Context, c client.Client) error {
	const timeout = 2 * time.Minute
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var cidp spiceboxv1alpha1.ClusterIdentityProvider
		if err := c.Get(waitCtx, client.ObjectKey{Name: "default"}, &cidp); err == nil {
			for _, cond := range cidp.Status.Conditions {
				if cond.Type == spiceboxv1alpha1.ConditionIdPValid && cond.Status == metav1.ConditionTrue {
					return nil
				}
			}
		}
		if s.timeline != nil {
			s.timeline.Detail("Configuring cluster", "waiting for the sign-in provider to validate…")
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("cluster identity provider %q not Valid=True after %s (admin sign-in would 500 until it validates)", "default", timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

// installStepName MUST match the label Engine.Up's Progress hook passes for
// the install step (see orchestrate.go's Up: e.progress("Installing
// components")) — it is the exact Timeline step name installDetailWriter
// attaches Detail lines to; Timeline.Detail is a silent no-op for an
// unrecognized step name (see model.go), so a drift here would fail closed
// as "no detail line ever shows up," not a crash.
const installStepName = "Installing components"

// installDetailWriter tees runInstall's log output to the underlying
// writer (the log file + stdout, unchanged) AND promotes select
// "meaningful" lines into the setup UI's live timeline via
// timeline.Detail(installStepName, ...): lines containing "waiting for",
// "applied", or "ready" (case-insensitive). Latest matching line wins — no
// attempt to model runInstall's actual phases, which is intentional (see
// the task brief: "keep it simple").
type installDetailWriter struct {
	underlying io.Writer
	timeline   *setupui.Timeline
	buf        []byte
}

func (w *installDetailWriter) Write(p []byte) (int, error) {
	n, err := w.underlying.Write(p)
	if err != nil {
		return n, err
	}
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		if isMeaningfulInstallLine(line) {
			w.timeline.Detail(installStepName, strings.TrimSpace(line))
		}
	}
	return n, nil
}

// isMeaningfulInstallLine reports whether line looks like a progress
// update worth surfacing as the install step's live Detail line.
func isMeaningfulInstallLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "waiting for") || strings.Contains(lower, "applied") || strings.Contains(lower, "ready")
}

// configureHook registers the cluster-default model — the provider the user
// picked at first-run setup, and cfg.EffectiveModel() (their explicit model,
// or that provider's recommended one) — by reusing the SAME non-interactive
// path `oap settings wizard --defaults --default-model=...` runs
// (runSettingsWizard / applyDefaultModel / modeltoken.EnsureSecret); see
// cmd/oap/internal/settingscmd/wizard.go. cfg.Model.APIKey is staged to a
// 0600 file (rather than an env var) so modeltoken.Resolve picks it up via
// settingscmd.DefaultModelOpts.TokenFile without touching the process
// environment.
//
// --defaults composes the catalog from this one entry alone, and the CR's
// modelCatalog is an atomic list under a forced apply, so re-running with a
// different provider REPLACES the previous default rather than leaving a
// second one behind; the central token Secret is likewise rewritten in place
// under its fixed name. That is what keeps a provider switch from stranding a
// stale default model or a stale key.
//
// It also applies the bundled demo AgentClass (see applyDemoAgentClass) so
// the chat's agent selector has something to talk to the instant bring-up
// finishes; the built-in web chat's routes are already live by then, gated
// by AP_CLUSTER_KIND=local (stamped at install time) rather than by anything
// this hook does.
//
// TODO: apply cfg.Channel (external channel secret + Channel CR) — today
// cmd/oap/internal/desktop.knownExternalChannelKinds is empty (Slack is
// deliberately excluded from the personal-use flow and no other external
// kind exists yet), so cfg.Channel != nil is unreachable in practice and
// this hook never needs to handle it.
func (s *desktopState) configureHook(ctx context.Context, kubeconfig []byte, cfg desktop.Config) error {
	dg, err := s.writeKubeconfig(kubeconfig)
	if err != nil {
		return err
	}

	tokenPath := filepath.Join(s.support, modelTokenFileName)
	if err := atomicWriteFile(tokenPath, []byte(cfg.Model.APIKey), 0o600); err != nil {
		return fmt.Errorf("stage model token: %w", err)
	}
	defer os.Remove(tokenPath)

	model, err := cfg.EffectiveModel()
	if err != nil {
		return err
	}
	dm := settingscmd.DefaultModelOpts{
		Model:           model,
		Provider:        cfg.Model.Provider,
		TokenFile:       tokenPath,
		SecretName:      "model-default-token",
		SecretNamespace: "agentprimitives-system",
		SecretKey:       "token",
	}
	fakeCmd := &cobra.Command{}
	fakeCmd.SetOut(s.logWriter())
	if err := settingscmd.RunWizard(ctx, fakeCmd, dg, true /*defaults*/, false /*dryRun*/, "" /*registry*/, nil /*digests*/, dm); err != nil {
		return fmt.Errorf("register cluster default model: %w", err)
	}

	// Point webd's base URL at the SAME stable loopback port openDashboard's
	// port-forward will bind to (see webdPort's doc on desktopState). This
	// runs AFTER install has seeded the spicebox-webd-external-url ConfigMap
	// with its http://localhost:8080 default, so this is the update that
	// makes it match how the desktop app actually reaches webd — without it,
	// every request 404s (the root cause this whole feature exists to fix).
	// webd live-follows the ConfigMap (pkg/x/externalurl.Provider polls it), so
	// no webd restart is needed.
	//
	// A missing port is logged and skipped rather than failing Configure: the
	// dashboard shortcut just won't work, which is a lesser degradation than
	// the password-IdP provisioning below (a hard failure there would leave
	// the admin console entirely unreachable, not merely un-shortcut-able).
	s.mu.Lock()
	port := s.webdPort
	s.mu.Unlock()
	if port == 0 {
		s.logf("configureHook: no stable webd port was chosen; leaving %s ConfigMap untouched (dashboard will not work)", spiceboxv1alpha1.WebdExternalURLConfigMap)
	} else if err := s.setWebdExternalURL(ctx, dg, port); err != nil {
		return fmt.Errorf("point webd base URL at loopback port %d: %w", port, err)
	}

	// Provision the local admin's password sign-in (Secret + password-kind
	// ClusterIdentityProvider) and grant platform-admin, so the freshly
	// set password actually unlocks the admin console. cfg.AdminPasswordHash
	// is empty only for a config.json predating this field (see its doc) —
	// treat that as "nothing to provision" rather than an error so an
	// existing desktop install's Configure re-runs (e.g. after `oap settings
	// wizard`-style re-invocation) don't fail on old state.
	if cfg.AdminPasswordHash != "" {
		if err := s.provisionPasswordIdP(ctx, dg, cfg.AdminPasswordHash); err != nil {
			return fmt.Errorf("provision local admin password sign-in: %w", err)
		}
	}

	// Apply the demo AgentClass so the session dashboard has something to
	// start. webd's conversation routes mount on their own, from their own
	// prerequisites (NATS, SpiceDB, the operator memory URL), which this
	// install wires. A dashboard with no AgentClass to start is still a dead
	// end the same way a login with no platform-admin grant would be — see
	// provisionPasswordIdP's doc for the same reasoning applied there.
	b, err := dg.Bundle()
	if err != nil {
		return fmt.Errorf("connect to local cluster: %w", err)
	}
	if err := applyDemoAgentClass(ctx, b.Controller); err != nil {
		return fmt.Errorf("apply demo AgentClass: %w", err)
	}
	s.logf("configureHook: applied demo AgentClass %s/%s", demoChatNamespace, demoAgentClassName)

	if err := s.ensureWebhookTunnel(ctx, b.Controller); err != nil {
		// Logged and continued, like the missing-port case above. A desktop
		// with no tunnel is a desktop whose webhook channel does not deliver;
		// a desktop that refuses to finish Configure is one that does nothing
		// at all, and every other surface here works without a tunnel.
		s.logf("configureHook: could not ensure the public endpoint for an existing webhook channel: %v", err)
	}

	return nil
}

// ensureWebhookTunnel opens this desktop's tunnel when a Channel that receives
// inbound HTTP is already wired.
//
// `oap agent install` opens one for a channel it DECLARES, before its wizard
// registers the address. This is the other way a webhook channel comes to
// exist: `oap channel create --kind github` wires one directly, and a desktop
// that was set up that way — or one whose cluster was rebuilt — has a Channel
// with nowhere to receive. It is also what brings the tunnel back after the VM
// is recreated.
//
// It runs AFTER setWebdExternalURL, and the order is load-bearing: the endpoint
// takes its spec.localURL from the value that hook writes, which is the only
// place the runtime-chosen loopback port is recorded.
//
// The kind is asked, never named — registry.NeedsWebhook is the same
// WebhookReceiver seam webd mounts its own routes from.
func (s *desktopState) ensureWebhookTunnel(ctx context.Context, c client.Client) error {
	var chans spiceboxv1alpha1.ChannelList
	if err := c.List(ctx, &chans); err != nil {
		return fmt.Errorf("list Channels to see whether any receives webhooks: %w", err)
	}
	idx := slices.IndexFunc(chans.Items, func(ch spiceboxv1alpha1.Channel) bool {
		return registry.NeedsWebhook(ch.Spec.Kind)
	})
	if idx < 0 {
		return nil
	}

	s.logf("configureHook: Channel %s/%s (kind %s) receives webhooks; ensuring this cluster has a public address",
		chans.Items[idx].Namespace, chans.Items[idx].Name, chans.Items[idx].Spec.Kind)
	// The desktop does not WAIT for Ready: the controller brings the tunnel up
	// on its own, and blocking bring-up on a provider round-trip would leave
	// the menu bar in Setup over something nothing on screen is waiting for.
	// `oap agent install`, whose wizard cannot register an address that is not
	// live yet, is the caller that waits.
	_, err := publicendpoint.EnsureWebdOnDemand(ctx, s.logWriter(), c, cloud.MustFor(cloud.KeyDesktop), nil)
	return err
}

// desktopAdminEmail is the fixed local-admin identifier used both as the
// password-kind ClusterIdentityProvider's spec.clientID and as the subject
// granted platform-admin below. It MUST be the exact string the "password"
// idp.Kind falls back to as its default identity
// (pkg/platform/identity/idp/passwordkind's defaultLocalIdentity) — the two packages
// can't share a Go constant (this file only depends on the kind by name,
// like the CR-validity controller does), so this hook sets spec.clientID
// explicitly instead of relying on that default, and
// TestPlatformCanonicalForEmail_MatchesPasswordKindPrincipal in
// platform_test.go pins the two literals equal so this can never silently
// drift from the login path.
const desktopAdminEmail = "admin@ap.local"

// desktopAdminPasswordSecretName/Key name the Secret provisionPasswordIdP
// writes the bcrypt hash to. These MUST match `oap idp setup password`'s own
// "idp-password"/"client_secret" convention (see passwordWizardOutput in
// pkg/platform/identity/idp/passwordkind/wizard.go): both webd's RBAC allowlist
// (config/webd/role-system.yaml) and the RBAC sufficiency test
// (pkg/platform/manifests/rbac_sufficiency_test.go) are pinned to the wizard's secret
// name, and identityd has no other way to read this Secret than that
// allowlisted name. Using a different name here (as this hook once did)
// means identityd's direct client.Get 403s and /admin login 500s.
const (
	desktopAdminPasswordSecretName = "idp-password"
	desktopAdminPasswordSecretKey  = "client_secret"
)

// provisionPasswordIdP creates (or updates, on a Configure re-run) the
// Secret holding the local admin's bcrypt password hash, applies the
// singleton ClusterIdentityProvider "default" (kind=password) pointed at
// it, and grants the local admin platform-admin. All three steps are
// applied via the exact same helpers `oap idp setup` / `oap platform
// grant-admin` use (applySecret / applyClusterIdentityProvider /
// newAuthzClient+TouchPlatformAdmin) so this hook can never drift from
// their behavior.
//
// Every step here is a HARD failure (returned, never logged-and-continued):
// a partial provision — e.g. the CR applies but the platform-admin grant
// fails — would leave a login that authenticates successfully but can
// never pass an authz check, which is a more confusing failure mode for
// the user than Configure failing outright with a clear, retryable error.
func (s *desktopState) provisionPasswordIdP(ctx context.Context, dg *apcmd.Globals, hash string) error {
	b, err := dg.Bundle()
	if err != nil {
		return fmt.Errorf("connect to local cluster: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desktopAdminPasswordSecretName,
			Namespace: externalurl.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{desktopAdminPasswordSecretKey: []byte(hash)},
	}
	if err := identitycmd.ApplySecret(ctx, b.Controller, secret); err != nil {
		return fmt.Errorf("apply admin password secret %s/%s: %w", externalurl.Namespace, desktopAdminPasswordSecretName, err)
	}

	spec := spiceboxv1alpha1.ClusterIdentityProviderSpec{
		Kind:     "password",
		ClientID: desktopAdminEmail,
		ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
			Namespace: externalurl.Namespace,
			Name:      desktopAdminPasswordSecretName,
			Key:       desktopAdminPasswordSecretKey,
		},
		// The password kind has exactly one local account — no email domain
		// concept applies (see the CR-validity controller's password-kind
		// gate, which rejects a CR that leaves this false).
		AllowAnyEmail: true,
	}
	if err := identitycmd.ApplyClusterIdentityProvider(ctx, b.Controller, spec); err != nil {
		return fmt.Errorf("apply password ClusterIdentityProvider: %w", err)
	}
	// Wait for the CIdP controller to stamp Valid=True before returning.
	// Bring-up completing enables the dashboard menu item, but webd's IdP
	// loader 500s ("cluster identity provider has not yet been validated")
	// until the ClusterIdentityProvider is Valid — so without this wait the
	// admin dashboard shows "Sign-in unavailable" for the ~minute the reconcile
	// takes. Waiting here makes the dashboard reachable the moment bring-up
	// reports done.
	if err := s.waitCIdPValid(ctx, b.Controller); err != nil {
		return err
	}

	cl, cleanup, err := apspicedb.NewAuthzClient(ctx, apspicedb.ClusterAuthzDialer(dg))
	if err != nil {
		return fmt.Errorf("connect to authz service: %w", err)
	}
	defer cleanup()
	canonical, err := installcmd.CanonicalForEmail(desktopAdminEmail)
	if err != nil {
		return fmt.Errorf("canonicalize local admin email %q: %w", desktopAdminEmail, err)
	}
	if err := cl.TouchPlatformAdmin(ctx, canonical); err != nil {
		return fmt.Errorf("grant platform-admin to local admin (user:%s): %w", canonical, err)
	}

	s.logf("configureHook: provisioned local admin password sign-in and granted platform-admin (user:%s)", canonical)
	return nil
}

// setWebdExternalURL sets BOTH the trusted-url and sandbox-url keys of the
// spicebox-webd-external-url ConfigMap to http://127.0.0.1:<port> — the
// exact loopback address+port openDashboard's port-forward binds its local
// side to. The desktop app has no separate trusted/sandbox origin split (a
// single local VM, single loopback port serves both), so both keys always get
// the same value here.
//
// # Why the desktop still writes this at all
//
// The PublicEndpoint controller is this ConfigMap's writer wherever an endpoint
// has actually taken it. `desktop`'s policy is OnDemand, though, so no endpoint
// exists until something needs inbound webhooks, and a desktop may go its whole
// life without one. With no writer at all the ConfigMap keeps install's
// `http://localhost:8080` seed, which is wrong on BOTH halves — the desktop
// binds 127.0.0.1, on a port picked at runtime by PickStablePort — and webd
// dispatches on an exact bare-host match, so every route 404s.
//
// So this hook stands down rather than disappearing, and the condition is
// HANDOVER, not existence: see webdExternalURLClaimedByController.
//
// Get-or-create rather than a bare Update: `oap install` (which configureHook
// always runs after — see the doc above) already seeds this ConfigMap with
// localhost defaults, so the NotFound branch is not expected in practice, but
// failing loudly by creating it keeps this hook self-contained for the
// desktop's own install path.
func (s *desktopState) setWebdExternalURL(ctx context.Context, dg *apcmd.Globals, port int) error {
	b, err := dg.Bundle()
	if err != nil {
		return fmt.Errorf("build cluster client: %w", err)
	}
	return s.writeWebdExternalURL(ctx, b.Controller, b.Typed, port)
}

// writeWebdExternalURL is setWebdExternalURL's body with its two clients handed
// in rather than built, so the stand-down decision above can be tested in both
// directions without a cluster. See setWebdExternalURL for what it does and why.
func (s *desktopState) writeWebdExternalURL(ctx context.Context, ctrl client.Client, typed kubernetes.Interface, port int) error {
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cms := typed.CoreV1().ConfigMaps(cloud.WebdServiceNamespace)

	cm, err := cms.Get(ctx, spiceboxv1alpha1.WebdExternalURLConfigMap, metav1.GetOptions{})
	if err == nil {
		claimed, cErr := webdExternalURLClaimedByController(ctx, ctrl, cm)
		if cErr != nil {
			return cErr
		}
		if claimed {
			s.logf("configureHook: the PublicEndpoint controller has claimed the %s ConfigMap (field manager %q); leaving it alone",
				spiceboxv1alpha1.WebdExternalURLConfigMap, spiceboxv1alpha1.WebdExternalURLFieldOwner)
			return nil
		}
	}
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
				Namespace: cloud.WebdServiceNamespace,
			},
			Data: map[string]string{
				spiceboxv1alpha1.WebdTrustedURLKey: url,
				spiceboxv1alpha1.WebdSandboxURLKey: url,
			},
		}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create %s/%s configmap: %w", cloud.WebdServiceNamespace, spiceboxv1alpha1.WebdExternalURLConfigMap, err)
		}
		s.logf("configureHook: created %s/%s ConfigMap with webd base URL %s", cloud.WebdServiceNamespace, spiceboxv1alpha1.WebdExternalURLConfigMap, url)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s/%s configmap: %w", cloud.WebdServiceNamespace, spiceboxv1alpha1.WebdExternalURLConfigMap, err)
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[spiceboxv1alpha1.WebdTrustedURLKey] = url
	cm.Data[spiceboxv1alpha1.WebdSandboxURLKey] = url
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %s/%s configmap: %w", cloud.WebdServiceNamespace, spiceboxv1alpha1.WebdExternalURLConfigMap, err)
	}
	s.logf("configureHook: set %s/%s ConfigMap webd base URL to %s", cloud.WebdServiceNamespace, spiceboxv1alpha1.WebdExternalURLConfigMap, url)
	return nil
}

// webdExternalURLClaimedByController reports whether the PublicEndpoint
// controller has TAKEN OVER webd's external-URL ConfigMap — the only condition
// under which the desktop's own writer may stand down.
//
// It is two facts, and each excludes a different way of getting this wrong:
//
//  1. The controller's SSA field manager appears in the ConfigMap's
//     managedFields, i.e. it has actually applied the two keys at least once.
//     "A PublicEndpoint exists" is NOT this: the controller leaves the value
//     untouched on every Failed outcome, so an endpoint whose first reconcile
//     fails — unknown provider, refused domain, a policy that says no — would
//     silence this writer while writing nothing itself, leaving the desktop on
//     install's localhost:8080 seed. That is exactly the 404 this hook exists
//     to prevent.
//  2. An endpoint targeting webd still exists. A field-manager entry is not
//     withdrawn when a CR is deleted, so a claim alone outlives the thing that
//     made it: after a tunnel is torn down, the ConfigMap would keep a dead
//     public URL forever with this writer permanently muted.
//
// cloud.IsWebdTarget is the same predicate the controller itself asks, so the
// two cannot disagree about which endpoint owns the value.
//
// Errors are returned, never swallowed: "I could not tell" must not read as
// "nothing owns it", because that answer is the one that writes.
func webdExternalURLClaimedByController(ctx context.Context, c client.Client, cm *corev1.ConfigMap) (bool, error) {
	var applied bool
	for _, f := range cm.ManagedFields {
		if f.Manager == spiceboxv1alpha1.WebdExternalURLFieldOwner {
			applied = true
			break
		}
	}
	if !applied {
		return false, nil
	}

	var eps spiceboxv1alpha1.PublicEndpointList
	if err := c.List(ctx, &eps); err != nil {
		return false, fmt.Errorf("list PublicEndpoints to see who owns the %s ConfigMap: %w",
			spiceboxv1alpha1.WebdExternalURLConfigMap, err)
	}
	for i := range eps.Items {
		t := eps.Items[i].Spec.Target
		if cloud.IsWebdTarget(t.Namespace, t.Service) {
			return true, nil
		}
	}
	return false, nil
}

// demoChatNamespace is where the demo AgentClass MUST land: pkg/web/webui/chat
// creates every new chat conversation's Channel/AgentSession in a fixed
// namespace (its unexported newChatSessionNamespace constant) — an
// AgentClass applied to any other namespace (e.g. agentprimitives-system,
// where the rest of configureHook operates) is invisible to a new chat
// conversation started against it. The two packages can't share a Go
// constant (pkg/web/webui/chat's is intentionally unexported — it's a
// webd-internal implementation detail), so this is pinned separately and
// asserted against in TestApplyDemoAgentClass /
// TestDemoAgentClassYAML_MatchesChatNamespace.
const demoChatNamespace = "default"

// demoAgentClassName is the embedded demo AgentClass's metadata.name (see
// cmd/oap/internal/desktop/demo/pirate.yaml — the `pirate-private` variant).
// Pinned here so callers can reference the name without parsing the embedded
// YAML first.
const demoAgentClassName = "pirate-private"

// applyDemoAgentClass parses the embedded demo AgentClass (see
// cmd/oap/internal/desktop/demo) and applies it via the same
// get-or-create-or-update-spec idempotent pattern
// applySecret/applyClusterIdentityProvider use (idp_setup.go): so a rebuilt
// `oap` binary that ships an updated demo prompt/budget lands on an existing
// desktop install's next Configure too, not just a first install. Takes a
// plain client.Client (not a *apcmd.Globals) so it's directly unit-testable
// against a fake controller-runtime client — see TestApplyDemoAgentClass.
func applyDemoAgentClass(ctx context.Context, c client.Client) error {
	var ac spiceboxv1alpha1.AgentClass
	if err := yaml.Unmarshal(demo.AgentClassYAML, &ac); err != nil {
		return fmt.Errorf("parse embedded demo AgentClass: %w", err)
	}
	if ac.Name != demoAgentClassName || ac.Namespace != demoChatNamespace {
		return fmt.Errorf("embedded demo AgentClass is %s/%s, expected %s/%s", ac.Namespace, ac.Name, demoChatNamespace, demoAgentClassName)
	}

	existing := &spiceboxv1alpha1.AgentClass{}
	err := c.Get(ctx, types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, &ac)
	}
	if err != nil {
		return fmt.Errorf("get %s/%s AgentClass: %w", ac.Namespace, ac.Name, err)
	}
	existing.Spec = ac.Spec
	return c.Update(ctx, existing)
}

// initLocalHook is only invoked by Engine.Up when cfg.Channel != nil — which
// is unreachable today (see configureHook's TODO on
// knownExternalChannelKinds). Left as a clearly-erroring stub: a desktop's
// tunnel is a PublicEndpoint the operator reconciles, created on demand when a
// declared channel needs inbound webhooks, and this hook has neither the
// channel nor the provider credential that would take.
func (s *desktopState) initLocalHook(_ context.Context, _ []byte, _ map[string]string) (string, error) {
	return "", fmt.Errorf("desktop: external-channel tunnel setup is not implemented yet (built-in chat needs no tunnel)")
}

// logWriter returns a writer that tees to both the process's own stdout
// (useful when `oap desktop` is run from a Terminal) and the on-disk log file
// under ~/Library/Application Support/oap/logs — see the design doc's "Errors
// -> notification + logs" behavior.
func (s *desktopState) logWriter() io.Writer {
	return io.MultiWriter(s.out, s.logFile)
}

func (s *desktopState) logf(format string, args ...any) {
	fmt.Fprintf(s.logWriter(), "[%s] "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}

func openDesktopLogFile(supportDir string) (*os.File, error) {
	dir := filepath.Join(supportDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// notify shows a native macOS notification banner via osascript. Best-effort:
// a failure here (e.g. no display attached) is itself logged to stderr rather
// than silently dropped, but never treated as fatal — it's already the
// fallback error-surfacing path.
func notify(title, message string) {
	script := fmt.Sprintf("display notification %q with title %q", message, title)
	if err := exec.Command("osascript", "-e", script).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "oap desktop: notify(%q) failed: %v\n", title, err)
	}
}

// confirmDialog shows a native Yes/No confirmation via osascript and reports
// whether the user chose the affirmative button.
func confirmDialog(title, message string) bool {
	script := fmt.Sprintf(`display dialog %q with title %q buttons {"Cancel", "Confirm"} default button "Cancel" cancel button "Cancel"`, message, title)
	return exec.Command("osascript", "-e", script).Run() == nil
}

// onReady builds the (fixed) menu-item set once — systray has no API to
// reorder items, only to add/show/hide/remove — then starts the bring-up
// loop. Per-state transitions (setting-up / running / error / stopped) toggle
// title/tooltip/enabled on these same items, and drive the menu-bar status
// icon (see iconAnim), rather than rebuilding the menu.
func (s *desktopState) onReady() {
	anim := menubaricons.NewAnimator(
		func(b []byte) { systray.SetTemplateIcon(b, b) },
		assets.Frames,
		menuIconInterval,
	)
	s.mu.Lock()
	s.iconAnim = anim
	s.mu.Unlock()
	systray.SetTitle("") // the rendered icon replaces the text glyph
	anim.SetState(menubaricons.StateSetup)
	systray.SetTooltip("OAP Desktop — setting up…")

	// The status item doubles as the health parent: while setting up it shows
	// the current step (disabled, no submenu); once running, refreshHealth
	// retitles it ("Running" / "N components unhealthy") and its submenu lists
	// each deployed component's health. systray can't add/remove items at
	// runtime, so the per-component lines are a bounded pre-created pool (all
	// hidden now) that applyHealth shows/retitles — same shape as the sessions
	// pool. The parent is enabled in bringUp so the submenu becomes openable.
	s.statusItem = systray.AddMenuItem("Setting up…", "Current status")
	s.statusItem.Disable()
	s.healthPool = make([]*systray.MenuItem, maxHealthMenuItems)
	for i := range s.healthPool {
		item := s.statusItem.AddSubMenuItem("", "")
		item.Disable() // informational lines; not clickable
		item.Hide()
		s.healthPool[i] = item
	}
	// The controls follow the component-line pool directly. systray has no
	// in-submenu separator API, so there's no divider line here — the checkbox
	// (a checkmark glyph) and the "…" entry read as distinct from the ✓/✗ lines.
	notifyEnabled := true
	if cfg, err := desktop.Load(s.support); err != nil {
		s.logf("build health menu: load config for notify preference: %v", err)
	} else {
		notifyEnabled = cfg.HealthNotificationsEnabled()
	}
	s.healthNotifyItem = s.statusItem.AddSubMenuItemCheckbox("Notify on health issues", "Show a macOS notification when a component goes unhealthy or recovers", notifyEnabled)
	s.healthViewItem = s.statusItem.AddSubMenuItem("View full health…", "Open the admin dashboard in your browser")
	systray.AddSeparator()

	// "New chat" is the primary action (openSessions): it opens the session
	// dashboard at /sessions?new=1 over the same port-forward openDashboard
	// uses, which lands on the agent picker rather than on the list. It sits at
	// the top with its own separator beneath it.
	s.newChatItem = systray.AddMenuItem("New chat…", "Pick an agent and start a session in your browser")
	s.newChatItem.Disable()
	systray.AddSeparator()

	// "Active sessions" lists the live AgentSessions; each child opens that
	// session in the dashboard (openSessionRef -> /sessions?session=<ns>/<name>).
	// systray can't add/remove menu items at runtime, so the submenu is a
	// bounded POOL of pre-created slots (all hidden now) that refreshSessions
	// shows/hides + retitles as the live-session set changes — plus one
	// disabled "No active sessions" line shown when the set is empty. The
	// parent stays disabled (its submenu un-openable) until bring-up completes;
	// watchSessions then keeps it current. See applySessions / watchSessions.
	s.sessionsItem = systray.AddMenuItem("Active sessions", "Open a running session in your browser")
	s.sessionsItem.Disable()
	s.sessionPool = make([]*systray.MenuItem, maxSessionMenuItems)
	s.sessionRefs = make([]string, maxSessionMenuItems)
	for i := range s.sessionPool {
		item := s.sessionsItem.AddSubMenuItem("", "")
		item.Hide()
		s.sessionPool[i] = item
		go s.watchSessionClick(i, item)
	}
	s.sessionEmptyItem = s.sessionsItem.AddSubMenuItem("No active sessions", "No live chat sessions are running")
	s.sessionEmptyItem.Disable()
	s.sessionEmptyItem.Hide()

	s.dashboardItem = systray.AddMenuItem("Open dashboard", "Open the admin dashboard in your browser")
	s.dashboardItem.Disable()

	// "Install agent from .oap…" sits alongside "Open dashboard" — both are
	// cluster-affecting actions that need bring-up complete (see the Enable
	// block in bringUp / the Disable in stopCluster below). Picking a file
	// this way needs no dashboard round trip for the common case (a bundle
	// with no unmet required question); one that does need config falls back
	// to opening the dashboard itself — see installAgentFromFile.
	s.installAgentItem = systray.AddMenuItem("Install agent from .oap…", "Install a packaged agent bundle into your cluster")
	s.installAgentItem.Disable()

	// Checked iff the user's kubectl is currently pointed at this VM's
	// cluster (see IsVMContextCurrent) — kept in sync at construction, at
	// bring-up completion, after every click, and on a periodic tick (see
	// watchKubectlContext) so an external `kubectl config use-context` is
	// reflected too. Disabled until the VM kubeconfig exists (bring-up must
	// reach the point where writeKubeconfig has staged one).
	s.kubectlItem = systray.AddMenuItemCheckbox("Use oap cluster for kubectl", "Point kubectl at this VM's cluster", false)
	s.kubectlItem.Disable()
	s.refreshKubectlCheckmark()

	systray.AddSeparator()
	// Settings is always enabled, unlike the cluster-affecting items above:
	// systray's menu is fixed at build time (no add/remove after onReady), and
	// the settings window's config-file tabs (General, Model) work with the VM
	// down — only its cluster-backed tabs (health, kubectl) degrade
	// server-side (see settingsDeps' State/Clients closures). The reveal
	// behavior that used to live on its own "Edit configuration…" item now
	// lives in the settings UI itself (General tab, via the RevealConfig seam).
	s.settingsItem = systray.AddMenuItem("Settings…", "Open OAP Desktop settings")
	s.viewLogsItem = systray.AddMenuItem("View logs…", "Reveal the log file in Finder")

	systray.AddSeparator()
	// Stop sits directly above Uninstall — the VM-lifecycle actions grouped
	// together (stop keeps disk state, uninstall destroys it, quit stops + exits).
	s.stopItem = systray.AddMenuItem("Stop", "Stop the local VM (keeps its disk state)")
	s.stopItem.Disable()
	s.uninstallItem = systray.AddMenuItem("Uninstall", "Destroy the local VM and its disk state")
	s.uninstallItem.Disable()
	s.quitItem = systray.AddMenuItem("Quit", "Stop the local VM and quit OAP Desktop")

	go s.watchClicks()
	go s.watchKubectlContext()
	go s.watchSessions()
	go s.watchHealth()
	// Only start bring-up here when startup already had a valid config — the
	// alternate first-run/invalid-config path starts it from
	// onConfigSubmit once the setup window's form is submitted instead (see
	// startBringUp's doc for the once-guard that makes exactly one of these
	// two call sites actually run Engine.Up).
	if !s.needsConfigAtStart {
		s.startBringUp(s.initialConfig)
	}
}

// watchClicks fans in every menu item's ClickedCh onto the handlers below.
// Disabled items never fire (systray suppresses clicks on disabled items), so
// no extra guarding is needed here for e.g. sessionsItem.
func (s *desktopState) watchClicks() {
	for {
		select {
		case <-s.dashboardItem.ClickedCh:
			go s.openDashboard()
		case <-s.installAgentItem.ClickedCh:
			go s.installAgentFromFile()
		case <-s.newChatItem.ClickedCh:
			go s.openSessions()
		case <-s.kubectlItem.ClickedCh:
			go s.useClusterForKubectl()
		case <-s.stopItem.ClickedCh:
			go s.stopCluster()
		case <-s.settingsItem.ClickedCh:
			go s.openSettings()
		case <-s.viewLogsItem.ClickedCh:
			go s.revealInFinder(filepath.Join(s.support, "logs", "desktop.log"))
		case <-s.uninstallItem.ClickedCh:
			go s.uninstall()
		case <-s.healthNotifyItem.ClickedCh:
			// systray toggles the checkbox's visual state on click; mirror the
			// NEW state into config. (Checked() reflects the post-click state.)
			go s.setHealthNotifications(s.healthNotifyItem.Checked())
		case <-s.healthViewItem.ClickedCh:
			go s.openDashboard()
		case <-s.quitItem.ClickedCh:
			go s.quit()
		case <-s.ctx.Done():
			return
		}
	}
}

// watchKubectlContext keeps the "Use oap cluster for kubectl" checkmark in
// sync with the user's kubeconfig on a periodic tick, so an external change
// (e.g. the user running `kubectl config use-context` themselves, or editing
// ~/.kube/config by hand) is reflected without requiring a click on the item
// itself. The other refresh call sites (menu construction, bring-up
// completion, after a click) cover the cases that happen inside this
// process; this one covers everything else.
func (s *desktopState) watchKubectlContext() {
	ticker := time.NewTicker(kubectlCheckmarkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.refreshKubectlCheckmark()
		case <-s.ctx.Done():
			return
		}
	}
}

// startBringUp runs Engine.Up exactly once, however it's triggered: onReady
// calls it directly when a valid config already existed at launch;
// onConfigSubmit calls it with the just-submitted config once the setup
// window's first-run form is accepted. bringUpOnce guarantees only the
// first of those two ever actually starts bringUp — the config-gate
// invariant the task requires ("bring-up starts exactly once").
func (s *desktopState) startBringUp(cfg desktop.Config) {
	s.bringUpOnce.Do(func() {
		go s.bringUp(cfg)
	})
}

// bringUp runs Engine.Up once: Install -> Configure -> (skipped; built-in
// chat needs no tunnel). Transitions the menu (and the setup UI's Timeline)
// through setting-up -> running or -> error.
func (s *desktopState) bringUp(cfg desktop.Config) {
	s.setStatus(menubaricons.StateSetup, "Setting up…", "OAP Desktop — setting up…")
	s.logf("bring-up: starting")

	// Mark the synthetic "Initializing" step active immediately — BEFORE the
	// orphan-cleanup wait and disk staging below — so the setup window shows a
	// spinning first row the instant it loads, rather than a wall of pending
	// rows for the few seconds it takes to reach Engine.Up's first progress
	// call. The first real Up step ("Provisioning disk image") auto-completes
	// this one, since Timeline.Begin marks every earlier step done. See
	// bringUpStepNames.
	s.timeline.Begin("Initializing", time.Now().UnixMilli())

	// Orphan-VM cleanup runs in the background off the startup path (see
	// runDesktop) so it never delays the menubar icon; wait for it here so a
	// leftover VM from a prior crashed run is killed before we provision a new
	// one. The setup icon animates during the wait.
	if s.orphanCleanupDone != nil {
		<-s.orphanCleanupDone
	}

	// The orphan check is done; the remaining Initializing work (port selection
	// + staging the disk image) is usually quick, but update the sub-status so
	// the row never lingers on a stale "Checking for existing VM…" from the
	// cleanup phase until "Provisioning disk image" takes over and completes it.
	s.timeline.Detail("Initializing", "Preparing the virtual machine…")

	// Pick the stable loopback port BEFORE Install/Configure so configureHook
	// (which runs as part of eng.Up below) has it available to patch the
	// external-URL ConfigMap — see webdPort's doc for why this must happen
	// before Configure, not lazily in openDashboard. A failure here is
	// surfaced loudly but is NOT fatal to bring-up: the cluster still comes
	// up, only the dashboard shortcut won't work (s.webdPort stays 0, which
	// configureHook and openDashboard both treat as "no usable port").
	if port, err := desktop.PickStablePort(desktopWebdPortBase); err != nil {
		s.fail(fmt.Errorf("choose local port for webd dashboard: %w", err))
	} else {
		s.mu.Lock()
		s.webdPort = port
		s.mu.Unlock()
		s.logf("bring-up: webd dashboard will use loopback port %d", port)
	}

	if _, err := ensureDiskImage(s.resources, s.support); err != nil {
		s.failBringUp(fmt.Errorf("stage rootfs image: %w", err))
		return
	}

	// Stage the VM SSH key into the writable support dir so a *standalone* `oap`
	// (one not running from inside the .app bundle) can auto-discover it during
	// image reconcile against the oap-desktop context — see ensureVMKey. This is a
	// convenience for the external CLI; the app reads the key straight from
	// resourcesDir, so a staging failure is logged (per no-silent-errors) but must
	// NOT fail bring-up.
	if err := ensureVMKey(s.resources, s.support); err != nil {
		s.logf("bring-up: warning: could not stage VM SSH key for standalone oap: %v", err)
	}

	if _, err := s.eng.Up(s.ctx, cfg); err != nil {
		s.failBringUp(fmt.Errorf("bring-up failed: %w", err))
		return
	}

	s.logf("bring-up: complete")
	s.setStatus(menubaricons.StateRunning, "Running", "OAP Desktop — running")
	s.timeline.Complete(time.Now().UnixMilli())
	// Bring-up succeeded: enable the full running-state menu. Only the kubectl
	// item was unlocked early (installHook -> enableKubectlMenuItem, when k3s
	// began serving); enableRunningMenu enables the rest — dashboard, install
	// agent, new chat, sessions, status — and re-enables kubectl idempotently.
	s.enableRunningMenu()
	// The VM kubeconfig now exists (writeKubeconfig ran as part of Up), so
	// the checkmark can reflect reality; outside the lock above since
	// refreshKubectlCheckmark takes s.mu itself.
	s.refreshKubectlCheckmark()
	// Populate the "Active sessions" submenu immediately rather than waiting a
	// full watchSessions tick, so the list is current the moment the cluster
	// reaches running.
	s.refreshSessions()
	// Populate component health immediately rather than waiting a full
	// watchHealth tick, so the submenu is current the moment we reach running.
	s.refreshHealth()
}

// enableRunningMenu marks the cluster running and enables every
// cluster-dependent menu item. Called once, on FULL bring-up success:
// dashboard, install-agent, new-chat, sessions, and status all need the
// control plane the install pipeline brings up, so they stay disabled until
// bring-up completes. The one exception is the "Use oap cluster for kubectl"
// item, which works the instant the apiserver serves and is therefore unlocked
// earlier by enableKubectlMenuItem (installHook) — re-enabling it here is a
// harmless idempotent no-op.
//
// Nil-guarded per item: onConfigSubmit can trigger bring-up from an HTTP
// handler goroutine that races onReady's (systray-thread) menu-item
// construction (see onReady's doc); the guards make that race harmless instead
// of a nil-pointer panic.
func (s *desktopState) enableRunningMenu() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// running gates watchSessions' periodic list (see watchSessions) and
	// mirrors the "Active sessions" parent's enabled state.
	s.running = true
	for _, it := range []*systray.MenuItem{
		s.dashboardItem, s.installAgentItem, s.sessionsItem, s.newChatItem,
		s.kubectlItem, s.stopItem, s.uninstallItem, s.statusItem,
	} {
		if it != nil {
			it.Enable()
		}
	}
}

// enableKubectlMenuItem unlocks ONLY the "Use oap cluster for kubectl" item and
// refreshes its checkmark. Called the instant the guest Kubernetes API begins
// serving (installHook, right after waitClusterAPIReady): pointing kubectl at
// the VM works as soon as the apiserver responds, so a user can inspect the
// cluster with kubectl even while the rest of bring-up is still running — or is
// stuck (e.g. the install pipeline's "PostgreSQL — agent memory store" wait,
// which the desktop doesn't even use, running MEMORY_BACKEND=sqlite). Every
// other cluster-dependent item needs the control plane the pipeline still has
// to bring up, so those stay disabled until enableRunningMenu on bring-up
// success. refreshKubectlCheckmark takes s.mu itself, so it runs outside the
// lock. Nil-guarded for the same onReady race enableRunningMenu documents.
func (s *desktopState) enableKubectlMenuItem() {
	s.mu.Lock()
	if s.kubectlItem != nil {
		s.kubectlItem.Enable()
	}
	s.mu.Unlock()
	s.refreshKubectlCheckmark()
}

func (s *desktopState) fail(err error) {
	s.logf("error: %v", err)
	notify("OAP Desktop — error", err.Error())
	s.setStatus(menubaricons.StateError, fmt.Sprintf("Error: %v", err), "OAP Desktop — error (see View logs…)")
	s.mu.Lock()
	if s.uninstallItem != nil {
		s.uninstallItem.Enable() // still allow tearing down a partially-provisioned VM
	}
	s.mu.Unlock()
}

// failBringUp marks a HARD bring-up failure: same UI treatment as fail()
// (status icon, notification, uninstall left enabled so the menubar stays
// up for inspect/uninstall), plus it records err on s.runErr so runDesktop's
// return value — read after systray.Run returns, whether that's via the Quit
// menu item, Ctrl-C, or Uninstall — is non-zero. That's the process exit-code
// signal a launchd wrapper (or a Terminal exit-code check) keys off. It also
// fails the setup UI's Timeline, naming whichever step was last reported by
// progressHook (s.currentStep) — empty when the failure happened before any
// step began (e.g. ensureDiskImage), which Timeline.Fail handles by moving
// the overall phase to failed without attaching the error to any one step.
//
// Scoped to bringUp's two failure points rather than every s.fail() call: a
// later transient failure (e.g. "open dashboard" while the cluster is
// briefly unreachable) is an operational hiccup after a successful bring-up,
// not a reason to make the whole process report unhealthy on exit.
func (s *desktopState) failBringUp(err error) {
	s.fail(err)
	s.mu.Lock()
	s.runErr = err
	step := s.currentStep
	s.mu.Unlock()
	s.timeline.Fail(step, time.Now().UnixMilli(), err.Error())
}

func (s *desktopState) setStatus(state menubaricons.State, title, tooltip string) {
	s.mu.Lock()
	anim := s.iconAnim
	item := s.statusItem
	s.mu.Unlock()
	if anim != nil {
		anim.SetState(state)
	}
	systray.SetTooltip(tooltip)
	if item != nil {
		item.SetTitle(title)
	}
}

// progressHook is desktop.EngineHooks.Progress: it surfaces the current
// bring-up step in the menubar (the setup icon keeps animating; the status
// item + tooltip name the step), logs it, records it as s.currentStep (so a later
// failBringUp names the right step), and drives the setup UI's live
// Timeline via Begin. Called from the bring-up goroutine as each Up step
// starts.
func (s *desktopState) progressHook(step string) {
	s.logf("bring-up step: %s", step)
	s.setStatus(menubaricons.StateSetup, "Setting up: "+step, "OAP Desktop — "+step)
	s.mu.Lock()
	s.currentStep = step
	s.mu.Unlock()
	s.timeline.Begin(step, time.Now().UnixMilli())
}

// openDashboard port-forwards to the in-cluster webd Service (ClusterIP —
// there is no other host-reachable route to it yet) and opens it in the
// default browser. The port-forward is reused across clicks (started once,
// lazily) rather than torn down after each open.
//
// The port-forward's LOCAL side is pinned to s.webdPort — the SAME stable
// port configureHook pointed the spicebox-webd-external-url ConfigMap at.
// webd matches incoming requests' Host against that ConfigMap value and
// 404s on any mismatch (the root cause this feature exists to fix), so this
// must never fall back to an OS-assigned random port even if binding
// s.webdPort fails here: a random port would silently reintroduce the exact
// same 404 in a way that's much harder to notice (the port-forward itself
// succeeds; only the resulting page 404s).
func (s *desktopState) openDashboard() {
	pf, err := s.ensureWebdPortForward()
	if err != nil {
		s.fail(fmt.Errorf("open dashboard: %w", err))
		return
	}
	// webd serves the admin console under /admin (internal/cmd/webd/main.go); the
	// port-forward root itself 404s. Open the console path directly.
	if err := xbrowser.Open(strings.TrimRight(pf.URL(), "/") + "/admin"); err != nil {
		s.fail(fmt.Errorf("open dashboard: open browser: %w", err))
	}
}

// openSessions opens the session dashboard (pkg/web/webui/sessions) in the default
// browser. webd serves /admin and /sessions from the SAME process and port, so
// this reuses (and, on first click, lazily starts) exactly the same
// port-forward openDashboard uses — see ensureWebdPortForward for why the local
// side is pinned to s.webdPort rather than an OS-assigned port. The dashboard's
// data plane exists once webd's own prerequisites hold (NATS, SpiceDB, the
// operator memory URL — see pkg/web/webui/chat's ensureRegistry), all of which this
// install wires; this item stays disabled until bring-up completes, so that
// precondition is guaranteed by the time a click can reach here.
//
// The "?new=1" is what makes this a NEW-chat item rather than a link to a
// list: the shell reads it once into the picker's initial state and opens the
// agent chooser on first paint. Picking a UI-bearing agent lands on that
// agent's own view, every other agent on the transcript — the shell's own
// routing (pkg/web/webui/sessions' viewFor), not a decision made here.
//
// The picker is non-empty on this install because bring-up grants the desktop
// admin platform-admin (see provisionPasswordIdP), which carries
// platform#start_session — the BOOTSTRAP arm of the start gate. The gate's
// other arm derives standing from sessions the viewer already holds interact
// on, which is empty on a cluster whose first session has never been started,
// so without the grant this item would open a picker with nothing in it.
func (s *desktopState) openSessions() {
	pf, err := s.ensureWebdPortForward()
	if err != nil {
		s.fail(fmt.Errorf("open sessions: %w", err))
		return
	}
	if err := xbrowser.Open(strings.TrimRight(pf.URL(), "/") + "/sessions?new=1"); err != nil {
		s.fail(fmt.Errorf("open sessions: open browser: %w", err))
	}
}

// openSessionRef opens a SPECIFIC AgentSession in the dashboard via a
// "/sessions?session=<ns>/<name>" deep link. The selection is a QUERY
// parameter, not a path segment, because /sessions/api/… is a sibling route
// prefix — a namespace literally named "api" would be shadowed by it under a
// /sessions/{ns}/{name} form. Reuses the exact same webd port-forward
// openSessions/openDashboard use. Invoked from an "Active sessions" submenu
// slot's click handler (watchSessionClick) with the "<ns>/<name>" ref that slot
// currently maps to; the whole ref is escaped as one value so a namespace or
// name containing a reserved byte cannot forge a second parameter.
func (s *desktopState) openSessionRef(ref string) {
	pf, err := s.ensureWebdPortForward()
	if err != nil {
		s.fail(fmt.Errorf("open session %q: %w", ref, err))
		return
	}
	u := strings.TrimRight(pf.URL(), "/") + "/sessions?session=" + url.QueryEscape(ref)
	if err := xbrowser.Open(u); err != nil {
		s.fail(fmt.Errorf("open session %q: open browser: %w", ref, err))
	}
}

// openSettings lazily starts the settings server, then opens (or re-fronts)
// the singleton settings window. Always available — unlike the dashboard/
// sessions items above, this one is never disabled: its config-file tabs
// (General, Model) work with the VM down, and its cluster-backed tabs
// degrade server-side via settingsDeps' State/Clients closures instead of
// the item being greyed out.
//
// settingsMu makes the whole "is there already a server/window, and is that
// window still alive" decision atomic against a second click arriving while
// the first is still constructing — without it, two near-simultaneous clicks
// could each see settingsSrv == nil and start two servers, or each see a nil
// settingsWindowCmd and spawn two windows.
func (s *desktopState) openSettings() {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	if s.settingsSrv == nil {
		srv, err := settingsui.New(s.settingsDeps())
		if err != nil {
			s.logf("settings: construct server: %v", err)
			notify("OAP Desktop", "Couldn't open Settings — see logs.")
			return
		}
		if err := srv.Start(0); err != nil {
			s.logf("settings: start server: %v", err)
			notify("OAP Desktop", "Couldn't open Settings — see logs.")
			return
		}
		s.settingsSrv = srv
		go func() {
			if serveErr, ok := <-srv.Errors(); ok {
				s.logf("settings: server error: %v", serveErr)
			}
		}()
	}

	// A live settings window already exists — re-front it instead of spawning
	// a second one. ProcessState is only set once Wait returns (the reaper
	// goroutine below), which is exactly the liveness signal needed: nil
	// Process means Start never ran (shouldn't happen once settingsWindowCmd
	// is set, but guarded anyway), non-nil ProcessState means the child has
	// already exited.
	if c := s.settingsWindowCmd; c != nil && c.Process != nil && c.ProcessState == nil {
		activateProcess(c.Process.Pid)
		return
	}

	selfExe, err := mustSelfExe()
	if err != nil {
		s.logf("settings: resolve own executable path: %v", err)
		notify("OAP Desktop", "Couldn't open the Settings window — see logs.")
		return
	}
	cmd := exec.Command(selfExe, "desktop-window", "--url", s.settingsSrv.URL(), "--title", "OAP Desktop Settings")
	cmd.Stdout = s.logWriter()
	cmd.Stderr = s.logWriter()
	if err := cmd.Start(); err != nil {
		s.logf("settings: start window process: %v", err)
		notify("OAP Desktop", "Couldn't open the Settings window — see logs.")
		return
	}
	s.settingsWindowCmd = cmd
	// Reap the child in the background so it never lingers as a zombie once
	// the user closes the window on their own; stopSettingsUI's cleanup path
	// only signals the process (Kill), it never calls Wait itself — Wait must
	// be called exactly once per exec.Cmd, and this is that one call (mirrors
	// spawnSetupWindow's own reaper below).
	go func() {
		if waitErr := cmd.Wait(); waitErr != nil {
			s.logf("settings window process exited: %v", waitErr)
		}
	}()
}

// settingsDeps builds the settings server's collaborator seam. Every
// closure here either reads a set-once desktopState field directly (support,
// logf) or takes the same lock the field's other readers/writers already
// use (s.mu for the bring-up/menu-item fields; none of these touch
// settingsMu, since that guards settingsSrv/settingsWindowCmd themselves,
// not the desktop's underlying VM state).
func (s *desktopState) settingsDeps() settingsui.Deps {
	return settingsui.Deps{
		SupportDir: s.support,
		AppVersion: oapVersion(),
		Logf:       s.logf,
		State: func() settingsui.State {
			s.mu.Lock()
			defer s.mu.Unlock()
			switch {
			case s.running:
				return settingsui.State{Running: true, Phase: "running"}
			case s.runErr != nil:
				return settingsui.State{Phase: "failed", Step: s.currentStep}
			case s.currentStep != "":
				return settingsui.State{Phase: "starting", Step: s.currentStep}
			default:
				return settingsui.State{Phase: "stopped"}
			}
		},
		Clients: func() (*kube.Bundle, error) {
			s.mu.Lock()
			dg := s.dg
			s.mu.Unlock()
			if dg == nil {
				return nil, fmt.Errorf("cluster not running")
			}
			return dg.Bundle()
		},
		// ReapplyConfig pushes ONLY the changed field group to the cluster (see
		// reapplyConfig) — deliberately NOT configureHook / the full settings
		// wizard, whose atomic model-catalog apply would wipe the operator's
		// Cluster-tab edits.
		ReapplyConfig: s.reapplyConfig,
		// OnHealthNotificationsSaved only syncs the checkbox: the server's
		// config PUT now owns the load-modify-save to config.json (see
		// setHealthNotifications' doc), so this seam just mirrors the NEW
		// value onto the menu item the click path also drives.
		OnHealthNotificationsSaved: func(enabled bool) {
			s.mu.Lock()
			item := s.healthNotifyItem
			s.mu.Unlock()
			if item == nil {
				return
			}
			if enabled {
				item.Check()
			} else {
				item.Uncheck()
			}
		},
		KubectlCurrent: func() bool {
			data, err := os.ReadFile(userKubeconfigPath())
			if err != nil {
				return false
			}
			return desktop.IsVMContextCurrent(data)
		},
		// Fire-and-forget (see settingsui.Deps.KubectlUse): useClusterForKubectl
		// surfaces any failure through s.fail's desktop notification, so this
		// returns nil — the HTTP layer reads that as accepted-not-verified.
		KubectlUse:   func() error { s.useClusterForKubectl(); return nil },
		RevealConfig: func() { s.revealInFinder(filepath.Join(s.support, "config.json")) },
		RevealLogs:   func() { s.revealInFinder(filepath.Join(s.support, "logs", "desktop.log")) },
	}
}

// reapplyConfig pushes ONLY the field groups named by scope to the running
// cluster, so a Settings-UI save of one thing never disturbs the operator's
// other cluster-tier settings.
//
// It deliberately does NOT re-run configureHook / the full --defaults settings
// wizard. That path force-SSA-applies the whole ClusterAgentSettings under the
// wizard's field manager, and because the model catalog is an atomic list it
// would delete every extra catalog entry, pinning rule and content inspector
// the operator set from the Cluster tab. Each half here targets exactly its own
// cluster resource instead: the central token Secret + the default catalog
// entry (settingswizard.UpdateDefaultModelEntry preserves the rest of the
// spec), or the password sign-in IdP.
//
// scope is honored verbatim — handleConfigPut only asks for the groups that
// actually changed, and never asks for a group that did not (see settingsui's
// ReapplyScope). A password re-apply is skipped when the hash is empty, exactly
// as configureHook treats a config.json predating the field.
//
// SECURITY: no error this returns embeds cfg.Model.APIKey or the password hash;
// its text flows verbatim into the settings server's HTTP response.
// modeltoken.EnsureSecret and provisionPasswordIdP name only paths/coordinates,
// never the secret value, and the wraps added here name only steps.
func (s *desktopState) reapplyConfig(ctx context.Context, cfg desktop.Config, scope settingsui.ReapplyScope) error {
	kc, err := os.ReadFile(filepath.Join(s.support, kubeconfigFileName))
	if err != nil {
		return fmt.Errorf("read staged kubeconfig: %w", err)
	}
	dg, err := s.writeKubeconfig(kc)
	if err != nil {
		return err
	}

	if scope.Model {
		b, err := dg.Bundle()
		if err != nil {
			return fmt.Errorf("connect to local cluster: %w", err)
		}
		ref := modeltoken.SecretRef{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token"}
		if err := modeltoken.EnsureSecret(ctx, b.Dynamic, ref, cfg.Model.APIKey); err != nil {
			return err
		}
		model, err := cfg.EffectiveModel()
		if err != nil {
			return err
		}
		if err := settingswizard.UpdateDefaultModelEntry(ctx, b.Controller, cfg.Model.Provider, model); err != nil {
			return fmt.Errorf("update cluster default model entry: %w", err)
		}
	}

	if scope.Password && cfg.AdminPasswordHash != "" {
		if err := s.provisionPasswordIdP(ctx, dg, cfg.AdminPasswordHash); err != nil {
			return fmt.Errorf("provision local admin password sign-in: %w", err)
		}
	}

	return nil
}

// oapVersion reports the build that produced this binary, mirroring
// sessioncmd.oapVersion's use of runtime/debug.ReadBuildInfo (that one is
// unexported in its own package too, so this is a small separate copy in
// this package rather than a cross-package import of an unexported
// identifier). Surfaced to the settings UI via settingsDeps' AppVersion, and
// echoed verbatim by GET /api/cluster/install-info — see settingsui.Deps'
// doc on that field for why there's no shared "oap's own version" helper to
// call instead. An unstamped local build (`go build` outside `go install`/a
// release) reports Go's own "(devel)", same as sessioncmd's copy.
func oapVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return ""
	}
	return info.Main.Version
}

// ensureWebdPortForward returns the process's single loopback port-forward to
// the in-cluster webd Service, starting it lazily on first use. Every browser
// shortcut (openDashboard / openChat / openChatSession) shares this one
// forwarder.
//
// The forward's LOCAL side is pinned to s.webdPort — the SAME stable port
// configureHook pointed the spicebox-webd-external-url ConfigMap at. webd
// matches an incoming request's Host against that ConfigMap value and 404s on
// any mismatch (the root cause this whole feature exists to fix), so this must
// never fall back to an OS-assigned random port: a random port would silently
// reintroduce the same 404 in a way that's hard to notice (the forward itself
// succeeds; only the resulting page 404s). A zero s.webdPort (PickStablePort
// failed at bring-up) is therefore a loud error here, not a fallback.
//
// Concurrency: multiple shortcut clicks can race to be the first to start the
// forwarder. Binding s.webdPort twice would fail the second Start, so the
// winner is chosen under s.mu and any loser's forwarder is Stopped — callers
// always get the one live forwarder.
func (s *desktopState) ensureWebdPortForward() (*portforward.PortForwarder, error) {
	s.mu.Lock()
	dg, pf, port := s.dg, s.pf, s.webdPort
	// A cached forwarder is only good while its stream is still alive. A
	// port-forward binds to ONE pod, not to the Service, so anything that
	// replaces the pod — a webd rollout, an eviction, a node restart — kills
	// it; the local port simply stops listening.
	//
	// Returning the dead one anyway (the previous behaviour) made that
	// unrecoverable without restarting the whole app: s.pf stayed non-nil
	// forever, so every later click handed back the same corpse and opened a
	// browser at a port nothing was listening on. The symptom is a menu that
	// silently stops working, with a running cluster and a healthy webd behind
	// it — see portforward.Done's own doc, which asks callers who need to
	// detect mid-session stream death to do exactly this.
	//
	// Checked and cleared under the SAME lock that read it, so two clicks
	// racing cannot both decide to rebuild.
	var stale *portforward.PortForwarder
	if pf != nil && forwarderDead(pf) {
		stale, pf, s.pf = pf, nil, nil
	}
	s.mu.Unlock()
	if stale != nil {
		// Idempotent, and outside the lock: it only closes a channel, but no
		// cleanup belongs inside a mutex this hot.
		stale.Stop()
		s.logf("webd port-forward on port %d died (its backing pod went away); re-establishing", port)
	}
	if dg == nil {
		return nil, fmt.Errorf("no running cluster yet")
	}
	if port == 0 {
		return nil, fmt.Errorf("no stable local port was chosen at bring-up; webd's base URL was never configured")
	}
	if pf != nil {
		return pf, nil
	}

	b, err := dg.Bundle()
	if err != nil {
		return nil, fmt.Errorf("connect to local cluster: %w", err)
	}
	newPf, err := portforward.New(b.REST, cloud.WebdServiceNamespace, cloud.WebdServiceName, cloud.WebdPodSelector, uint16(cloud.WebdServicePort), uint16(port))
	if err != nil {
		return nil, fmt.Errorf("build port-forward: %w", err)
	}
	if err := newPf.Start(s.ctx, s.logWriter()); err != nil {
		return nil, fmt.Errorf("start port-forward on loopback port %d (it must match the webd base URL configured at bring-up): %w", port, err)
	}

	s.mu.Lock()
	if s.pf != nil {
		// Lost the race — another click started one first; keep that.
		s.mu.Unlock()
		newPf.Stop()
		return s.pf, nil
	}
	s.pf = newPf
	s.mu.Unlock()
	// Watch this forwarder so its death is noticed when it happens, rather
	// than on the next click. See watchWebdPortForward.
	go s.watchWebdPortForward(newPf, port)
	return newPf, nil
}

// watchWebdPortForward re-establishes the webd forward as soon as the one it
// was given dies, instead of waiting for a viewer to click something.
//
// A port-forward binds to a POD, not to a Service, so anything that replaces
// the pod kills it: a webd rollout, an eviction, a node restart. The lazy
// repair in ensureWebdPortForward handles that correctly but only when
// something asks — so between the pod going away and the next menu click, the
// dashboard is simply gone, and a viewer with the page already open sees a
// dead tab with no clue that a click would fix it. During a session of
// repeated webd deploys that is most of the time.
//
// It re-enters ensureWebdPortForward rather than rebuilding here, so there is
// exactly one construction path and one place that owns s.pf. The stale
// forwarder is already cleared by that function's own forwarderDead check; all
// this goroutine adds is the timing.
//
// One goroutine per forwarder, ending when its own forwarder dies — so a
// rebuild replaces the watcher along with the thing watched, and they cannot
// accumulate. A context cancellation ends it without a rebuild: the app is
// shutting down, and reconnecting to a cluster nobody is watching would keep
// the process alive doing nothing.
func (s *desktopState) watchWebdPortForward(pf *portforward.PortForwarder, port int) {
	select {
	case <-pf.Done():
		// Only the CURRENT forwarder's death is worth acting on. A watcher
		// whose forwarder has already been replaced (a racing click rebuilt it
		// first) would otherwise tear down a healthy one.
		// Clearing s.pf here is REQUIRED, not tidiness. PortForwarder.done is
		// a single-value buffered channel and forwarderDead reads it with a
		// non-blocking receive — so this goroutine has just CONSUMED the only
		// death notice there will ever be. Leaving s.pf in place would make
		// the lazy path's forwarderDead check answer "alive" for a corpse, and
		// ensureWebdPortForward would hand that corpse back to every caller
		// forever: a self-heal that permanently disabled the repair it exists
		// to trigger.
		//
		// Cleared under the SAME lock that identifies it, so a racing click
		// cannot observe the half-state.
		s.mu.Lock()
		current := s.pf == pf
		running := s.running
		if current {
			s.pf = nil
		}
		s.mu.Unlock()
		if !current || !running {
			return
		}
		pf.Stop() // idempotent; releases the local port before the rebuild binds it
		s.logf("webd port-forward on port %d died; re-establishing without waiting for a click", port)
		if _, err := s.ensureWebdPortForward(); err != nil {
			// Logged, never silent: the menu still works (the lazy path will
			// try again on the next click), but a viewer whose page just went
			// dead deserves a reason to exist somewhere an operator can read.
			s.logf("webd port-forward: automatic re-establish failed: %v (a menu click will retry)", err)
		}
	case <-s.ctx.Done():
		return
	}
}

// watchSessions keeps the "Active sessions" submenu in sync with the live
// chat AgentSessions on a periodic tick, mirroring watchKubectlContext's
// pattern. It only lists while the cluster is running (the running gate) —
// before bring-up completes and after a Stop there is nothing to list and the
// parent item is disabled anyway. The immediate post-bring-up populate happens
// in bringUp; this loop handles every change after that (sessions started from
// the web chat, sessions finishing, etc.).
func (s *desktopState) watchSessions() {
	ticker := time.NewTicker(sessionRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			running := s.running
			s.mu.Unlock()
			if running {
				s.refreshSessions()
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// refreshSessions lists the live chat AgentSessions and rewires the submenu
// pool to match (see applySessions). A client/list failure is logged (never
// silently dropped) and the last-known submenu is left in place rather than
// blanked — a transient apiserver blip shouldn't erase the list. Does nothing
// before a cluster exists.
func (s *desktopState) refreshSessions() {
	s.mu.Lock()
	dg := s.dg
	s.mu.Unlock()
	if dg == nil {
		return
	}
	b, err := dg.Bundle()
	if err != nil {
		s.logf("refresh sessions: build cluster client: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, sessionRefreshTimeout)
	defer cancel()
	sessions, err := listChatSessions(ctx, b.Controller, maxSessionMenuItems)
	if err != nil {
		s.logf("refresh sessions: list chat AgentSessions: %v", err)
		return
	}
	s.applySessions(sessions)
}

// applySessions rewires the submenu pool to the given (already filtered,
// sorted, and capped) sessions: slot i shows sessions[i]'s label + tooltip and
// records its name for the click handler; surplus slots are hidden. The empty
// line shows only when there are zero sessions. The name mapping is rewritten
// under s.mu (so a concurrent click handler reads a consistent slice); the
// systray items themselves are mutated outside the lock, mirroring
// refreshKubectlCheckmark's "read item ref under lock, touch systray outside".
func (s *desktopState) applySessions(sessions []spiceboxv1alpha1.AgentSession) {
	s.mu.Lock()
	pool := s.sessionPool
	empty := s.sessionEmptyItem
	n := len(sessions)
	if n > len(pool) {
		n = len(pool)
	}
	labels := make([]string, n)
	tips := make([]string, n)
	for i := 0; i < len(s.sessionRefs); i++ {
		if i < n {
			s.sessionRefs[i] = sessions[i].Namespace + "/" + sessions[i].Name
			labels[i] = chatSessionLabel(sessions[i])
			tips[i] = chatSessionTooltip(sessions[i])
		} else {
			s.sessionRefs[i] = ""
		}
	}
	s.mu.Unlock()

	for i, item := range pool {
		if i < n {
			item.SetTitle(labels[i])
			item.SetTooltip(tips[i])
			item.Show()
		} else {
			item.Hide()
		}
	}
	if empty != nil {
		if n == 0 {
			empty.Show()
		} else {
			empty.Hide()
		}
	}
}

// clearSessions hides every submenu slot + the empty line and forgets the
// name mapping. Called on Stop so a stale list never lingers behind the
// (now-disabled) parent and a later re-run repopulates from scratch.
func (s *desktopState) clearSessions() {
	s.mu.Lock()
	pool := s.sessionPool
	empty := s.sessionEmptyItem
	for i := range s.sessionRefs {
		s.sessionRefs[i] = ""
	}
	s.mu.Unlock()
	for _, item := range pool {
		item.Hide()
	}
	if empty != nil {
		empty.Hide()
	}
}

// watchSessionClick handles clicks on one submenu pool slot for the life of
// the process. Each slot has a fixed index; the AgentSession it currently
// maps to lives in s.sessionRefs[idx] (rewritten on every refresh), read
// here under s.mu. An empty name (a hidden/unused slot) is ignored — a
// belt-and-suspenders guard on top of systray already suppressing clicks on
// hidden items.
func (s *desktopState) watchSessionClick(idx int, item *systray.MenuItem) {
	for {
		select {
		case <-item.ClickedCh:
			s.mu.Lock()
			var ref string
			if idx < len(s.sessionRefs) {
				ref = s.sessionRefs[idx]
			}
			s.mu.Unlock()
			if ref != "" {
				go s.openSessionRef(ref)
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// chatSessionNamespace is the namespace the built-in web chat creates its
// AgentSessions in — pkg/web/webui/chat's (unexported) newChatSessionNamespace,
// which is also where configureHook lands the demo AgentClass
// (demoChatNamespace). Pinned to that same "default" value; the packages
// can't share a Go constant (chat's is a webd-internal detail), so
// listChatSessions references this.
const chatSessionNamespace = demoChatNamespace

// listChatSessions lists the live chat AgentSessions in the chat namespace —
// those labelled with the browser channel kind (the label browsersession.Create
// stamps on every web-chat session) — then filters out terminal ones, sorts,
// and caps to limit via selectChatSessions.
func listChatSessions(ctx context.Context, c client.Client, limit int) ([]spiceboxv1alpha1.AgentSession, error) {
	var list spiceboxv1alpha1.AgentSessionList
	if err := c.List(ctx, &list,
		client.InNamespace(chatSessionNamespace),
		client.MatchingLabels{spiceboxv1alpha1.LabelChannelKind: browser.KindName},
	); err != nil {
		return nil, err
	}
	return selectChatSessions(list.Items, limit), nil
}

// selectChatSessions is the pure core of the session refresh: drop terminal
// sessions (Succeeded/Failed — a finished conversation isn't "active"), sort
// newest-first (tiebroken by name for a stable menu order), and cap to limit.
func selectChatSessions(items []spiceboxv1alpha1.AgentSession, limit int) []spiceboxv1alpha1.AgentSession {
	live := make([]spiceboxv1alpha1.AgentSession, 0, len(items))
	for _, s := range items {
		if isTerminalChatSessionPhase(s.Status.Phase) {
			continue
		}
		live = append(live, s)
	}
	sort.SliceStable(live, func(i, j int) bool {
		ti, tj := live[i].CreationTimestamp.Time, live[j].CreationTimestamp.Time
		if ti.Equal(tj) {
			return live[i].Name < live[j].Name
		}
		return ti.After(tj)
	})
	if limit >= 0 && len(live) > limit {
		live = live[:limit]
	}
	return live
}

// isTerminalChatSessionPhase reports whether an AgentSession has finished
// (Succeeded or Failed) and so is no longer an "active" chat. Every other
// phase — Pending, Running, AwaitingDecision, AwaitingRetry,
// AwaitingCredentials, Idle, or the empty (freshly-created) phase — is treated
// as live.
func isTerminalChatSessionPhase(phase string) bool {
	switch phase {
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded, spiceboxv1alpha1.AgentSessionPhaseFailed:
		return true
	default:
		return false
	}
}

// chatSessionLabel is the human-readable submenu title for a chat session:
// the agent class prefixed onto the first message (spec.prompt.inline,
// collapsed to a single line and truncated) when a prompt is available, and
// the full session name (which itself encodes class + short id — see
// browsersession.NewSessionName) as an unambiguous fallback when it isn't.
func chatSessionLabel(s spiceboxv1alpha1.AgentSession) string {
	class := s.Spec.Class
	if prompt := singleLine(s.Spec.Prompt.Inline); prompt != "" {
		if class == "" {
			return truncateLabel(prompt, 48)
		}
		return fmt.Sprintf("%s: %s", class, truncateLabel(prompt, 48))
	}
	if s.Name != "" {
		return s.Name
	}
	if class != "" {
		return class
	}
	return "session"
}

// chatSessionTooltip is the hover text for a session's submenu slot: its full
// name and current phase, so the (necessarily short) title stays readable
// while the details are one hover away.
func chatSessionTooltip(s spiceboxv1alpha1.AgentSession) string {
	phase := s.Status.Phase
	if phase == "" {
		phase = spiceboxv1alpha1.AgentSessionPhasePending
	}
	return fmt.Sprintf("%s — %s", s.Name, phase)
}

// singleLine collapses any run of whitespace (including newlines) in s to a
// single space and trims the ends, so a multi-line first message renders as a
// one-line menu title.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateLabel shortens s to at most max runes, appending an ellipsis when it
// had to cut (rune-aware so a multibyte first message never splits a
// character). max <= 0 returns s unchanged.
func truncateLabel(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ") + "…"
}

// userKubeconfigPath resolves the user's default kubeconfig location —
// respecting $KUBECONFIG the same way `kubectl` itself would — via clientcmd's
// default loading rules. This
// is deliberately NOT the outer *apcmd.Globals' --kubeconfig flag (if any): the
// menu item is about the AMBIENT kubectl the user runs from a terminal, not
// whatever this process happens to be pointed at.
func userKubeconfigPath() string {
	return clientcmd.NewDefaultClientConfigLoadingRules().GetDefaultFilename()
}

// useClusterForKubectl merges the VM's staged kubeconfig into the user's
// default kubeconfig as the distinct "oap-desktop" context (see
// desktop.MergeVMContext) and makes it current, then refreshes the menu
// item's checkmark. The VM kubeconfig must already exist — the item is kept
// disabled until bring-up stages one (see bringUp) — but a missing/empty
// user kubeconfig is fine (MergeVMContext starts from an empty config).
func (s *desktopState) useClusterForKubectl() {
	vmPath := filepath.Join(s.support, kubeconfigFileName)
	vmKubeconfig, err := os.ReadFile(vmPath)
	if err != nil {
		s.fail(fmt.Errorf("use oap cluster for kubectl: read VM kubeconfig %q: %w", vmPath, err))
		return
	}

	userPath := userKubeconfigPath()
	userKubeconfig, err := os.ReadFile(userPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail(fmt.Errorf("use oap cluster for kubectl: read %q: %w", userPath, err))
		return
	}

	merged, err := desktop.MergeVMContext(userKubeconfig, vmKubeconfig)
	if err != nil {
		s.fail(fmt.Errorf("use oap cluster for kubectl: %w", err))
		return
	}
	if err := atomicWriteFile(userPath, merged, 0o600); err != nil {
		s.fail(fmt.Errorf("use oap cluster for kubectl: write %q: %w", userPath, err))
		return
	}

	s.logf("kubectl now points at the oap cluster context %q in %s", desktop.APContextName, userPath)
	notify("OAP Desktop", "kubectl now points at the oap cluster.")
	s.refreshKubectlCheckmark()
}

// refreshKubectlCheckmark sets the "Use oap cluster for kubectl" item's
// checkmark to match reality: checked iff the user's current-context is the
// oap-desktop one (desktop.IsVMContextCurrent). Called at menu construction,
// at bring-up completion, after every click on the item, and periodically by
// watchKubectlContext — see those call sites for why each is needed. A
// missing user kubeconfig reads as "not current" (IsVMContextCurrent treats
// empty input as false), which is correct: no kubeconfig means kubectl isn't
// pointed at anything, let alone this VM.
func (s *desktopState) refreshKubectlCheckmark() {
	s.mu.Lock()
	item := s.kubectlItem
	s.mu.Unlock()
	if item == nil {
		return
	}

	path := userKubeconfigPath()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logf("refresh kubectl checkmark: read %q: %v", path, err)
		return
	}
	if desktop.IsVMContextCurrent(data) {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func (s *desktopState) revealInFinder(path string) {
	if err := exec.Command("open", "-R", path).Run(); err != nil {
		s.logf("reveal %q in Finder: %v", path, err)
		notify("OAP Desktop", fmt.Sprintf("Couldn't reveal %s in Finder: %v", path, err))
	}
}

// stopCluster stops the VM without destroying its disk state (Engine.Down).
func (s *desktopState) stopCluster() {
	s.mu.Lock()
	// Stop the health watcher from touching the icon the instant Stop is clicked
	// (before eng.Down and before setStatus(StateStopped)): applyHealth only
	// flips the icon while running is true, checked under this same s.mu, so once
	// this commits no in-flight or later health tick can override the
	// StateShuttingDown/StateStopped icons — making "lifecycle transition always
	// wins" airtight.
	s.running = false
	if pf := s.pf; pf != nil {
		pf.Stop()
		s.pf = nil
	}
	s.mu.Unlock()

	s.setStatus(menubaricons.StateShuttingDown, "Stopping…", "OAP Desktop — stopping…")
	if err := s.eng.Down(s.ctx); err != nil {
		s.fail(fmt.Errorf("stop: %w", err))
		return
	}
	s.logf("stopped")
	s.setStatus(menubaricons.StateStopped, "Stopped", "OAP Desktop — stopped")
	s.mu.Lock()
	s.dashboardItem.Disable()
	s.installAgentItem.Disable()
	s.sessionsItem.Disable()
	s.newChatItem.Disable()
	s.stopItem.Disable()
	s.mu.Unlock()
	// Hide the (now un-openable) submenu's slots and forget their session
	// mapping so a later re-run repopulates from scratch rather than briefly
	// showing stale titles.
	s.clearSessions()
}

// uninstall confirms via a native dialog, then destroys the VM and its disk
// state (Engine.Uninstall) — deliberately NOT the persistent-memory virtiofs
// share (see cmd/oap/internal/desktop/vz/provider_darwin.go's Destroy doc: it
// leaves VMConfig.HostShareDir alone by design).
func (s *desktopState) uninstall() {
	if !confirmDialog("OAP Desktop", "Uninstall will destroy the local VM and all its disk state (your chat memory is kept). Continue?") {
		return
	}
	s.mu.Lock()
	if pf := s.pf; pf != nil {
		pf.Stop()
		s.pf = nil
	}
	s.mu.Unlock()

	s.setStatus(menubaricons.StateUninstalling, "Uninstalling…", "OAP Desktop — uninstalling…")
	if err := s.eng.Uninstall(s.ctx); err != nil {
		s.fail(fmt.Errorf("uninstall: %w", err))
		return
	}
	s.logf("uninstalled")
	notify("OAP Desktop", "Uninstalled. Quitting.")
	systray.Quit()
}

// quitShutdownTimeout bounds the fresh context quit() (and runDesktop's own
// stop-on-exit defer, above) gives Engine.Down — see quit's doc for why it
// can't reuse s.ctx/ctx.
const quitShutdownTimeout = 35 * time.Second

// quitAnimHold is how long the quit fade-away animation plays after the VM has
// stopped, before the process exits. Kept under one animation loop so the icon
// fades once (without looping back to full) as the app closes.
const quitAnimHold = 1200 * time.Millisecond

// quit stops the VM (best-effort — a failure here is logged but does not
// block quitting) and exits. This is also the SIGINT/SIGTERM path (see
// runDesktop's signal goroutine), so by the time quit runs, s.ctx may
// ALREADY be cancelled — using it here would make vz.Provider.Stop's
// internal context.WithTimeout(ctx, vmStopTimeout) expire immediately,
// skipping the graceful guest RequestStop wait entirely. Use a fresh,
// independently-bounded context instead so a Ctrl-C still gets a clean VM
// shutdown rather than an abrupt hard stop.
func (s *desktopState) quit() {
	// Shutdown portion: the VM is stopping (eng.Down, bounded by
	// quitShutdownTimeout), so show the same flip the Stop menu uses while it
	// winds down.
	s.setStatus(menubaricons.StateShuttingDown, "Quitting…", "OAP Desktop — quitting…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), quitShutdownTimeout)
	defer cancel()
	if err := s.eng.Down(shutdownCtx); err != nil {
		s.logf("quit: stop failed: %v", err)
	}
	// Quitting portion: the VM is down — play the fade-away for a beat so it's
	// visible before the process exits (otherwise systray.Quit removes the icon
	// immediately and nothing animates).
	s.setStatus(menubaricons.StateQuitting, "Quitting…", "OAP Desktop — quitting…")
	time.Sleep(quitAnimHold)
	s.stopSetupUI()
	s.stopSettingsUI()
	systray.Quit()
}

// onExit runs after systray.Quit() completes teardown; systray.Run then
// returns and runDesktop reads s.runErr. Quitting is a normal user action,
// not a failure, so runErr stays nil here.
func (s *desktopState) onExit() {
	s.mu.Lock()
	anim := s.iconAnim
	if pf := s.pf; pf != nil {
		pf.Stop()
	}
	s.mu.Unlock()
	if anim != nil {
		anim.Stop()
	}
	s.stopSetupUI()
	s.stopSettingsUI()
}

// onConfigSubmit is setupui.OnConfigFunc, wired via setupui.New in
// runDesktop. Invoked by POST /config once the setup window's first-run
// form is submitted; the HTTP layer (setupui.Server.handleConfig) already
// validated cfg before calling this, but OnConfigFunc is a public seam any
// caller could invoke, so this validates again rather than trusting the one
// known call site.
//
// password is the plaintext admin password submitted alongside cfg — see
// OnConfigFunc's doc for why it travels as a separate argument rather than
// a Config field. It is hashed via passwordkind.HashPassword (the SAME
// bcrypt cost `oap idp setup password` uses) immediately, and only the
// resulting hash is ever assigned onto cfg.AdminPasswordHash; the plaintext
// itself is never logged, never written to disk, and is dropped here once
// this function returns. configureHook (run by Engine.Up's Configure step
// below) is what actually provisions the password IdP CR + Secret from
// cfg.AdminPasswordHash — this function only prepares cfg for it.
//
// Persists cfg (hash only) to support/config.json (the SAME file
// desktop.Load reads — so a later relaunch has a valid config and skips the
// setup form), flips the Timeline's NeedsConfig off so the setup page moves
// from the form to the live timeline, then starts bring-up.
func (s *desktopState) onConfigSubmit(cfg desktop.Config, password string) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if password == "" {
		return fmt.Errorf("admin password is required")
	}
	hash, err := passwordkind.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	cfg.AdminPasswordHash = hash

	// Preserve UI preferences the setup form doesn't surface (the setup form
	// builds cfg from its own fields only, so a re-run would otherwise reset
	// HealthNotifications back to nil/default).
	if existing, err := desktop.Load(s.support); err != nil {
		s.logf("preserve health-notification preference: load existing config: %v", err)
	} else {
		cfg.HealthNotifications = existing.HealthNotifications
	}

	if err := cfg.Save(s.support); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	s.timeline.SetNeedsConfig(false)
	s.startBringUp(cfg)
	return nil
}

// bringUpStepNames returns the ordered step names setupui.Timeline is
// seeded with. Every name after "Initializing" MUST mirror orchestrate.go's
// Engine.Up exactly — Timeline.Begin/Fail look steps up by exact name, so any
// step name here that doesn't match Up's e.progress(...) calls would silently
// never appear as "done" (Timeline.Begin no-ops on an unrecognized name; see
// model.go).
//
// "Initializing" is the exception: it is NOT an Engine.Up step. It exists so
// the timeline shows an active (spinning) first row the instant the window
// opens, instead of a wall of pending rows during the pre-Up work (orphan-VM
// cleanup, port selection, disk staging). bringUp marks it active up front via
// Timeline.Begin; the first real Up step ("Provisioning disk image") then
// auto-completes it, because Begin marks every earlier step done. See bringUp.
//
// The tunnel step only applies when cfg has an external Channel configured,
// which is unreachable today (see desktop.knownExternalChannelKinds) —
// cfg.Channel is always nil in practice, so this is always the 7 steps.
func bringUpStepNames(cfg desktop.Config) []string {
	steps := []string{
		"Initializing",
		"Provisioning disk image",
		"Booting virtual machine",
		"Waiting for guest network",
		"Fetching cluster credentials",
		"Installing components",
		"Configuring cluster",
	}
	if cfg.Channel != nil {
		steps = append(steps, "Setting up tunnel")
	}
	return steps
}

// mustSelfExe resolves the path to this same running `oap` binary, shared by
// both window spawners (spawnSetupWindow and openSettings) that re-exec it
// as the hidden `desktop-window` subcommand. Despite the name (kept from the
// original inline os.Executable() call this was extracted from), it never
// panics — callers get an error back and are expected to log + notify + bail
// out, same as every other failure path in this file.
func mustSelfExe() (string, error) {
	selfExe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable path: %w", err)
	}
	return selfExe, nil
}

// spawnSetupWindow re-execs this same binary as the hidden `oap
// desktop-window` subcommand (see desktop_window_darwin.go /
// desktop_window_stub.go), pointed at serverURL, as a SEPARATE CHILD
// PROCESS: systray (this process's menubar) and webview (the child's
// native WKWebView window) both need to own their process's main thread
// and cannot share one. The child's stdout/stderr are teed into the same
// log file/writer as everything else in this process.
func (s *desktopState) spawnSetupWindow(serverURL string) error {
	selfExe, err := mustSelfExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(selfExe, "desktop-window", "--url", serverURL, "--title", "OAP Desktop Setup")
	cmd.Stdout = s.logWriter()
	cmd.Stderr = s.logWriter()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start setup window process: %w", err)
	}
	s.windowCmd = cmd
	// Reap the child in the background so it never lingers as a zombie once
	// the user closes the window (or quits it) on their own; stopSetupUI's
	// cleanup path only signals the process (Kill), it never calls Wait
	// itself — Wait must be called exactly once per exec.Cmd, and this is
	// that one call.
	go func() {
		if waitErr := cmd.Wait(); waitErr != nil {
			s.logf("setup window process exited: %v", waitErr)
		}
	}()
	return nil
}

// stopSetupUI shuts down the setup UI's HTTP server and, if still running,
// kills the setup-window child process. Called from every exit path
// (runDesktop's defer, quit(), onExit()) — stopSetupUIOnce makes that safe
// to do redundantly (mirroring the existing eng.Down dual-call-site
// pattern elsewhere in this file). Deliberately best-effort and
// non-blocking: Close (not the graceful Shutdown) stops the HTTP server
// immediately without waiting on in-flight /events SSE connections, and
// Kill is fire-and-forget — a stuck webview process or slow server
// shutdown must never hold up Quit.
func (s *desktopState) stopSetupUI() {
	s.stopSetupUIOnce.Do(func() {
		if s.setupSrv != nil {
			if err := s.setupSrv.Close(); err != nil {
				s.logf("setupui: close server: %v", err)
			}
		}
		if s.windowCmd != nil && s.windowCmd.Process != nil {
			if err := s.windowCmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.logf("setupui: kill setup window process: %v", err)
			}
		}
	})
}

// stopSettingsUI shuts down the settings server and, if still running, kills
// the settings-window child process — mirrors stopSetupUI exactly (see its
// doc for the "Close not Shutdown, Kill is fire-and-forget" reasoning),
// called from the same exit paths (quit(), onExit()). stopSettingsUIOnce
// makes repeat calls safe; settingsMu is taken too, since — unlike
// setupSrv/windowCmd, which are set once before systray.Run — settingsSrv/
// settingsWindowCmd can still be mid-construction on the openSettings click
// path when Quit fires.
func (s *desktopState) stopSettingsUI() {
	s.stopSettingsUIOnce.Do(func() {
		s.settingsMu.Lock()
		defer s.settingsMu.Unlock()
		if s.settingsSrv != nil {
			if err := s.settingsSrv.Close(); err != nil {
				s.logf("settingsui: close server: %v", err)
			}
		}
		if s.settingsWindowCmd != nil && s.settingsWindowCmd.Process != nil {
			if err := s.settingsWindowCmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.logf("settingsui: kill settings window process: %v", err)
			}
		}
	})
}

// forwarderDead reports whether pf's stream has already finished — a
// non-blocking read of the one value portforward.Done is contracted to yield.
//
// Non-blocking is the whole point: this runs on the click path, and a live
// forwarder must cost nothing to check. It consumes the value, which is safe
// only because every caller discards the forwarder immediately afterwards — a
// second call on the same object would block forever on the now-empty channel,
// so this must never be used as a repeatable health probe on a forwarder that
// is being kept.
//
// A nil Done channel (Start never succeeded) reads as NOT dead: such a
// forwarder was never cached in the first place, and blocking-forever on nil
// is the one answer that would be wrong here.
func forwarderDead(pf *portforward.PortForwarder) bool { return doneFired(pf.Done()) }

// doneFired is forwarderDead's whole decision, over the channel rather than
// the forwarder — the split exists so it can be tested. portforward's `done`
// channel is unexported and written only by a successful Start, so there is no
// way to build an already-dead forwarder from this package; over a channel the
// three cases (fired, live, never-started) are three literals.
func doneFired(done <-chan error) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
