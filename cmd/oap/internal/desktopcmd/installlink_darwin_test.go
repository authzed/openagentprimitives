//go:build darwin && arm64

package desktopcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/linkinstall"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/links"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/settingsui"
)

const testLinkRef = "ghcr.io/acme/agents/demo-agent:1.0"

func testInstallLink(ref string) string { return "oap://install/agent?v=1&ref=" + ref }

// captureLinkNotify swaps linkNotify for a recorder for the test's duration.
func captureLinkNotify(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := linkNotify
	linkNotify = func(msg string) { got = append(got, msg) }
	t.Cleanup(func() { linkNotify = orig })
	return &got
}

// stubPrepareLink swaps prepareLink for fn and counts its calls.
func stubPrepareLink(t *testing.T, fn func(ctx context.Context, supportDir, ref string) ([]byte, linkinstall.Prompt, error)) *int {
	t.Helper()
	calls := 0
	orig := prepareLink
	prepareLink = func(ctx context.Context, supportDir, ref string) ([]byte, linkinstall.Prompt, error) {
		calls++
		return fn(ctx, supportDir, ref)
	}
	t.Cleanup(func() { prepareLink = orig })
	return &calls
}

// linkStateForTest is stateForTest plus the ctx the fetch path derives from.
func linkStateForTest(t *testing.T) *desktopState {
	t.Helper()
	s := stateForTest(t)
	s.ctx = t.Context()
	return s
}

func TestLinkActionsCoverRegistry(t *testing.T) {
	for _, k := range links.Keys() {
		assert.Contains(t, linkActions, k, "registered link key %q has no desktop action", k)
	}
}

func TestPhase(t *testing.T) {
	cases := []struct {
		name  string
		set   func(s *desktopState)
		want  settingsui.State
		ready linkinstall.Readiness
	}{
		{name: "nothing started: stopped/Unavailable", set: func(*desktopState) {}, want: settingsui.State{Phase: "stopped"}, ready: linkinstall.Unavailable},
		{name: "bring-up marked: starting/Starting", set: func(s *desktopState) { s.markBringUpStarting() }, want: settingsui.State{Phase: "starting", Step: "Initializing"}, ready: linkinstall.Starting},
		{name: "mid bring-up step: starting/Starting", set: func(s *desktopState) { s.currentStep = "Booting virtual machine" }, want: settingsui.State{Phase: "starting", Step: "Booting virtual machine"}, ready: linkinstall.Starting},
		{name: "running: running/Ready", set: func(s *desktopState) { s.currentStep = "Configuring cluster"; s.running = true }, want: settingsui.State{Running: true, Phase: "running"}, ready: linkinstall.Ready},
		{name: "bring-up failed: failed/Unavailable", set: func(s *desktopState) { s.currentStep = "Booting virtual machine"; s.runErr = errors.New("boom") }, want: settingsui.State{Phase: "failed", Step: "Booting virtual machine"}, ready: linkinstall.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := linkStateForTest(t)
			tc.set(s)
			got := s.phase()
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.ready, linkReadiness(got))
		})
	}
}

func TestMarkBringUpStartingKeepsALaterStep(t *testing.T) {
	s := linkStateForTest(t)
	s.currentStep = "Booting virtual machine"
	s.markBringUpStarting()
	assert.Equal(t, "Booting virtual machine", s.phase().Step, "a real step must not be rewound to Initializing")
}

