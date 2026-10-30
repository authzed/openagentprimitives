//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/linkinstall"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/links"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/settingsui"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
)

// linkActions dispatches a parsed link by its registry key. Every key the
// links package registers must have an entry (TestLinkActionsCoverRegistry).
var linkActions = map[string]func(*desktopState, links.Link){
	links.KeyInstallAgent: (*desktopState).installAgentFromLink,
}

// linkNotify surfaces a link outcome to the user. A package var so tests can
// record notifications instead of running osascript. Rate limited: any web
// page can fire oap:// links, and each refusal posts a banner, so without a
// limit a page could keep the user's screen full of them. Every link outcome
// is logged before it is notified, so a dropped banner loses nothing.
var linkNotify = throttledNotify(
	rate.NewLimiter(rate.Every(linkNotifyInterval), linkNotifyBurst),
	func(msg string) { notify("OAP Desktop", msg) },
	func(msg string) { onLinkNotifyDropped(msg) },
)

// linkNotifyBurst link banners may post back to back; after that, one more
// is allowed per linkNotifyInterval. A real click produces at most two (a
// "still starting" hold, then its outcome).
const (
	linkNotifyBurst    = 3
	linkNotifyInterval = 20 * time.Second
)

// onLinkNotifyDropped records a banner the rate limit dropped. runDesktop
// points it at st.logf, like onNotifyFailed.
var onLinkNotifyDropped = func(msg string) {}

// throttledNotify posts through post while limit allows, and hands every
// other message to dropped.
func throttledNotify(limit *rate.Limiter, post, dropped func(string)) func(string) {
	return func(msg string) {
		if !limit.Allow() {
			dropped(msg)
			return
		}
		post(msg)
	}
}

// linkFetchTimeout bounds verify/resolve/pull for one link so a hung registry
// cannot leave the install gate busy forever. The approval dialog runs inside
// Prepare but does not consult the context, so a slow reader is unaffected.
const linkFetchTimeout = 5 * time.Minute

// linkCancelCooldown is how long links are refused after the user cancels an
// approval dialog. A var so tests that are not about the cooldown can zero it.
var linkCancelCooldown = 10 * time.Second

// linkConfirmMinDwell is how long the approval dialog must have been up before
// an Install click counts. A dialog that appears under a click or keypress
// already in progress (a page firing a link as the user clicks) can otherwise
// be approved by it; a too-early Install shows the dialog again instead.
const linkConfirmMinDwell = time.Second

// linkConfirmTooFast is prepended to the dialog shown again after an Install
// that came before linkConfirmMinDwell.
const linkConfirmTooFast = "Install was chosen as soon as this dialog appeared. Review it, then choose Install again."

// dialogClock times the approval dialog. A var so tests can step it.
var dialogClock = time.Now

// errLinkSettings marks a failure to read config.json (and so the allowlist).
var errLinkSettings = errors.New("read OAP Desktop settings")

// linkVerifyError tags a signature-verification failure so it can be told
// apart from other fetch failures after Prepare wraps it.
type linkVerifyError struct{ err error }

func (e *linkVerifyError) Error() string { return e.err.Error() }
func (e *linkVerifyError) Unwrap() error { return e.err }

