package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
)

// installConfirm swaps tools.RunShellConfirm for the duration of the test
// and restores it on cleanup.
func installConfirm(t *testing.T, fn func(string, io.Reader, io.Writer) (string, error)) {
	t.Helper()
	old := tools.RunShellConfirm
	tools.RunShellConfirm = fn
	t.Cleanup(func() { tools.RunShellConfirm = old })
}

func TestRunShellRun(t *testing.T) {
	cases := []struct {
		name      string
		allowlist []*regexp.Regexp
		confirm   string // ShellAllow / ShellDeny — applied via installConfirm
		cmd       string
		check     func(t *testing.T, out string, err error)
	}{
		{
			name:      "command not in allowlist: returns allowlist error",
			allowlist: []*regexp.Regexp{regexp.MustCompile(`^echo `)},
			confirm:   tools.ShellAllow,
			cmd:       "rm -rf /",
			check: func(t *testing.T, _ string, err error) {
				require.Error(t, err, "expected allowlist denial")
				assert.Contains(t, err.Error(), "allowlist", "error should mention allowlist")
			},
		},
		{
			name:      "user denies confirmation: returns denied error",
			allowlist: []*regexp.Regexp{regexp.MustCompile(`^echo hi$`)},
			confirm:   tools.ShellDeny,
			cmd:       "echo hi",
			check: func(t *testing.T, _ string, err error) {
				require.Error(t, err, "expected user-deny error")
				assert.Contains(t, err.Error(), "denied", "error should mention denied")
			},
		},
		{
			name:      "allowlist match + user allow: returns stdout and exit 0",
			allowlist: []*regexp.Regexp{regexp.MustCompile(`^echo .*`)},
			confirm:   tools.ShellAllow,
			cmd:       "echo hi",
			check: func(t *testing.T, out string, err error) {
				require.NoError(t, err, "RunShellRun")
				// "echo hi" outputs "hi\n"; after trimming the JSON field is "hi\n"
				assert.Contains(t, out, `"stdout":"hi`, "stdout missing in result")
				assert.Contains(t, out, `"exit":0`, "exit code missing/non-zero in result")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &tools.RunShellState{Allowlist: tc.allowlist}
			decision := tc.confirm
			installConfirm(t, func(string, io.Reader, io.Writer) (string, error) {
				return decision, nil
			})

			out, err := tools.RunShellRun(context.Background(),
				json.RawMessage(`{"cmd":"`+tc.cmd+`"}`), state, strings.NewReader(""), io.Discard)
			tc.check(t, out, err)
		})
	}
}

func TestRunShellAlwaysCachesPerSession(t *testing.T) {
	state := &tools.RunShellState{
		Allowlist: []*regexp.Regexp{regexp.MustCompile(`^echo a$`)},
	}
	calls := 0
	installConfirm(t, func(string, io.Reader, io.Writer) (string, error) {
		calls++
		return tools.ShellAlways, nil
	})

	for i := 0; i < 3; i++ {
		_, err := tools.RunShellRun(context.Background(),
			json.RawMessage(`{"cmd":"echo a"}`), state, strings.NewReader(""), io.Discard)
		require.NoErrorf(t, err, "run %d", i)
	}
	assert.Equal(t, 1, calls, "RunShellConfirm should be cached after first ShellAlways")
}