func TestLinkFailureMessage(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     string
		wantShow bool
	}{
		{name: "not allowed: points at Settings", err: fmt.Errorf("%q: %w", testLinkRef, linkinstall.ErrNotAllowed), want: "OAP Desktop only installs agents from sources in Settings → Install links. " + testLinkRef + " isn't one of them.", wantShow: true},
		{name: "cancelled: no notification", err: fmt.Errorf("%q: %w", testLinkRef, linkinstall.ErrCancelled), wantShow: false},
		{name: "verify failure: signature message", err: fmt.Errorf("verify %q: %w", testLinkRef, &linkVerifyError{err: errors.New("no signature")}), want: "Couldn't verify " + testLinkRef + " was signed by the OAP project, so it wasn't installed.", wantShow: true},
		{name: "settings unreadable: see logs", err: fmt.Errorf("%w: %w", errLinkSettings, errors.New("eof")), want: "Couldn't read OAP Desktop settings — see logs.", wantShow: true},
		{name: "malformed install source: settings-invalid message", err: fmt.Errorf("x: %w: %w", linkinstall.ErrInvalidSource, errors.New("must end with '/'")), want: "OAP Desktop's install-link settings are invalid — check Settings → Install links.", wantShow: true},
		{name: "unusable ref: invalid-link message", err: fmt.Errorf("x: %w: %w", linkinstall.ErrInvalidRef, errors.New("bad ref")), want: "That isn't a valid OAP Desktop link.", wantShow: true},
		{name: "signing key unavailable: key message", err: fmt.Errorf("x: %w: %w", linkinstall.ErrTrustedKey, errors.New("no PEM")), want: "Couldn't load OAP's signing key — see logs.", wantShow: true},
		{name: "dialog failed: shown, not treated as a cancel", err: fmt.Errorf("x: %w: %w", linkinstall.ErrConfirmFailed, errors.New("no display")), want: "Couldn't show the install dialog — see logs.", wantShow: true},
		{name: "digest mismatch: download message without the raw error", err: linkinstall.ErrDigestMismatch, want: "Couldn't download " + testLinkRef + ". See logs for details.", wantShow: true},
		{name: "registry error: download message without the raw error", err: errors.New("pull: GET https://registry.internal.example/v2/: 401 body"), want: "Couldn't download " + testLinkRef + ". See logs for details.", wantShow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, show := linkFailureMessage(testLinkRef, tc.err)
			assert.Equal(t, tc.wantShow, show)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHandleLinkRejectsBadLinks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed: invalid-link notification", raw: "oap://install/agent?ref=x", want: "That isn't a valid OAP Desktop link."},
		{name: "unknown action: needs-newer notification", raw: "oap://open/session/x?v=1", want: "This link needs a newer version of OAP Desktop."},
		{name: "newer version: needs-newer notification", raw: "oap://install/agent?v=2&ref=x", want: "This link needs a newer version of OAP Desktop."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureLinkNotify(t)
			calls := stubPrepareLink(t, func(context.Context, string, string) ([]byte, linkinstall.Prompt, error) {
				return nil, linkinstall.Prompt{}, errors.New("must not be called")
			})
			s := linkStateForTest(t)
			s.running = true
			s.handleLink(tc.raw)
			assert.Equal(t, []string{tc.want}, *got)
			assert.Zero(t, *calls, "a rejected link must never reach the fetch path")
		})
	}
}

func TestInstallAgentFromLinkGateDecisions(t *testing.T) {
	cases := []struct {
		name      string
		set       func(s *desktopState)
		want      []string
		wantFetch int
		wantHeld  bool
	}{
		{name: "stopped: refused, nothing held", set: func(*desktopState) {}, want: []string{"Start OAP Desktop and finish setup, then click the link again."}},
		{name: "failed: refused, nothing held", set: func(s *desktopState) { s.runErr = errors.New("boom") }, want: []string{"Start OAP Desktop and finish setup, then click the link again."}},
		{name: "starting: held with notification", set: func(s *desktopState) { s.markBringUpStarting() }, want: []string{"OAP Desktop is still starting — the install will continue once it's ready."}, wantHeld: true},
		{name: "busy: refused", set: func(s *desktopState) { s.running = true; _, _ = s.linkGate.Offer("other", linkinstall.Ready) }, want: []string{"Another agent install is already in progress."}},
		{name: "ready, refused by allowlist: notified and gate released", set: func(s *desktopState) { s.running = true }, want: []string{"OAP Desktop only installs agents from sources in Settings → Install links. " + testLinkRef + " isn't one of them."}, wantFetch: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureLinkNotify(t)
			calls := stubPrepareLink(t, func(_ context.Context, _, ref string) ([]byte, linkinstall.Prompt, error) {
				return nil, linkinstall.Prompt{}, fmt.Errorf("%q: %w", ref, linkinstall.ErrNotAllowed)
			})
			s := linkStateForTest(t)
			tc.set(s)
			s.handleLink(testInstallLink(testLinkRef))
			assert.Equal(t, tc.want, *got)
			assert.Equal(t, tc.wantFetch, *calls)
			held, ok := s.linkGate.DropPending()
			assert.Equal(t, tc.wantHeld, ok)
			if tc.wantHeld {
				assert.Equal(t, testInstallLink(testLinkRef), held)
			}
		})
	}
}

