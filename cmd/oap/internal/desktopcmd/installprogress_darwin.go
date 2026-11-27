//go:build darwin && arm64

package desktopcmd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	xbrowser "github.com/authzed/openagentprimitives/pkg/x/browser"
)

// What the user sees once they approve an install (from an oap:// link or the
// "Install agent from .oap…" picker — both reach installApproved): a small
// "Installing <name>…" window with the animated relay mark while the install
// runs, then the new chat with the agent preselected on success, or a native
// dialog on failure. A notification alone was not enough: macOS often drops
// them, so a successful install looked like nothing happened.

const (
	// installingWindowWidth/Height size the progress window: room for the
	// mark and one line of text, much smaller than the setup window.
	installingWindowWidth  = 420
	installingWindowHeight = 320

	// maxDialogMessageRunes caps a dialog's body. Install errors can carry a
	// whole apiserver response; the full text is in the log either way.
	maxDialogMessageRunes = 500
	// maxDialogTitleRunes caps the bundle-supplied name where it lands in a
	// window or dialog title.
	maxDialogTitleRunes = 80
	// maxDialogDescriptionRunes caps a bundle's description in the install
	// confirm dialog.
	maxDialogDescriptionRunes = 300

	// Icons for installResultDialog (AppleScript `with icon` constants).
	dialogIconStop    = "stop"
	dialogIconCaution = "caution"
)

// errClusterNotRunning is an install attempted before bring-up finished (or
// after the cluster stopped). Its text is shown to the user as-is.
var errClusterNotRunning = errors.New("the local cluster isn't running yet — try again once OAP Desktop finishes starting up")

