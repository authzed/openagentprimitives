//go:build darwin && arm64

package desktopcmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// installUIRecorder captures every user-visible effect of an install outcome,
// in order, so a test can assert both what happened and that the progress
// window closed before any dialog appeared.
type installUIRecorder struct {
	events  []string
	dialogs []recordedDialog
	notes   []string
	chats   [][2]string
	// chatErr / dialogErr are returned by the stubbed browser open / dialog.
	chatErr   error
	dialogErr error
}

type recordedDialog struct{ title, message, icon, button string }

// stubInstallUI swaps every install-outcome seam for a recorder.
func stubInstallUI(t *testing.T) *installUIRecorder {
	t.Helper()
	rec := &installUIRecorder{}
	origWindow, origChat, origDialog, origNotify, origDash := showInstallingWindow, openAgentChat, installResultDialog, installNotify, openDashboardForConfig
	t.Cleanup(func() {
		showInstallingWindow, openAgentChat, installResultDialog, installNotify, openDashboardForConfig = origWindow, origChat, origDialog, origNotify, origDash
	})
	showInstallingWindow = func(_ *desktopState, name string) func() {
		rec.events = append(rec.events, "window-open:"+name)
		return func() { rec.events = append(rec.events, "window-close") }
	}
	openAgentChat = func(_ *desktopState, ns, class string) error {
		rec.events = append(rec.events, "chat")
		rec.chats = append(rec.chats, [2]string{ns, class})
		return rec.chatErr
	}
	installResultDialog = func(title, message, icon, button string) error {
		rec.events = append(rec.events, "dialog")
		rec.dialogs = append(rec.dialogs, recordedDialog{title, message, icon, button})
		return rec.dialogErr
	}
	installNotify = func(msg string) {
		rec.events = append(rec.events, "notify")
		rec.notes = append(rec.notes, msg)
	}
	openDashboardForConfig = func(*desktopState) { rec.events = append(rec.events, "dashboard") }
	return rec
}

func TestAgentChatURL(t *testing.T) {
	cases := []struct {
		name, base, ns, class, want string
	}{
		{name: "plain values: new-chat picker with agent preselected", base: "http://127.0.0.1:8080", ns: "default", class: "demo-agent", want: "http://127.0.0.1:8080/sessions?new=1&ns=default&agentClass=demo-agent"},
		{name: "trailing slash on base: not doubled", base: "http://127.0.0.1:8080/", ns: "default", class: "demo-agent", want: "http://127.0.0.1:8080/sessions?new=1&ns=default&agentClass=demo-agent"},
		{name: "reserved bytes: escaped so no second parameter can be forged", base: "http://h", ns: "a&b=c", class: "x y#z", want: "http://h/sessions?new=1&ns=a%26b%3Dc&agentClass=x+y%23z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, agentChatURL(tc.base, tc.ns, tc.class))
		})
	}
}

func TestSanitizeDialogText(t *testing.T) {
	long := strings.Repeat("é", 600)
	cases := []struct {
		name, in, want string
	}{
		{name: "plain text: unchanged", in: "install failed: boom", want: "install failed: boom"},
		{name: "newlines and tabs: flattened to single spaces", in: "line one\n\n\tline two\r\nthree", want: "line one line two three"},
		{name: "other control chars: dropped to spaces, not passed to osascript", in: "a\x00b\x1bc\x7fd", want: "a b c d"},
		{name: "leading/trailing whitespace: trimmed", in: "  \n x \n ", want: "x"},
		{name: "format runes: RLO, zero-width space and BOM become spaces", in: "safe\u202Eexe.txt\u200Bx\uFEFFy", want: "safe exe.txt x y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeDialogText(tc.in, maxDialogMessageRunes))
		})
	}

	t.Run("over the cap: truncated by runes with an ellipsis, still valid UTF-8", func(t *testing.T) {
		got := sanitizeDialogText(long, maxDialogMessageRunes)
		assert.True(t, utf8.ValidString(got))
		assert.Equal(t, maxDialogMessageRunes+1, utf8.RuneCountInString(got))
		assert.True(t, strings.HasSuffix(got, "…"))
	})
}

func TestInstallDisplayName(t *testing.T) {
	withDisplay := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "demo-agent", DisplayName: "Demo Agent"}}}
	nameOnly := &oap.Bundle{Manifest: &oap.Manifest{Agent: oap.Agent{Name: "demo-agent"}}}
	assert.Equal(t, "Demo Agent", installDisplayName(withDisplay, "fallback"), "displayName wins")
	assert.Equal(t, "demo-agent", installDisplayName(nameOnly, "fallback"), "agent name when no displayName")
	assert.Equal(t, "fallback", installDisplayName(nil, "fallback"), "no bundle: the source label")
	assert.Equal(t, "fallback", installDisplayName(&oap.Bundle{}, "fallback"), "bundle without a manifest: the source label")
}