// TestRunNowAlwaysReleasesTheGate: after an install attempt ends — however it
// ends — the next link must be offered a fresh RunNow, never RejectBusy.
func TestRunNowAlwaysReleasesTheGate(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "cancelled at dialog: released", err: linkinstall.ErrCancelled},
		{name: "fetch failed: released", err: errors.New("pull: 404")},
		{name: "settings unreadable: released", err: fmt.Errorf("%w: %w", errLinkSettings, errors.New("eof"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLinkNotify(t)
			stubPrepareLink(t, func(context.Context, string, string) ([]byte, linkinstall.Prompt, error) {
				return nil, linkinstall.Prompt{}, tc.err
			})
			s := linkStateForTest(t)
			s.running = true
			s.handleLink(testInstallLink(testLinkRef))
			d, _ := s.linkGate.Offer("next", linkinstall.Ready)
			assert.Equal(t, linkinstall.RunNow, d)
		})
	}
}

// recordFetches stubs prepareLink to record each fetched ref and end the
// install as a user cancel (the gate is released either way).
func recordFetches(t *testing.T) *[]string {
	t.Helper()
	var fetched []string
	stubPrepareLink(t, func(_ context.Context, _, ref string) ([]byte, linkinstall.Prompt, error) {
		fetched = append(fetched, ref)
		return nil, linkinstall.Prompt{}, linkinstall.ErrCancelled
	})
	return &fetched
}

const otherLinkRef = "ghcr.io/acme/agents/other-agent:1.0"

// TestHeldLinkSurvivesAReadyLinkDuringTheRunningTransition: the held link is
// claimed in the same critical section that flips running, so a Ready link
// arriving before the held install launches is refused as busy instead of
// taking the gate and stranding the held one.
func TestHeldLinkSurvivesAReadyLinkDuringTheRunningTransition(t *testing.T) {
	got := captureLinkNotify(t)
	fetched := recordFetches(t)
	s := linkStateForTest(t)
	s.markBringUpStarting()
	s.handleLink(testInstallLink(testLinkRef))

	held, claimed := s.enableRunningMenu()
	require.True(t, claimed, "the running transition claims the held link")
	assert.Equal(t, testInstallLink(testLinkRef), held)

	s.handleLink(testInstallLink(otherLinkRef)) // arrives before the held install launches
	s.runClaimedLink(held)

	assert.Equal(t, []string{testLinkRef}, *fetched, "the held link runs exactly once; the late one never fetches")
	assert.Equal(t, []string{
		"OAP Desktop is still starting — the install will continue once it's ready.",
		"Another agent install is already in progress.",
	}, *got)
	d, _ := s.linkGate.Offer("next", linkinstall.Ready)
	assert.Equal(t, linkinstall.RunNow, d, "the gate is released after the held install")
}

// TestReleaseDrainsALeftoverHeldLink: if a link is ever still held when an
// install releases the gate, the release runs it rather than stranding it.
func TestReleaseDrainsALeftoverHeldLink(t *testing.T) {
	captureLinkNotify(t)
	fetched := recordFetches(t)
	s := linkStateForTest(t)
	_, _ = s.linkGate.Offer(testInstallLink(testLinkRef), linkinstall.Starting)
	s.running = true

	s.handleLink(testInstallLink(otherLinkRef)) // RunNow; its release drains the held link

	assert.Equal(t, []string{otherLinkRef, testLinkRef}, *fetched, "the leftover held link runs once, after the in-flight one")
	_, pending := s.linkGate.Pending()
	assert.False(t, pending, "nothing left held")
	d, _ := s.linkGate.Offer("next", linkinstall.Ready)
	assert.Equal(t, linkinstall.RunNow, d, "the gate is released after the drained install")
}

