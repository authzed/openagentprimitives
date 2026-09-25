package preferencescmd

import (
	"bytes"
	"errors"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

func jsonVal(t *testing.T, raw string) *apiextv1.JSON {
	t.Helper()
	return &apiextv1.JSON{Raw: []byte(raw)}
}

// TestRenderSnapshot pins the table shape `oap preferences inspect` prints:
// one row per resolved key, the locked source spelled out plainly rather than
// carried in a separate column, and violations trailing the table as their
// own lines.
func TestRenderSnapshot(t *testing.T) {
	th := aptest.PlainTheme()

	resp := preferences.SnapshotResponse{
		Subject:        "alice",
		ClassNamespace: "demo",
		ClassName:      "helper",
		Snapshot: preferences.Snapshot{
			Keys: []preferences.Resolved{
				{Name: "theme", Type: "string", Value: jsonVal(t, `"dark"`), Source: preferences.SourceDefault},
				{Name: "layout", Type: "string", Value: jsonVal(t, `"tabs"`), Source: preferences.SourceUser},
				{
					Name:   "retention_days",
					Type:   "integer",
					Value:  jsonVal(t, `30`),
					Source: preferences.SourceLocked,
					Locked: true,
					Note:   "your saved value is overridden by admin policy",
				},
			},
			Violations: []string{"classUserPreferences.retention_days: value out of range"},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, renderSnapshot(&buf, th, resp))
	got := buf.String()

	t.Run("defaulted key shows its value and source", func(t *testing.T) {
		assert.Contains(t, got, "theme")
		assert.Contains(t, got, `"dark"`)
		assert.Contains(t, got, "default")
	})

	t.Run("user-set key shows the user source", func(t *testing.T) {
		assert.Contains(t, got, "layout")
		assert.Contains(t, got, `"tabs"`)
		assert.Contains(t, got, "user")
	})

	t.Run("locked key shows locked in SOURCE and its note", func(t *testing.T) {
		assert.Contains(t, got, "retention_days")
		assert.Contains(t, got, "30")
		assert.Contains(t, got, "locked")
		assert.Contains(t, got, "overridden by admin policy")
	})

	t.Run("violation trails the table as its own line", func(t *testing.T) {
		assert.Contains(t, got, "classUserPreferences.retention_days: value out of range")
	})

	t.Run("table header names the four columns", func(t *testing.T) {
		assert.Contains(t, got, "KEY")
		assert.Contains(t, got, "VALUE")
		assert.Contains(t, got, "SOURCE")
		assert.Contains(t, got, "NOTE")
	})
}

// TestRenderSnapshotUnsetKey pins the unset-value rendering: a key with no
// default, no global, and no saved user value must print something explicit
// rather than an empty cell that reads as a rendering bug.
func TestRenderSnapshotUnsetKey(t *testing.T) {
	th := aptest.PlainTheme()
	resp := preferences.SnapshotResponse{
		Snapshot: preferences.Snapshot{
			Keys: []preferences.Resolved{
				{Name: "nickname", Type: "string", Source: preferences.SourceUnset},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, renderSnapshot(&buf, th, resp))
	got := buf.String()
	assert.Contains(t, got, "nickname")
	assert.Contains(t, got, "unset")
	assert.Contains(t, got, "(unset)", "an unset value must render as an explicit placeholder, not a blank cell")
}

// TestIsNoPreferencesError pins the detection of the specific 404 httpsrv
// answers when a session's class declares no preferences at all — the one
// case runInspect renders as a plain message instead of an error dump.
func TestIsNoPreferencesError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the exact class-declares-no-preferences 404 body",
			err:  errors.New("httpclient: status=404 body=class declares no preferences\n"),
			want: true,
		},
		{
			name: "a different 404 (e.g. a mistyped session name) is not this case",
			err:  errors.New("httpclient: status=404 body=session lookup failed\n"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "an unrelated error",
			err:  errors.New("connection refused"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNoPreferencesError(tc.err))
		})
	}
}
