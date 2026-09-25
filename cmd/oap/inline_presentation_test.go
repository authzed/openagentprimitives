package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/agentcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/channelcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/identitycmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// What these tests can and cannot prove.
//
// CAN: which of `oap`'s commands DECLARE their questions inline, and that the
// several runs one command makes declare the same thing. That is where the
// decision lives — tui.Options.Inline carries the rule and DriverFor is the one
// place it is acted on.
//
// CANNOT: what a terminal draws. There is no pseudo-terminal in this suite, so
// nothing here observes an alternate screen being taken or not taken. pkg/cli/tui's
// own tests carry the wiring as far as bubbletea's program options and stop
// there for the same reason.

// TestWhichCommandsAskInline states the rule over every `oap` surface that makes
// the choice, so a command's answer sits next to its neighbours' rather than
// being re-derived one file at a time.
//
// The rule (tui.Options.Inline): a run is inline when its answers depend on the
// terminal around it — output the run does not itself render, whether streamed
// between its questions or printed before them. A run whose questions can be
// answered from what the form itself shows keeps the alternate screen.
func TestWhichCommandsAskInline(t *testing.T) {
	th := tui.NewTheme(tui.Caps{})
	var out bytes.Buffer
	script := strings.NewReader("")

	cases := []struct {
		name string
		// got is what the command declares; want is what the rule says, and
		// the subtest name carries the fact about the run that decides it.
		got  bool
		want bool
	}{
		{
			name: "oap agent install asks from the middle of its own output (clone notice, image progress, the conflict list): inline",
			got:  agentcmd.InstallQuestionDriverParams(th, script, &out).Inline,
			want: true,
		},
		{
			name: "oap idp setup with a redirect URI too long for the note, so the whole one is printed above the form: inline",
			got: identitycmd.IdpSetupOptions(th, script, &out, "oidc",
				"https://"+strings.Repeat("a", 80)+".example.test/oidc/callback/idp").Inline,
			want: true,
		},
		{
			name: "oap idp setup with a redirect URI the note carries whole, so nothing is printed above the form: not inline",
			got:  identitycmd.IdpSetupOptions(th, script, &out, "oidc", "https://ap.example.test/cb").Inline,
			want: false,
		},
		{
			name: "oap settings wizard renders a summary between two runs, but no answer depends on it: not inline",
			got:  settingscmd.NewPresentation(th, script, &out).RunOptions().Inline,
			want: false,
		},
		{
			name: "oap channel create composes a kind's guidance into its screens rather than the stream: not inline",
			got:  channelcmd.CreateRunOptions(th, script, &out, "slack", false).Inline,
			want: false,
		},
		{
			name: "oap identity setup composes a flow's browser address into its own note, not the stream: not inline",
			got:  identitycmd.SetupRunOptions(th, script, &out, "gh-pat", false).Inline,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}