// prepareLink runs linkinstall.Prepare with the real collaborators: the
// allowlist from config.json (re-read per link, so a Settings save applies to
// the next link), anonymous oci.Options (never AllowHosts), the installed-
// version lookup, and the native approval dialog. A package var so tests stub
// the network and the dialog.
var prepareLink = func(ctx context.Context, supportDir, ref string, installed func(context.Context, *oap.Bundle) (linkinstall.Installed, error)) ([]byte, linkinstall.Prompt, error) {
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
		Pull:      func(ctx context.Context, ref string) ([]byte, string, error) { return oci.Pull(ctx, ref, opts) },
		Installed: installed,
		Confirm:   linkConfirmDialog,
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
// before bring-up finishes (and enableRunningMenu/dropHeldLink sees it) or
// sees the final state, never neither.
func (s *desktopState) installAgentFromLink(l links.Link) {
	ref := l.Params["ref"]
	s.mu.Lock()
	r := linkReadiness(s.phaseLocked())
	d := s.linkGate.Offer(l.Raw, r)
	s.mu.Unlock()
	s.logf("install link: %s ref=%q readiness=%s decision=%s", l.Key, ref, r, d)

	switch d {
	case linkinstall.RejectUnavailable:
		linkNotify("Start OAP Desktop and finish setup, then click the link again.")
	case linkinstall.RejectBusy:
		linkNotify("Another agent install is already in progress.")
	case linkinstall.RejectPending:
		linkNotify("Another agent install is already waiting for OAP Desktop to start.")
	case linkinstall.RejectCoolingDown:
		linkNotify("You just cancelled an agent install. Wait a few seconds, then click the link again.")
	case linkinstall.Held:
		linkNotify("OAP Desktop is still starting — the install will continue once it's ready.")
	case linkinstall.RunNow:
		defer s.releaseLinkGate()
		s.runLinkInstall(l)
	}
}

// runClaimedLink runs a held link already claimed from the gate (by
// enableRunningMenu's running transition, or by drainHeldLink). The gate is
// busy on entry and released — then drained again — however the install ends.
func (s *desktopState) runClaimedLink(raw string) {
	defer s.releaseLinkGate()
	l, err := links.Parse(raw)
	if err != nil { // unreachable: only parsed links are held
		s.logf("install link: held link no longer parses (%d bytes): %v", len(raw), err)
		linkNotify("That isn't a valid OAP Desktop link.")
		return
	}
	s.logf("install link: %s ref=%q running link held during start-up", l.Key, l.Params["ref"])
	s.runLinkInstall(l)
}

// releaseLinkGate ends an install's hold on the gate, then runs any link
// still held so it can never be stranded behind the install that just ended.
func (s *desktopState) releaseLinkGate() {
	s.linkGate.Release()
	s.drainHeldLink()
}

// drainHeldLink claims and runs the held link when the cluster is running and
// the gate is free. Claiming happens under s.mu, like every running-state
// read, so it cannot race Stop. A link still held behind a busy gate is
// logged; the busy install's own release drains it.
func (s *desktopState) drainHeldLink() {
	var raw string
	var claimed, running bool
	s.mu.Lock()
	running = s.running
	if running {
		raw, claimed = s.linkGate.TakePending()
	}
	s.mu.Unlock()
	if claimed {
		s.runClaimedLink(raw)
		return
	}
	if held, ok := s.linkGate.Pending(); ok && running {
		ref := ""
		if l, err := links.Parse(held); err == nil {
			ref = l.Params["ref"]
		}
		s.logf("install link: %q still held; another install in progress", ref)
	}
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

// uninstallDropMessage is the notification for a link held when the user
// confirms Uninstall.
const uninstallDropMessage = "OAP Desktop is uninstalling, so the pending agent install was cancelled."

// stopTakingLinksForUninstall runs once the user has confirmed Uninstall:
// marking the cluster stopped (under s.mu, like every running-state write)
// makes links arriving during the uninstall refused rather than held or run,
// and a link already held during bring-up is dropped with a notification.
func (s *desktopState) stopTakingLinksForUninstall() {
	s.mu.Lock()
	s.markStoppedLocked()
	s.mu.Unlock()
	s.dropHeldLink(uninstallDropMessage)
}

// runLinkInstall fetches, verifies and gets approval for the link's ref, then
// installs the approved bytes. The caller owns the gate.
func (s *desktopState) runLinkInstall(l links.Link) {
	ref := l.Params["ref"]
	ctx, cancel := context.WithTimeout(s.ctx, linkFetchTimeout)
	defer cancel()
	s.logf("install link: %s ref=%q fetching", l.Key, ref)
	data, p, err := prepareLink(ctx, s.support, ref, s.installedLinkAgent)
	if err != nil {
		s.logf("install link: %s ref=%q not installed: %v", l.Key, ref, err)
		if errors.Is(err, linkinstall.ErrCancelled) {
			s.linkGate.CoolDown(linkCancelCooldown)
		}
		if msg, show := linkFailureMessage(ref, err); show {
			linkNotify(msg)
		}
		return
	}
	s.logf("install link: %s ref=%q approved digest=%s verified=%t", l.Key, ref, p.Digest, p.Verified)
	s.installApproved(data, ref, installDisplayName(p.Bundle, ref), bundleSource{Ref: ref, Digest: p.Digest})
}

// installedLinkAgent is linkinstall.Deps.Installed: the live AgentClass the
// bundle would install over, in the namespace link installs land in, and the
// version its oap-source provenance records (status's mirror when the
// annotation was stripped). Not found is "not installed", not an error.
func (s *desktopState) installedLinkAgent(ctx context.Context, b *oap.Bundle) (linkinstall.Installed, error) {
	crs, err := b.CRs()
	if err != nil {
		return linkinstall.Installed{}, err
	}
	root, err := install.FindAgentClass(crs)
	if err != nil {
		return linkinstall.Installed{}, err
	}
	s.mu.Lock()
	dg := s.dg
	s.mu.Unlock()
	if dg == nil {
		return linkinstall.Installed{}, errClusterNotRunning
	}
	kb, err := dg.Bundle()
	if err != nil {
		return linkinstall.Installed{}, fmt.Errorf("connect to the local cluster: %w", err)
	}
	return installedAgentClass(ctx, kb.Controller, client.ObjectKey{Namespace: demoChatNamespace, Name: root.GetName()})
}

// installedAgentClass reads the installed version of the AgentClass at key.
func installedAgentClass(ctx context.Context, c client.Reader, key client.ObjectKey) (linkinstall.Installed, error) {
	var live spiceboxv1alpha1.AgentClass
	if err := c.Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return linkinstall.Installed{}, nil
		}
		return linkinstall.Installed{}, fmt.Errorf("get AgentClass %s: %w", key, err)
	}
	if raw, ok := live.Annotations[instance.AnnotationOapSource]; ok {
		src, err := instance.ParseOapSource(raw)
		if err != nil {
			return linkinstall.Installed{}, fmt.Errorf("AgentClass %s: %w", key, err)
		}
		return linkinstall.Installed{Found: true, Version: src.Version}, nil
	}
	if live.Status.OapInstall != nil {
		return linkinstall.Installed{Found: true, Version: live.Status.OapInstall.Version}, nil
	}
	return linkinstall.Installed{Found: true}, nil
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
	case errors.Is(err, linkinstall.ErrConfirmFailed):
		return dialogFailedMessage, true
	case errors.Is(err, linkinstall.ErrInvalidSource):
		return "OAP Desktop's install-link settings are invalid — check Settings → Install links.", true
	case errors.Is(err, linkinstall.ErrInvalidRef):
		return "That isn't a valid OAP Desktop link.", true
	case errors.Is(err, linkinstall.ErrTrustedKey):
		return "Couldn't load OAP's signing key — see logs.", true
	case errors.Is(err, linkinstall.ErrDowngrade):
		return fmt.Sprintf("%s is older than the agent already installed, so it wasn't installed. To roll back on purpose, use Install agent from .oap….", ref), true
	case errors.Is(err, linkinstall.ErrInstalledLookup):
		return "Couldn't check which version of this agent is installed, so it wasn't installed — see logs.", true
	case errors.Is(err, linkinstall.ErrUnreadableBundle):
		return fmt.Sprintf("Couldn't read what %s would install, so it wasn't installed — see logs.", ref), true
	case errors.Is(err, linkinstall.ErrNotAllowed):
		return fmt.Sprintf("OAP Desktop only installs agents from sources in Settings → Install links. %s isn't one of them.", ref), true
	case errors.As(err, &verr):
		return fmt.Sprintf("Couldn't verify %s was signed by the OAP project, so it wasn't installed.", ref), true
	default:
		// The raw error (registry host, response body) stays in the log;
		// runLinkInstall has already written it there.
		return fmt.Sprintf("Couldn't download %s. See logs for details.", ref), true
	}
}

