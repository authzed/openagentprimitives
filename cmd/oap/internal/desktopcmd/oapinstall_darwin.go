//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// installChoice is the outcome of the pre-install confirm dialog.
type installChoice int

const (
	installCancel installChoice = iota
	installViewReadme
	installConfirm
	// installDialogFailed means the dialog could not be shown (no display,
	// osascript failure). It is not a cancel: the user never saw the prompt.
	installDialogFailed
)

// installDialogText derives the confirm dialog's title and body from the
// bundle: displayName (falling back to agent.name) and the short description.
func installDialogText(b *oap.Bundle) (title, message string) {
	a := b.Manifest.Agent
	title = a.DisplayName
	if title == "" {
		title = a.Name
	}
	message = fmt.Sprintf("Install %s v%s into your local cluster?", title, a.Version)
	if a.Description != "" {
		message = a.Description + "\n\n" + message
	}
	return title, message
}

// installConfirmDialog shows a native confirm dialog before installing. When
// hasReadme is true it offers a "View README" button. defaultButton names the
// button Return presses ("Install" for a file the user just picked, "Cancel"
// for an install link). Returns the user's choice; the Cancel button maps to
// installCancel, and any other failure to installDialogFailed with the error.
// Either way nothing is installed. A package var so tests drive the callers
// without osascript.
var installConfirmDialog = func(title, message string, hasReadme bool, defaultButton string) (installChoice, error) {
	buttons := `{"Cancel", "Install"}`
	if hasReadme {
		buttons = `{"Cancel", "View README", "Install"}`
	}
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons %s default button %q cancel button "Cancel"`,
		message, title, buttons, defaultButton)
	out, err := exec.Command("osascript", "-e", script).Output()
	return classifyInstallDialog(out, err)
}

// osascriptUserCanceled is the AppleScript error number osascript reports
// (on stderr, with a non-zero exit) when the dialog's cancel button is chosen.
const osascriptUserCanceled = "(-128)"

// classifyInstallDialog maps osascript's result for the install dialog onto
// a choice. Only the cancel button's -128 error is a cancel; every other
// error is installDialogFailed, returned with the error so it gets logged.
func classifyInstallDialog(out []byte, err error) (installChoice, error) {
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), osascriptUserCanceled) {
			return installCancel, nil
		}
		return installDialogFailed, fmt.Errorf("show install dialog: %w", err)
	}
	if strings.Contains(string(out), "View README") {
		return installViewReadme, nil
	}
	return installConfirm, nil
}

// dialogFailedMessage is the notification when the install dialog could not
// be shown, for both the file picker and install links.
const dialogFailedMessage = "Couldn't show the install dialog — see logs."

// adoptDialogText renders the adopt confirmation. The menu bar has no
// multi-select affordance, so the dialog is all-or-nothing — which makes
// enumerating every object, and saying plainly what adopting each one costs,
// the only consent this surface can offer.
func adoptDialogText(conflicts []install.Conflict) (title, message string) {
	title = "Adopt existing objects?"
	var b strings.Builder
	b.WriteString("These objects already exist and were not created by this install:\n\n")
	for _, c := range conflicts {
		if c.Secret {
			fmt.Fprintf(&b, "  • %s — overwrites this Secret's data\n", c)
			continue
		}
		fmt.Fprintf(&b, "  • %s\n", c)
	}
	b.WriteString("\nAdopting overwrites each object's spec with this bundle's, and uninstalling the agent will delete it.")
	return title, b.String()
}

// desktopAdoptDialog shows the native confirm. A package var so tests drive
// the decision without osascript, mirroring how installConfirmDialog is
// exercised.
var desktopAdoptDialog = func(title, message string) bool {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"Cancel", "Adopt & Install"} default button "Cancel" cancel button "Cancel"`,
		message, title)
	// Default button is Cancel, not Adopt: a stray Return keypress must not
	// seize somebody's object.
	if _, err := exec.Command("osascript", "-e", script).Output(); err != nil {
		return false // cancel button (non-zero exit) or no display
	}
	return true
}

// desktopAdoptDecision is the InstallOpts.AdoptDecision hook for the menu bar.
// Confirming adopts every listed object; cancelling returns no keys, which
// Install turns into a *ConflictError that aborts with nothing written.
func desktopAdoptDecision(_ context.Context, conflicts []install.Conflict) ([]string, error) {
	title, message := adoptDialogText(conflicts)
	if !desktopAdoptDialog(title, message) {
		return nil, nil
	}
	keys := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		keys = append(keys, c.Key())
	}
	return keys, nil
}

