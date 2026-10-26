//go:build darwin && arm64

package desktopcmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		{name: "digest mismatch: download message", err: linkinstall.ErrDigestMismatch, want: "Couldn't download " + testLinkRef + ": " + linkinstall.ErrDigestMismatch.Error(), wantShow: true},
		{name: "other fetch error: download message", err: errors.New("pull: 404"), want: "Couldn't download " + testLinkRef + ": pull: 404", wantShow: true},
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

func TestRunHeldLinkRunsAndReleases(t *testing.T) {
	got := captureLinkNotify(t)
	var fetched []string
	stubPrepareLink(t, func(_ context.Context, _, ref string) ([]byte, linkinstall.Prompt, error) {
		fetched = append(fetched, ref)
		return nil, linkinstall.Prompt{}, linkinstall.ErrCancelled
	})
	s := linkStateForTest(t)
	s.markBringUpStarting()
	s.handleLink(testInstallLink(testLinkRef))
	require.Len(t, *got, 1, "the hold itself notifies")

	s.running = true
	s.runHeldLink()
	assert.Equal(t, []string{testLinkRef}, fetched, "the held link runs through the install path once")
	d, _ := s.linkGate.Offer("next", linkinstall.Ready)
	assert.Equal(t, linkinstall.RunNow, d, "the gate is released after the held install")

	s.linkGate.Release()
	s.runHeldLink()
	assert.Len(t, fetched, 1, "nothing held: runHeldLink is a no-op")
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