// linkConfirmDialog is linkinstall.Deps.Confirm: the always-shown approval
// dialog, Cancel by default, looping on View README like the file flow, and
// on an Install chosen before linkConfirmMinDwell. A dialog that could not be
// shown is an error, never a decline.
func linkConfirmDialog(p linkinstall.Prompt) (bool, error) {
	title, message := linkinstall.DialogText(p)
	var readme []byte
	name := "agent"
	if p.Bundle != nil {
		readme = p.Bundle.Readme
		if p.Bundle.Manifest != nil {
			name = p.Bundle.Manifest.Agent.Name
		}
	}
	shown := message
	for {
		start := dialogClock()
		choice, err := installConfirmDialog(title, shown, len(readme) > 0, "Cancel")
		switch choice {
		case installViewReadme:
			openReadme(name, readme)
			shown = message
		case installConfirm:
			if dialogClock().Sub(start) < linkConfirmMinDwell {
				shown = linkConfirmTooFast + "\n\n" + message
				continue
			}
			return true, nil
		case installDialogFailed:
			return false, err
		default: // installCancel
			return false, nil
		}
	}
}

// markStoppedLocked records a stopped cluster; s.mu must be held. Clearing
// the step matters: without it the stale last bring-up step would make
// phase() report "starting" after a stop (and hold links forever).
func (s *desktopState) markStoppedLocked() {
	s.running = false
	s.currentStep = ""
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