// readmeTempPath returns the temp-file path the README preview is written to,
// with the agent name reduced to a single path element so the file always
// lands directly in dir.
//
// The name arrives from the bundle's own manifest via oap.Unpack, which
// verifies blob digests and hardens layer-tar extraction but never runs
// Manifest.Validate — and the preview dialog calls this BEFORE
// installOapFromPath, the one step that does validate. filepath.Join Cleans
// its arguments, so a raw "../.."-laden name would resolve outside dir
// entirely: the look-before-you-install action would write a file of the
// bundle author's choosing. Reducing to filepath.Base makes that
// unrepresentable rather than merely unlikely.
func readmeTempPath(dir, name string) string {
	base := filepath.Base(filepath.Clean(name))
	// Clean maps "", ".." and "/" to path elements that are not a usable
	// file-name stem; name the file after nothing rather than hide it.
	if base == "." || base == ".." || base == string(filepath.Separator) {
		base = "agent"
	}
	return filepath.Join(dir, base+"-README.md")
}

// openReadme writes the bundle README to a temp file and opens it in the
// default viewer so the user can read rendered markdown before installing.
func openReadme(name string, readme []byte) {
	f := readmeTempPath(os.TempDir(), name)
	if err := os.WriteFile(f, readme, 0o644); err != nil {
		notify("OAP Desktop", fmt.Sprintf("Could not open README: %v", err))
		return
	}
	if err := exec.Command("open", f).Run(); err != nil {
		notify("OAP Desktop", fmt.Sprintf("Could not open README: %v", err))
	}
}

// errNeedsConfig is returned by installOapFromPath when the bundle carries at
// least one REQUIRED install question with no manifest default. The menu bar
// has no form to collect an answer — unlike the CLI's --set/--values/huh
// prompt (cmd/oap/internal/agentcmd/install.go) or the admin UI's
// missing-questions form (pkg/web/admind/oapinstall.go) — so this defers to
// the dashboard's install flow instead of hanging or guessing a value.
// installOapFromPath applies NOTHING when it returns this error.
var errNeedsConfig = errors.New("agent needs configuration before it can be installed from the menu bar")

// installOapFromPath validates a packed graph, plans every node through the
// shared workflow without prompting, and applies it leaves first. Any required
// question without a default anywhere in the graph returns errNeedsConfig
// before mutation. Capacity suggestions are accepted non-interactively;
// adoption remains one native, path-qualified all-or-nothing dialog per node.
func installOapFromPath(ctx context.Context, kb *kube.Bundle, out io.Writer, oapBytes []byte, namespace string) (*install.Result, error) {
	return installOapBundle(ctx, kb, out, oapBytes, namespace, bundleSource{})
}

// bundleSource is the provenance stamped onto an install (see
// install.InstallOpts.SourceKind). The zero value is a local file: its
// digest is then the bundle's own oap.Digest. An install link is a registry
// source, recorded with the link's ref and the OCI digest Prepare verified or
// resolved, so the install can be traced back to exactly what was approved.
type bundleSource struct {
	Ref, Digest string
}

// installOapBundle is installOapFromPath with explicit provenance.
func installOapBundle(ctx context.Context, kb *kube.Bundle, out io.Writer, oapBytes []byte, namespace string, src bundleSource) (*install.Result, error) {
	b, err := oap.Unpack(oapBytes)
	if err != nil {
		return nil, fmt.Errorf("unpack .oap: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("invalid .oap bundle: %w", err)
	}
	opts := install.InstallOpts{SourceKind: "file", SourceDigest: src.Digest}
	if src.Ref != "" {
		opts.SourceKind, opts.SourceRef = "registry", src.Ref
	} else {
		if opts.SourceDigest, err = oap.Digest(oapBytes); err != nil {
			return nil, fmt.Errorf("digest .oap: %w", err)
		}
	}
	workflow := newDesktopOapWorkflow(ctx, kb, out, b, namespace, opts)
	plan, err := workflow.Plan(ctx, b, install.GraphAnswers{Interactive: false})
	if err != nil {
		var missing *install.MissingAnswersError
		if errors.As(err, &missing) {
			return nil, errNeedsConfig
		}
		return nil, err
	}
	graphResult, err := workflow.Execute(ctx, plan)
	if err != nil {
		// Workflow errors already carry the install and logical agent path.
		// Returned as-is so the caller
		// (installAgentFromFile) surfaces it via a native notification, which
		// has no CLI flag to prescribe, so it deliberately does NOT get the
		// --adopt remediation cmd/oap's wrapConflictError adds — see
		// install.ConflictError's doc comment for why the library error itself
		// stays flag-free.
		return nil, err
	}
	writeDesktopGraphResult(out, graphResult)
	result := desktopRootResult(graphResult)
	if result == nil {
		return nil, fmt.Errorf("install graph returned no root result")
	}
	return result, nil
}

// chooseOapFile shows the native macOS "choose file" dialog restricted to
// .oap files and returns the chosen file's POSIX path. Mirrors notify/
// confirmDialog's osascript-wrapper style. False means the user cancelled —
// osascript exits non-zero on "User canceled." — or the dialog otherwise
// couldn't be shown (e.g. no display attached); either way the caller treats
// it identically as "nothing to install", never as an error to surface.
func chooseOapFile() (string, bool) {
	script := `POSIX path of (choose file of type {"oap"} with prompt "Choose a .oap agent bundle")`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return "", false
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", false
	}
	return path, true
}