// Seams for the user-visible half of an install, swapped by tests so the
// outcome routing is exercised without a webview, osascript or a browser.
var (
	showInstallingWindow   = (*desktopState).showInstallingWindow
	openAgentChat          = (*desktopState).openAgentChat
	openDashboardForConfig = (*desktopState).openDashboard
	installNotify          = func(msg string) { notify("OAP Desktop", msg) }
	installResultDialog    = func(title, message, icon, button string) error {
		// icon is one of the dialogIcon* constants, an AppleScript identifier
		// rather than text, so it is the one value spliced into the source.
		if out, err := osascript(
			`display dialog (item 1 of argv) with title (item 2 of argv) buttons {item 3 of argv} default button (item 3 of argv) with icon `+icon,
			message, title, button).CombinedOutput(); err != nil {
			return fmt.Errorf("show dialog: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// installDisplayName is the name the user sees for a bundle: its displayName,
// then its agent name, then fallback (the source label) when there is no
// bundle to read.
func installDisplayName(b *oap.Bundle, fallback string) string {
	if b == nil || b.Manifest == nil {
		return fallback
	}
	if name, _ := installDialogText(b); name != "" {
		return name
	}
	return fallback
}

// agentChatURL is the session shell's New-chat picker with one agent
// preselected (pkg/web/webui/sessions/ui/SessionShell.tsx, startRequest).
// Both values are escaped so a namespace or class name cannot forge another
// parameter.
func agentChatURL(base, namespace, agentClass string) string {
	return strings.TrimRight(base, "/") + "/sessions?new=1&ns=" + url.QueryEscape(namespace) + "&agentClass=" + url.QueryEscape(agentClass)
}

// sanitizeDialogText flattens s to one readable line for a dialog: every
// non-printable rune (newlines, control characters, and format runes such as
// U+202E RIGHT-TO-LEFT OVERRIDE or U+200B ZERO WIDTH SPACE that could reorder
// or hide text) becomes a space, runs of spaces collapse to one, and the
// result is capped at maxRunes runes with a trailing ellipsis.
func sanitizeDialogText(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}

// openAgentChat opens the New-chat picker with agentClass preselected, over
// the same webd port-forward openSessions uses.
func (s *desktopState) openAgentChat(namespace, agentClass string) error {
	pf, err := s.ensureWebdPortForward()
	if err != nil {
		return err
	}
	if err := xbrowser.Open(agentChatURL(pf.URL(), namespace, agentClass)); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}

// showInstallingWindow opens the "Installing <name>…" progress window — the
// hidden `oap desktop-window` child, like the setup and settings windows,
// pointed at the setup UI server's /installing page — and returns a closer
// that kills it if it is still open. The closer is idempotent. The window is
// a nicety: a failure to open it is logged and the install goes ahead.
func (s *desktopState) showInstallingWindow(name string) (closeWindow func()) {
	noop := func() {}
	if s.setupSrv == nil {
		s.logf("install agent: no setup UI server; installing %q without a progress window", name)
		return noop
	}
	selfExe, err := mustSelfExe()
	if err != nil {
		s.logf("install agent: progress window: %v", err)
		return noop
	}
	title := "Installing " + sanitizeDialogText(name, maxDialogTitleRunes)
	pageURL := s.setupSrv.URL() + "/installing?name=" + url.QueryEscape(name)
	cmd := exec.Command(selfExe, "desktop-window",
		"--url", pageURL, "--title", title,
		"--width", strconv.Itoa(installingWindowWidth), "--height", strconv.Itoa(installingWindowHeight))
	cmd.Stdout = s.logWriter()
	cmd.Stderr = s.logWriter()
	if err := cmd.Start(); err != nil {
		s.logf("install agent: start progress window: %v", err)
		return noop
	}

	// Reap the child exactly once, here (see spawnSetupWindow). The closer's
	// own kill is expected, so it is not logged as an unexpected exit.
	var killed atomic.Bool
	go func() {
		if waitErr := cmd.Wait(); waitErr != nil && !killed.Load() {
			s.logf("install progress window exited: %v", waitErr)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			killed.Store(true)
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.logf("install agent: close progress window: %v", err)
			}
		})
	}
}

// runInstallWithProgress shows the progress window for the duration of
// doInstall, closes it (on every path, a panic included), then reports the
// outcome. name is what the user sees; label names the source in logs.
func (s *desktopState) runInstallWithProgress(name, label string, doInstall func() (*install.Result, error)) {
	// OnceFunc: closed exactly once whether the explicit call below or the
	// deferred one (a panicking install) gets there first.
	closeWindow := sync.OnceFunc(showInstallingWindow(s, name))
	defer closeWindow()
	result, err := doInstall()
	// Close before reporting: the outcome dialog or browser should not open
	// behind a window that still says "Installing".
	closeWindow()
	s.reportInstallOutcome(name, label, result, err)
}

// reportInstallOutcome logs an install's outcome and shows it to the user:
// the new chat on success, a dialog otherwise.
func (s *desktopState) reportInstallOutcome(name, label string, result *install.Result, err error) {
	switch {
	case errors.Is(err, errNeedsConfig):
		s.logf("install agent: %s needs configuration, nothing installed", label)
		s.showInstallDialog("Finish installing "+name,
			name+" needs configuration before it can run. OAP Desktop will open the dashboard so you can finish installing it.",
			dialogIconCaution, "Open dashboard")
		openDashboardForConfig(s)
	case err != nil:
		s.logf("install agent: %s: %v", label, err)
		s.showInstallDialog("Couldn't install "+name, err.Error(), dialogIconStop, "OK")
	case result == nil:
		// installOapFromPath never returns (nil, nil); guard rather than panic.
		s.logf("install agent: %s: install returned no result", label)
		s.showInstallDialog("Couldn't install "+name, "The install finished without a result — see logs.", dialogIconStop, "OK")
	default:
		s.reportInstallSuccess(name, result)
	}
}

// reportInstallSuccess logs the result and opens the new chat with the
// installed AgentClass preselected. Result.Name is that class's name: the
// desktop workflow never sets InstallOpts.Name, so the install name is the
// bundled AgentClass's own (physical) name.
func (s *desktopState) reportInstallSuccess(name string, result *install.Result) {
	// result.Adopted is logged even when empty (kinds/secrets already are) —
	// per Result.Adopted's own doc comment, adoption is recorded NOWHERE on
	// the seized object itself, so this log line is one of the only durable
	// records that it happened.
	s.logf("install agent: installed %s (kinds=%v secrets=%d adopted=%v)", result.Name, result.AppliedKinds, result.SecretsCreated, result.Adopted)
	for _, warn := range result.Warnings {
		s.logf("install agent: %s: %s", result.Name, warn)
	}
	// Adoption overwrote objects this install did not create; the chat page
	// says nothing about that, so a menu-bar user (who never sees a --adopt
	// flag or a confirm prompt's object list scroll by) still learns it here.
	if len(result.Adopted) > 0 {
		msg := fmt.Sprintf("Installed %s — adopted %d pre-existing object(s), see logs", result.Name, len(result.Adopted))
		if len(result.Warnings) > 0 {
			msg = fmt.Sprintf("Installed %s — adopted %d pre-existing object(s) and %d other note(s), see logs", result.Name, len(result.Adopted), len(result.Warnings))
		}
		installNotify(msg)
	}
	if err := openAgentChat(s, demoChatNamespace, result.Name); err != nil {
		s.logf("install agent: %s installed, but opening its chat failed: %v", result.Name, err)
		s.showInstallDialog("Installed "+name,
			fmt.Sprintf("%s was installed, but OAP Desktop couldn't open the chat: %v", name, err),
			dialogIconCaution, "OK")
	}
}

// showInstallDialog shows an outcome dialog, logging (never dropping) a
// failure to show it. Title and message are sanitized here, so the dialog
// seam only ever sees text safe to embed in an AppleScript string literal.
func (s *desktopState) showInstallDialog(title, message, icon, button string) {
	title = sanitizeDialogText(title, maxDialogTitleRunes)
	message = sanitizeDialogText(message, maxDialogMessageRunes)
	if err := installResultDialog(title, message, icon, button); err != nil {
		s.logf("install agent: %q dialog: %v", title, err)
	}
}