func TestDrainHeldLink(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		busy    bool
		wantLog string
	}{
		{name: "running, gate busy: stays held and is logged", running: true, busy: true, wantLog: "still held; another install in progress"},
		{name: "not running: stays held, not run", running: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLinkNotify(t)
			fetched := recordFetches(t)
			s := linkStateForTest(t)
			_, _ = s.linkGate.Offer(testInstallLink(testLinkRef), linkinstall.Starting)
			if tc.busy {
				_, _ = s.linkGate.Offer("in-flight", linkinstall.Ready)
			}
			s.running = tc.running

			s.drainHeldLink()

			assert.Empty(t, *fetched)
			held, ok := s.linkGate.Pending()
			assert.True(t, ok)
			assert.Equal(t, testInstallLink(testLinkRef), held)
			if tc.wantLog != "" {
				assert.Contains(t, s.out.(*bytes.Buffer).String(), tc.wantLog)
				assert.Contains(t, s.out.(*bytes.Buffer).String(), testLinkRef)
			}
		})
	}
}

func TestMarkStoppedLockedReportsStopped(t *testing.T) {
	s := linkStateForTest(t)
	s.running, s.currentStep = true, "Configuring cluster"
	s.mu.Lock()
	s.markStoppedLocked()
	got := s.phaseLocked()
	s.mu.Unlock()
	assert.Equal(t, settingsui.State{Phase: "stopped"}, got, "after Stop the stale last step must not read as starting")
}

func TestDropHeldLinkNotifiesOnlyWhenSomethingWasHeld(t *testing.T) {
	got := captureLinkNotify(t)
	stubPrepareLink(t, func(context.Context, string, string) ([]byte, linkinstall.Prompt, error) {
		return nil, linkinstall.Prompt{}, errors.New("must not be called")
	})
	s := linkStateForTest(t)
	s.dropHeldLink("dropped")
	assert.Empty(t, *got, "nothing held: no notification")

	s.markBringUpStarting()
	s.handleLink(testInstallLink(testLinkRef))
	s.dropHeldLink("dropped")
	assert.Equal(t, []string{"OAP Desktop is still starting — the install will continue once it's ready.", "dropped"}, *got)
	_, ok := s.linkGate.DropPending()
	assert.False(t, ok, "the held link is gone")
}

func TestStopTakingLinksForUninstall(t *testing.T) {
	got := captureLinkNotify(t)
	calls := stubPrepareLink(t, func(context.Context, string, string) ([]byte, linkinstall.Prompt, error) {
		return nil, linkinstall.Prompt{}, errors.New("must not be called")
	})
	s := linkStateForTest(t)
	s.markBringUpStarting()
	s.handleLink(testInstallLink(testLinkRef)) // held during start-up

	s.stopTakingLinksForUninstall()
	_, held := s.linkGate.Pending()
	assert.False(t, held, "the held link is dropped")

	s.handleLink(testInstallLink(testLinkRef)) // arrives mid-uninstall
	assert.Equal(t, []string{
		"OAP Desktop is still starting — the install will continue once it's ready.",
		uninstallDropMessage,
		"Start OAP Desktop and finish setup, then click the link again.",
	}, *got)
	_, held = s.linkGate.Pending()
	assert.False(t, held, "a link arriving during uninstall is refused, not held")
	assert.Zero(t, *calls, "nothing reaches the fetch path")
}

func TestLinkConfirmDialog(t *testing.T) {
	boom := errors.New("osascript: no display")
	cases := []struct {
		name    string
		choice  installChoice
		err     error
		want    bool
		wantErr error
	}{
		{name: "Install: approved", choice: installConfirm, want: true},
		{name: "Cancel: declined, no error", choice: installCancel},
		{name: "dialog failed: error, not a decline", choice: installDialogFailed, err: boom, wantErr: boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := installConfirmDialog
			installConfirmDialog = func(string, string, bool, string) (installChoice, error) { return tc.choice, tc.err }
			t.Cleanup(func() { installConfirmDialog = orig })
			got, err := linkConfirmDialog(linkinstall.Prompt{Ref: testLinkRef, Digest: "sha256:aa"})
			assert.Equal(t, tc.want, got)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestThrottledNotifyDropsBannersPastTheBurst(t *testing.T) {
	var posted, dropped []string
	// rate.Every(time.Hour) so no token refills while the test runs.
	n := throttledNotify(rate.NewLimiter(rate.Every(time.Hour), 2),
		func(m string) { posted = append(posted, m) },
		func(m string) { dropped = append(dropped, m) })
	for _, m := range []string{"a", "b", "c", "d"} {
		n(m)
	}
	assert.Equal(t, []string{"a", "b"}, posted, "the burst posts")
	assert.Equal(t, []string{"c", "d"}, dropped, "everything past the burst is handed to dropped, not lost silently")
}
