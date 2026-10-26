//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/linkinstall"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/links"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/settingsui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// linkActions dispatches a parsed link by its registry key. Every key the
// links package registers must have an entry (TestLinkActionsCoverRegistry).
var linkActions = map[string]func(*desktopState, links.Link){
	links.KeyInstallAgent: (*desktopState).installAgentFromLink,
}

// linkNotify surfaces a link outcome to the user. A package var so tests can
// record notifications instead of running osascript.
var linkNotify = func(msg string) { notify("OAP Desktop", msg) }

// linkFetchTimeout bounds verify/resolve/pull for one link so a hung registry
// cannot leave the install gate busy forever. The approval dialog runs inside
// Prepare but does not consult the context, so a slow reader is unaffected.
const linkFetchTimeout = 5 * time.Minute

// errLinkSettings marks a failure to read config.json (and so the allowlist).
var errLinkSettings = errors.New("read OAP Desktop settings")

// linkVerifyError tags a signature-verification failure so it can be told
// apart from other fetch failures after Prepare wraps it.
type linkVerifyError struct{ err error }

func (e *linkVerifyError) Error() string { return e.err.Error() }
func (e *linkVerifyError) Unwrap() error { return e.err }

// prepareLink runs linkinstall.Prepare with the real collaborators: the
// allowlist from config.json (re-read per link, so a Settings save applies to
// the next link), anonymous oci.Options (never AllowHosts), and the native
// approval dialog. A package var so tests stub the network and the dialog.
var prepareLink = func(ctx context.Context, supportDir, ref string) ([]byte, linkinstall.Prompt, error) {
	cfg, err := desktop.Load(supportDir)
	if err != nil {
		return nil, linkinstall.Prompt{}, fmt.Errorf("%w: %w", errLinkSettings, err)
	}
	opts := oci.Options{}
	return linkinstall.Prepare(ctx, ref, linkinstall.Deps{
		Sources: cfg.EffectiveInstallSources(),
		Resolve: func(ctx context.Context, ref string) (string, error) { return oci.Resolve(ctx, ref, opts) },
		Verify: func(ctx context.Context, ref string, pub crypto.PublicKey) (string, error) {
			d, err := oci.Verify(ctx, ref, pub, opts)
			if err != nil {
				return "", &linkVerifyError{err: err}
			}
			return d, nil
		},
		Pull:    func(ctx context.Context, ref string) ([]byte, string, error) { return oci.Pull(ctx, ref, opts) },
		Confirm: linkConfirmDialog,
	})
}

// watchURLEvents feeds every incoming oap:// link to handleLink until the
// desktop context ends. Each link is handled on its own goroutine so a link
// arriving while an approval dialog is up reaches the gate (and is refused as
// busy) instead of queueing silently behind it.
func (s *desktopState) watchURLEvents() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case raw := <-urlEvents:
			go s.handleLink(raw)
		}
	}
}

// handleLink parses an untrusted link and dispatches it by action key.
func (s *desktopState) handleLink(raw string) {
	l, err := links.Parse(raw)
	switch {
	case errors.Is(err, links.ErrUnsupported):
		s.logf("link: refused (%d bytes): %v", len(raw), err)
		linkNotify("This link needs a newer version of OAP Desktop.")
		return
	case err != nil:
		s.logf("link: refused (%d bytes): %v", len(raw), err)
		linkNotify("That isn't a valid OAP Desktop link.")
		return
	}
	action, ok := linkActions[l.Key]
	if !ok {
		s.logf("link: no desktop action for registered key %q", l.Key)
		linkNotify("This link needs a newer version of OAP Desktop.")
		return
	}
	action(s, l)
}

// linkReadiness maps the desktop lifecycle phase onto the gate's readiness:
// only "starting" may hold a link; stopped, failed and first-run setup refuse.
func linkReadiness(st settingsui.State) linkinstall.Readiness {
	switch {
	case st.Running:
		return linkinstall.Ready
	case st.Phase == "starting":
		return linkinstall.Starting
	default:
		return linkinstall.Unavailable
	}
}