// installAgentFromFile is the "Install agent from .oap…" menu action: choose
// a .oap file via the native file picker, read it, and install it into
// demoChatNamespace — the same namespace the built-in chat's agent selector
// lists from (see chatSessionNamespace/applyDemoAgentClass) — so a
// freshly-installed agent shows up in "New chat" immediately. The status icon
// flips to a working state for the duration and is restored to Running
// afterward regardless of outcome: the cluster itself stays up whether the
// install succeeded, needed configuration, or failed outright.
func (s *desktopState) installAgentFromFile() {
	path, ok := chooseOapFile()
	if !ok {
		return // user cancelled (or no display) — not an error, nothing to notify
	}
	label := filepath.Base(path)

	data, err := os.ReadFile(path)
	if err != nil {
		s.logf("install agent: read %s: %v", path, err)
		notify("OAP Desktop", fmt.Sprintf("Could not read %s: %v", label, err))
		return
	}

	// Confirm-with-preview: if the bundle ships a README, let the user read it
	// and confirm before we apply anything. A README-less bundle keeps the
	// one-click behavior (no dialog).
	// An unpack failure here is not reported on its own: installOapFromPath
	// unpacks again and reports it as the install's failure.
	preview, uerr := oap.Unpack(data)
	if uerr != nil {
		preview = nil
	}
	if preview != nil && len(preview.Readme) > 0 {
		title, message := installDialogText(preview)
	confirm:
		for {
			choice, derr := installConfirmDialog(title, message, true, "Install")
			switch choice {
			case installViewReadme:
				openReadme(preview.Manifest.Agent.Name, preview.Readme)
				continue
			case installConfirm:
				break confirm
			case installDialogFailed:
				s.logf("install agent: %s: %v", label, derr)
				notify("OAP Desktop", dialogFailedMessage)
				return
			default: // installCancel
				s.logf("install agent: %s cancelled at confirm dialog", label)
				return
			}
		}
	}

	s.installApproved(data, label, installDisplayName(preview, label), bundleSource{})
}

// installApproved installs bundle bytes the user has already approved into
// demoChatNamespace — the post-approval half shared by the .oap file picker
// and oap:// install links. label names the source in logs (a file name or an
// OCI ref); name is what the user sees (see installDisplayName). The outcome
// is shown by runInstallWithProgress (installprogress_darwin.go): a progress
// window while it runs, then the new chat or a dialog. src is the provenance
// recorded on the install.
func (s *desktopState) installApproved(data []byte, label, name string, src bundleSource) {
	s.mu.Lock()
	dg := s.dg
	s.mu.Unlock()
	// Fail before the progress window opens: an install that cannot start
	// should not flash a window up first.
	if dg == nil {
		s.reportInstallOutcome(name, label, nil, errClusterNotRunning)
		return
	}
	kb, err := dg.Bundle()
	if err != nil {
		s.reportInstallOutcome(name, label, nil, fmt.Errorf("could not connect to the local cluster: %w", err))
		return
	}

	s.runInstallWithProgress(name, label, func() (*install.Result, error) {
		s.setStatus(menubaricons.StateSetup, "Installing agent…", "OAP Desktop — installing agent…")
		defer s.setStatus(menubaricons.StateRunning, "Running", "OAP Desktop — running")
		return installOapBundle(s.ctx, kb, s.logWriter(), data, demoChatNamespace, src)
	})
}
