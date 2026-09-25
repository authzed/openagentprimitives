package auditgen

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock returns a deterministic date so report paths are stable in tests.
func fixedClock() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) }

// recorder captures the argv of each claude invocation so tests can assert what
// the generator would have run without a real claude process.
type recorder struct {
	calls    int
	lastArgs []string
}

func (r *recorder) run(_ context.Context, args []string) error {
	r.calls++
	r.lastArgs = args
	return nil
}

// prompt returns the final argv element — AuditPrompt always puts the prompt last.
func (r *recorder) prompt() string {
	if len(r.lastArgs) == 0 {
		return ""
	}
	return r.lastArgs[len(r.lastArgs)-1]
}

func newGen(t *testing.T, rec *recorder) (*Generator, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	g := &Generator{
		Run: rec.run,
		Diff: func(base string) (string, error) {
			return "", errors.New("diff not configured for this test")
		},
		Now: fixedClock,
		Out: &out,
	}
	return g, &out
}

func TestPkg_InvokesWithSlugifiedReportPath(t *testing.T) {
	rec := &recorder{}
	g, _ := newGen(t, rec)

	require.NoError(t, g.Pkg(context.Background(), "pkg/web/webui/chat/"))
	assert.Equal(t, 1, rec.calls, "one claude invocation")
	assert.Contains(t, rec.prompt(), "docs/audits/2026-07-15-pkg-web-webui-chat-audit.md")
	assert.Contains(t, rec.lastArgs, "--model")
	assert.Contains(t, rec.lastArgs, DefaultModel, "defaults to opus when no model set")
}

func TestPkg_EmptyPathErrorsAndDoesNotRun(t *testing.T) {
	for _, path := range []string{"", "   ", "/"} {
		rec := &recorder{}
		g, _ := newGen(t, rec)
		err := g.Pkg(context.Background(), path)
		assert.Error(t, err, "path %q must be rejected", path)
		assert.Zero(t, rec.calls, "path %q must not invoke claude", path)
	}
}

func TestAll_UsesWholeRepoSlug(t *testing.T) {
	rec := &recorder{}
	g, _ := newGen(t, rec)

	require.NoError(t, g.All(context.Background()))
	assert.Equal(t, 1, rec.calls)
	assert.Contains(t, rec.prompt(), "docs/audits/2026-07-15-all-audit.md")
	assert.Contains(t, rec.prompt(), "WHOLE repository")
}

func TestRecent_EmptyDiffSkips(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	g := &Generator{
		Run:  rec.run,
		Diff: func(base string) (string, error) { return "   \n  ", nil },
		Now:  fixedClock,
		Out:  &out,
	}
	require.NoError(t, g.Recent(context.Background()))
	assert.Zero(t, rec.calls, "empty diff is a no-op, not an invocation")
	assert.Contains(t, out.String(), "skipping")
}

func TestRecent_NonEmptyDiffInvokesWithDiffAndBase(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	g := &Generator{
		Run:      rec.run,
		Diff:     func(base string) (string, error) { return "DIFF-BODY-99", nil },
		DiffBase: "release-1",
		Now:      fixedClock,
		Out:      &out,
	}
	require.NoError(t, g.Recent(context.Background()))
	require.Equal(t, 1, rec.calls)
	assert.Contains(t, rec.prompt(), "DIFF-BODY-99", "the diff body is embedded")
	assert.Contains(t, rec.prompt(), "release-1...HEAD", "honors AUDIT_DIFF_BASE")
	assert.Contains(t, rec.prompt(), "docs/audits/2026-07-15-recent-audit.md")
}

func TestRecent_DiffErrorSurfaces(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	g := &Generator{
		Run:  rec.run,
		Diff: func(base string) (string, error) { return "", errors.New("boom") },
		Now:  fixedClock,
		Out:  &out,
	}
	err := g.Recent(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom", "the git error is wrapped, not swallowed")
	assert.Zero(t, rec.calls)
}

func TestDryRun_PrintsPromptAndDoesNotRun(t *testing.T) {
	var out bytes.Buffer
	ranErr := errors.New("Run must not be called in DryRun")
	g := &Generator{
		Run:    func(context.Context, []string) error { return ranErr },
		Now:    fixedClock,
		DryRun: true,
		Out:    &out,
	}
	require.NoError(t, g.Pkg(context.Background(), "pkg/x"))
	s := out.String()
	assert.Contains(t, s, "[dry-run]")
	assert.Contains(t, s, "docs/audits/2026-07-15-pkg-x-audit.md")
	assert.Contains(t, s, Lenses[0].Title, "the prompt (with lenses) is printed")
}

func TestInvoke_NoRunFuncErrors(t *testing.T) {
	g := &Generator{Now: fixedClock} // no Run, not DryRun
	err := g.Pkg(context.Background(), "pkg/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Run func")
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pkg/web/webui/chat", "pkg-web-webui-chat"},
		{"pkg/web/webui/chat/", "pkg-web-webui-chat"},
		{"cmd/oap", "cmd-oap"},
		{"pkg/foo_bar", "pkg-foo-bar"},
		{"pkg//double", "pkg-double"},
		{"pkg/mixedCASE/9", "pkg-mixedCASE-9"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, slugify(tc.in), "slugify(%q)", tc.in)
	}
}
