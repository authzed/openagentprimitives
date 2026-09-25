package wait

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeadline(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		term   *TerminatedNote
		want   func(t *testing.T, got string)
	}{
		{
			name:   "CrashLoopBackoff with no terminated container: framed as backing off",
			reason: "CrashLoopBackOff",
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "backing off")
			},
		},
		{
			name:   "genuine config error: no backing-off framing",
			reason: "CreateContainerConfigError",
			want: func(t *testing.T, got string) {
				assert.Equal(t, "", got)
			},
		},
		{
			name:   "empty reason: no headline",
			reason: "",
			want: func(t *testing.T, got string) {
				assert.Equal(t, "", got)
			},
		},
		{
			// The incident shape: a fatal startup error must NOT be reframed as a
			// retryable wait, because retrying it never succeeds.
			name:   "CrashLoopBackOff with a fatal exit: leads with the container's own error, not 'will retry'",
			reason: "CrashLoopBackOff",
			term: &TerminatedNote{
				Container: "spicedb",
				ExitCode:  1,
				Reason:    "Error",
				Message:   "failed to create datastore: cannot apply bootstrap data: schema or tuples already exist in the datastore",
			},
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "cannot apply bootstrap data", "the fatal error itself must reach the operator")
				assert.Contains(t, got, "spicedb", "name the container that died")
				assert.Contains(t, got, "fatal", "frame it as fatal")
				assert.NotContains(t, got, "will retry", "a permanent misconfiguration is not a retryable wait")
			},
		},
		{
			name:   "no termination message: falls back to the first error log line",
			reason: "CrashLoopBackOff",
			term: &TerminatedNote{
				Container: "spicedb",
				ExitCode:  1,
				Reason:    "Error",
				Logs:      []string{`{"level":"error","message":"terminated with errors"}`},
			},
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "terminated with errors")
			},
		},
		{
			name:   "clean exit(0): not a fault worth leading with",
			reason: "CrashLoopBackOff",
			term:   &TerminatedNote{Container: "spicedb", ExitCode: 0, Message: "bye"},
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "backing off", "exit 0 falls through to the ordinary framing")
			},
		},
		{
			name:   "terminated but nothing to say: falls through rather than printing an empty error",
			reason: "CrashLoopBackOff",
			term:   &TerminatedNote{Container: "spicedb", ExitCode: 1, Reason: "Error"},
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "backing off")
			},
		},
		{
			name:   "long message is truncated so the headline stays one line",
			reason: "CrashLoopBackOff",
			term: &TerminatedNote{
				Container: "spicedb",
				ExitCode:  1,
				Reason:    "Error",
				Message:   strings.Repeat("x", maxDiagDetailChars+50),
			},
			want: func(t *testing.T, got string) {
				assert.Contains(t, got, "…")
				assert.NotContains(t, got, strings.Repeat("x", maxDiagDetailChars+1))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.want(t, headline(tc.reason, tc.term))
		})
	}
}

func TestLooksLikeError(t *testing.T) {
	for _, line := range []string{
		`{"level":"error","message":"terminated with errors"}`,
		`{"level":"fatal"}`,
		`ts=1 level=error msg=boom`,
		`LEVEL=FATAL`,
		`Error: could not connect`,
	} {
		assert.True(t, looksLikeError(line), "should match: %s", line)
	}
	for _, line := range []string{
		`{"level":"info","message":"configured logging"}`,
		`starting revision heartbeat`,
		``,
	} {
		assert.False(t, looksLikeError(line), "should not match: %s", line)
	}
}