func TestRunInstallWithProgress_RoutesEachOutcome(t *testing.T) {
	installed := &install.Result{Name: "demo-agent"}
	cases := []struct {
		name       string
		result     *install.Result
		err        error
		chatErr    error
		wantEvents []string
		check      func(t *testing.T, rec *installUIRecorder)
	}{
		{
			name:       "success: window closed, then the new chat opens on the installed AgentClass; no dialog or notification",
			result:     installed,
			wantEvents: []string{"window-open:Demo", "window-close", "chat"},
			check: func(t *testing.T, rec *installUIRecorder) {
				assert.Equal(t, [][2]string{{demoChatNamespace, "demo-agent"}}, rec.chats)
			},
		},
		{
			name:       "success with adoption: chat opens and the adoption notice is still posted",
			result:     &install.Result{Name: "demo-agent", Adopted: []string{"Secret/x"}},
			wantEvents: []string{"window-open:Demo", "window-close", "notify", "chat"},
			check: func(t *testing.T, rec *installUIRecorder) {
				require.Len(t, rec.notes, 1)
				assert.Contains(t, rec.notes[0], "adopted 1 pre-existing object(s)")
			},
		},
		{
			name:       "success but the browser can't open: an installed-but dialog explains",
			result:     installed,
			chatErr:    errors.New("no browser"),
			wantEvents: []string{"window-open:Demo", "window-close", "chat", "dialog"},
			check: func(t *testing.T, rec *installUIRecorder) {
				require.Len(t, rec.dialogs, 1)
				assert.Contains(t, rec.dialogs[0].message, "was installed")
				assert.Contains(t, rec.dialogs[0].message, "no browser")
			},
		},
		{
			name:       "failure: window closed, then a stop dialog carrying the flattened error",
			err:        errors.New("apply AgentClass:\nforbidden"),
			wantEvents: []string{"window-open:Demo", "window-close", "dialog"},
			check: func(t *testing.T, rec *installUIRecorder) {
				require.Len(t, rec.dialogs, 1)
				assert.Equal(t, recordedDialog{title: "Couldn't install Demo", message: "apply AgentClass: forbidden", icon: "stop", button: "OK"}, rec.dialogs[0])
				assert.Empty(t, rec.chats)
			},
		},
		{
			name:       "needs config: caution dialog, then the dashboard opens",
			err:        fmt.Errorf("wrapped: %w", errNeedsConfig),
			wantEvents: []string{"window-open:Demo", "window-close", "dialog", "dashboard"},
			check: func(t *testing.T, rec *installUIRecorder) {
				require.Len(t, rec.dialogs, 1)
				assert.Equal(t, "caution", rec.dialogs[0].icon)
				assert.Equal(t, "Open dashboard", rec.dialogs[0].button)
				assert.Equal(t, "Demo needs configuration before it can run. OAP Desktop will open the dashboard so you can finish installing it.", rec.dialogs[0].message)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := stubInstallUI(t)
			rec.chatErr = tc.chatErr
			s := stateForTest(t)
			s.runInstallWithProgress("Demo", "demo.oap", func() (*install.Result, error) { return tc.result, tc.err })
			assert.Equal(t, tc.wantEvents, rec.events)
			tc.check(t, rec)
		})
	}
}

func TestRunInstallWithProgress_DialogFailureIsLogged(t *testing.T) {
	rec := stubInstallUI(t)
	rec.dialogErr = errors.New("osascript: no display")
	s := stateForTest(t)
	s.runInstallWithProgress("Demo", "demo.oap", func() (*install.Result, error) { return nil, errors.New("boom") })
	assert.Contains(t, s.out.(*bytes.Buffer).String(), "osascript: no display", "a dialog that fails to show must be logged")
}

func TestRunInstallWithProgress_PanicStillClosesWindow(t *testing.T) {
	rec := stubInstallUI(t)
	s := stateForTest(t)
	require.Panics(t, func() {
		s.runInstallWithProgress("Demo", "demo.oap", func() (*install.Result, error) { panic("boom") })
	})
	assert.Equal(t, []string{"window-open:Demo", "window-close"}, rec.events)
}

func TestInstallApproved_NoClusterShowsDialogWithoutWindow(t *testing.T) {
	rec := stubInstallUI(t)
	s := stateForTest(t) // no s.dg: the cluster isn't running
	s.installApproved([]byte("unused"), "demo.oap", "Demo", bundleSource{})
	assert.Equal(t, []string{"dialog"}, rec.events, "no progress window flashes up for an install that cannot start")
	require.Len(t, rec.dialogs, 1)
	assert.Equal(t, "Couldn't install Demo", rec.dialogs[0].title)
	assert.Contains(t, rec.dialogs[0].message, "isn't running yet")
}

func TestNotifyFailed_ReachesTheDesktopLogHook(t *testing.T) {
	var got []string
	orig := onNotifyFailed
	onNotifyFailed = func(title string, err error) { got = append(got, title+": "+err.Error()) }
	t.Cleanup(func() { onNotifyFailed = orig })

	notifyFailed("OAP Desktop", errors.New("osascript: no display"))
	assert.Equal(t, []string{"OAP Desktop: osascript: no display"}, got)
}
