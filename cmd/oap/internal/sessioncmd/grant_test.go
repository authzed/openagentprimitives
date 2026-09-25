package sessioncmd

import (
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func TestSessionCmds_RequireArgs(t *testing.T) {
	cases := []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{
			name: "grant: too-few args (1 of 2) errors",
			cmd:  func() *cobra.Command { return newSessionGrantCmd(&apcmd.Globals{}) },
			args: []string{"only-one"},
		},
		{
			name: "grant: zero args errors",
			cmd:  func() *cobra.Command { return newSessionGrantCmd(&apcmd.Globals{}) },
			args: []string{},
		},
		{
			name: "revoke: too-few args (1 of 2) errors",
			cmd:  func() *cobra.Command { return newSessionRevokeCmd(&apcmd.Globals{}) },
			args: []string{"only-one"},
		},
		{
			name: "participants: missing session arg errors",
			cmd:  func() *cobra.Command { return newSessionParticipantsCmd(&apcmd.Globals{}) },
			args: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cmd()
			c.SetArgs(tc.args)
			assert.Error(t, c.Execute(), "expected error")
		})
	}
}

// A subject SET is now valid here. The blocklist accepts every subject type
// owner and participant do, because a grant that can be made to a group has to
// be revocable from that same group — denying a channel's members one at a time
// never finishes for a channel that keeps growing.
//
// The assertion is that validation lets it THROUGH: the command gets as far as
// needing a SpiceDB connection, which is the next step after the subject parses.
func TestSessionUnDenyCmd_acceptsASubjectSet(t *testing.T) {
	cmd := newSessionUnDenyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"sess-1", "group:engineering#member"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	require.Error(t, err, "no SpiceDB is wired in this test")
	// The subject-validation message is the only place this hint appears, so its
	// absence proves the subject set parsed and the command moved on.
	assert.NotContains(t, err.Error(), "group:eng#member",
		"the failure must be the missing connection, not subject validation")
}

// A subject with no type prefix at all is still refused — "alice" names nothing
// SpiceDB can resolve, and passing it through would produce a confusing
// server-side error instead of an actionable one.
func TestSessionUnDenyCmd_rejectsAnUntypedSubject(t *testing.T) {
	cmd := newSessionUnDenyCmd(&apcmd.Globals{})
	cmd.SetArgs([]string{"sess-1", "alice"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group:eng#member",
		"the message must show the subject-set form, not only the user form")
}
