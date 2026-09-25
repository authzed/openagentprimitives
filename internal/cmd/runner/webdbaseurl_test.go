package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// TestWebdBaseURLFlag is the runner's half of a contract whose other half is in
// a different binary: pkg/controllers/agentsession's podspec stamps this env
// var onto the pod, and this is where the value is read back.
//
// Nothing fails when the two disagree. The runner simply never learns webd's
// address, conclude_trigger_status quietly stops attaching a details link, and
// the only visible symptom is a check run with no link on it — weeks later, on
// someone else's pull request. The shared externalurl.EnvWebdBaseURL constant
// is what prevents that, and this test is what proves the flag is actually
// bound to it.
func TestWebdBaseURLFlag(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{
			name: "stamped by the operator: read back verbatim",
			env:  "https://ap.example",
			want: "https://ap.example",
		},
		{
			// The operator omits the variable entirely until webd has an
			// external address, so this is what an ordinary pre-ingress
			// install looks like — not a misconfiguration.
			name: "unset: empty, and the runner still starts",
			env:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(externalurl.EnvWebdBaseURL, tc.env)
			cmd := newCommand()
			require.NoError(t, cmd.ParseFlags(nil), "ParseFlags")
			require.NoError(t, cmd.PreRunE(cmd, nil), "PreRunE")

			got, err := cmd.Flags().GetString("webd-base-url")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