// installAgentFromLink offers the link to the gate and acts on the decision.
// Readiness is read and offered under s.mu so the offer is atomic with
// bring-up's running/failed transitions: a link either lands as pending
// before bring-up finishes (and runHeldLink/dropHeldLink sees it) or sees the
// final state, never neither.
func (s *desktopState) installAgentFromLink(l links.Link) {
	ref := l.Params["ref"]
	s.mu.Lock()
	r := linkReadiness(s.phaseLocked())
	d, replaced := s.linkGate.Offer(l.Raw, r)
	s.mu.Unlock()
	s.logf("install link: %s ref=%q readiness=%s decision=%s", l.Key, ref, r, d)

	switch d {
	case linkinstall.RejectUnavailable:
		linkNotify("Start OAP Desktop and finish setup, then click the link again.")
	case linkinstall.RejectBusy:
		linkNotify("Another agent install is already in progress.")
	case linkinstall.Held:
		if replaced != "" {
			s.logf("install link: %s ref=%q replaced an earlier pending link (%d bytes)", l.Key, ref, len(replaced))
		}
		linkNotify("OAP Desktop is still starting — the install will continue once it's ready.")
	case linkinstall.RunNow:
		defer s.linkGate.Release()
		s.runLinkInstall(l)
	}
}

// runHeldLink runs the link held during bring-up, if any. Called once
// bring-up has succeeded; the gate is released however the install ends.
func (s *desktopState) runHeldLink() {
	raw, ok := s.linkGate.TakePending()
	if !ok {
		return
	}
	defer s.linkGate.Release()
	l, err := links.Parse(raw)
	if err != nil { // unreachable: only parsed links are held
		s.logf("install link: held link no longer parses (%d bytes): %v", len(raw), err)
		linkNotify("That isn't a valid OAP Desktop link.")
		return
	}
	s.logf("install link: %s ref=%q running link held during start-up", l.Key, l.Params["ref"])
	s.runLinkInstall(l)
}

// dropHeldLink cancels a link held during bring-up (bring-up failed or the
// cluster stopped), notifying with msg only when one was actually held.
func (s *desktopState) dropHeldLink(msg string) {
	raw, ok := s.linkGate.DropPending()
	if !ok {
		return
	}
	ref := ""
	if l, err := links.Parse(raw); err == nil {
		ref = l.Params["ref"]
	}
	s.logf("install link: dropped held link ref=%q: %s", ref, msg)
	linkNotify(msg)
}

// runLinkInstall fetches, verifies and gets approval for the link's ref, then
// installs the approved bytes. The caller owns the gate.
func (s *desktopState) runLinkInstall(l links.Link) {
	ref := l.Params["ref"]
	ctx, cancel := context.WithTimeout(s.ctx, linkFetchTimeout)
	defer cancel()
	s.logf("install link: %s ref=%q fetching", l.Key, ref)
	data, p, err := prepareLink(ctx, s.support, ref)
	if err != nil {
		s.logf("install link: %s ref=%q not installed: %v", l.Key, ref, err)
		if msg, show := linkFailureMessage(ref, err); show {
			linkNotify(msg)
		}
		return
	}
	s.logf("install link: %s ref=%q approved digest=%s verified=%t", l.Key, ref, p.Digest, p.Verified)
	s.installApproved(data, ref)
}

// linkFailureMessage is the notification for a Prepare failure; show is
// false for a user cancel, which is logged only.
func linkFailureMessage(ref string, err error) (msg string, show bool) {
	var verr *linkVerifyError
	switch {
	case errors.Is(err, linkinstall.ErrCancelled):
		return "", false
	case errors.Is(err, errLinkSettings):
		return "Couldn't read OAP Desktop settings — see logs.", true
	case errors.Is(err, linkinstall.ErrNotAllowed):
		return fmt.Sprintf("OAP Desktop only installs agents from sources in Settings → Install links. %s isn't one of them.", ref), true
	case errors.As(err, &verr):
		return fmt.Sprintf("Couldn't verify %s was signed by the OAP project, so it wasn't installed.", ref), true
	default:
		return fmt.Sprintf("Couldn't download %s: %v", ref, err), true
	}
}

// linkConfirmDialog is linkinstall.Deps.Confirm: the always-shown approval
// dialog, Cancel by default, looping on View README like the file flow.
func linkConfirmDialog(p linkinstall.Prompt) bool {
	title, message := linkinstall.DialogText(p)
	var readme []byte
	name := "agent"
	if p.Bundle != nil {
		readme = p.Bundle.Readme
		if p.Bundle.Manifest != nil {
			name = p.Bundle.Manifest.Agent.Name
		}
	}
	for {
		switch installConfirmDialog(title, message, len(readme) > 0, "Cancel") {
		case installViewReadme:
			openReadme(name, readme)
		case installConfirm:
			return true
		default: // installCancel
			return false
		}
	}
}

// markBringUpStarting records that bring-up is committed, so phase() reports
// "starting" (and a link is held) from that moment rather than only once the
// first Engine.Up step reports — a cold launch BY a link otherwise races it.
// A later, real step is never rewound.
func (s *desktopState) markBringUpStarting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentStep == "" {
		s.currentStep = "Initializing"
	}
}
