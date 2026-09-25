package besteffort_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// captureInfo returns an InfoFunc that records each call as a
// formatted string. Compatible with both slog.Logger.Info and
// logr.Logger.Info shapes since besteffort.InfoFunc is just
// `func(string, ...any)`.
func captureInfo(records *[]string) besteffort.InfoFunc {
	return func(msg string, args ...any) {
		var b strings.Builder
		b.WriteString(msg)
		for i := 0; i+1 < len(args); i += 2 {
			b.WriteString(" ")
			fmtKV(&b, args[i], args[i+1])
		}
		*records = append(*records, b.String())
	}
}

func fmtKV(b *strings.Builder, k, v any) {
	b.WriteString(stringify(k))
	b.WriteString("=")
	b.WriteString(stringify(v))
}

func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

func TestLog_NilErrIsSilent(t *testing.T) {
	var records []string
	assert.False(t, besteffort.Log(captureInfo(&records), "patch placeholder", nil, "channel", "c1"),
		"Log should return false for nil err")
	assert.Empty(t, records, "Log should not log on nil err")
}

func TestLog_NonNilErrLogsAndReturnsTrue(t *testing.T) {
	var records []string
	require.True(t, besteffort.Log(captureInfo(&records), "patch placeholder", errors.New("api: 500 Internal"), "channel", "c1"),
		"Log should return true for non-nil err")
	require.Len(t, records, 1)
	got := records[0]
	for _, want := range []string{"patch placeholder", "api: 500 Internal", "c1"} {
		assert.Contains(t, got, want, "log record missing %q", want)
	}
}

func TestLog_PassesThroughExtraKVs(t *testing.T) {
	var records []string
	besteffort.Log(captureInfo(&records), "publish notification", errors.New("nats closed"),
		"session", "default/foo",
		"subject", "ap.session.default.foo.out.notification")
	require.Len(t, records, 1)
	got := records[0]
	for _, want := range []string{"default/foo", "ap.session.default.foo.out.notification"} {
		assert.Contains(t, got, want)
	}
}
