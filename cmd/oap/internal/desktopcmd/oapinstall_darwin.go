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
// hasReadme is true it offers a "View README" button. Returns the user's
// choice; a cancel / dialog error maps to installCancel (install nothing).
func installConfirmDialog(title, message string, hasReadme bool) installChoice {
	buttons := `{"Cancel", "Install"}`
	if hasReadme {
		buttons = `{"Cancel", "View README", "Install"}`
	}
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons %s default button "Install" cancel button "Cancel"`,
		message, title, buttons)
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return installCancel // cancel button (osascript exits non-zero) or no display
	}
	if strings.Contains(string(out), "View README") {
		return installViewReadme
	}
	return installConfirm
}

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
	b, err := oap.Unpack(oapBytes)
	if err != nil {
		return nil, fmt.Errorf("unpack .oap: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("invalid .oap bundle: %w", err)
	}
	digest, err := oap.Digest(oapBytes)
	if err != nil {
		return nil, fmt.Errorf("digest .oap: %w", err)
	}
	workflow := newDesktopOapWorkflow(ctx, kb, out, b, namespace, digest)
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

	data, err := os.ReadFile(path)
	if err != nil {
		s.logf("install agent: read %s: %v", path, err)
		notify("OAP Desktop", fmt.Sprintf("Could not read %s: %v", filepath.Base(path), err))
		return
	}

	s.mu.Lock()
	dg := s.dg
	s.mu.Unlock()
	if dg == nil {
		s.logf("install agent: no running cluster yet")
		notify("OAP Desktop", "The local cluster isn't running yet — try again once OAP Desktop finishes starting up.")
		return
	}
	kb, err := dg.Bundle()
	if err != nil {
		s.logf("install agent: connect to local cluster: %v", err)
		notify("OAP Desktop", fmt.Sprintf("Could not connect to the local cluster: %v", err))
		return
	}

	// Confirm-with-preview: if the bundle ships a README, let the user read it
	// and confirm before we apply anything. A README-less bundle keeps the
	// one-click behavior (no dialog).
	if preview, uerr := oap.Unpack(data); uerr == nil && len(preview.Readme) > 0 {
		title, message := installDialogText(preview)
	confirm:
		for {
			switch installConfirmDialog(title, message, true) {
			case installViewReadme:
				openReadme(preview.Manifest.Agent.Name, preview.Readme)
				continue
			case installConfirm:
				break confirm
			default: // installCancel
				s.logf("install agent: %s cancelled at confirm dialog", filepath.Base(path))
				return
			}
		}
	}

	s.setStatus(menubaricons.StateSetup, "Installing agent…", "OAP Desktop — installing agent…")
	defer s.setStatus(menubaricons.StateRunning, "Running", "OAP Desktop — running")

	result, err := installOapFromPath(s.ctx, kb, s.logWriter(), data, demoChatNamespace)
	switch {
	case errors.Is(err, errNeedsConfig):
		s.logf("install agent: %s needs configuration, nothing installed", filepath.Base(path))
		notify("OAP Desktop", "This agent needs configuration — install it from the dashboard.")
		s.openDashboard()
	case err != nil:
		s.logf("install agent: %s: %v", filepath.Base(path), err)
		notify("OAP Desktop", fmt.Sprintf("Could not install %s: %v", filepath.Base(path), err))
	default:
		// result.Adopted is logged here even when empty (kinds/secrets already
		// are) — per Result.Adopted's own doc comment, adoption is recorded
		// NOWHERE on the seized object itself, so this log line is one of the
		// only durable records that it happened.
		s.logf("install agent: installed %s (kinds=%v secrets=%d adopted=%v)", result.Name, result.AppliedKinds, result.SecretsCreated, result.Adopted)
		// Surface every non-fatal warning (e.g. a capacity clamp notice) to the
		// desktop log, and pop ONE notification when there is at least one — the
		// full text is in the log; the banner just says to go look.
		for _, warn := range result.Warnings {
			s.logf("install agent: %s: %s", result.Name, warn)
		}
		switch {
		case len(result.Adopted) > 0 && len(result.Warnings) > 0:
			notify("OAP Desktop", fmt.Sprintf("Installed %s — adopted %d pre-existing object(s) and %d other note(s), see logs", result.Name, len(result.Adopted), len(result.Warnings)))
		case len(result.Adopted) > 0:
			// Adoption overwrote an object this install did not create; the
			// notification exists specifically so a menu-bar user (who never
			// sees a --adopt flag or a confirm prompt's object list scroll by)
			// still learns that it happened.
			notify("OAP Desktop", fmt.Sprintf("Installed %s — adopted %d pre-existing object(s), see logs", result.Name, len(result.Adopted)))
		case len(result.Warnings) > 0:
			notify("OAP Desktop", fmt.Sprintf("Installed %s with %d note(s) — see logs", result.Name, len(result.Warnings)))
		default:
			notify("OAP Desktop", fmt.Sprintf("Installed %s", result.Name))
		}
	}
}
