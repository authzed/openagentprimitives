package progress

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_NonTTY_ReturnsStreaming(t *testing.T) {
	r := New(&bytes.Buffer{}, strings.NewReader(""), false)
	_, ok := r.(*streaming)
	require.True(t, ok, "non-terminal out must select the streaming renderer")
}

func TestStreaming_PhaseDetailDone_WritesLines(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	p := r.Phase("postgres")
	p.Detail("applied Deployment spicebox-postgres")
	p.Done()
	require.NoError(t, r.Close())

	out := buf.String()
	assert.Contains(t, out, "==> postgres")
	assert.Contains(t, out, "applied Deployment spicebox-postgres")
	assert.Contains(t, out, "postgres ready")
}

func TestStreaming_Fail_WritesError(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	r.Phase("postgres").Fail()
	assert.Contains(t, buf.String(), "postgres")
	assert.Contains(t, strings.ToLower(buf.String()), "fail")
}

// TestStreaming_Progress_PrintsCountsThrottled asserts the non-TTY bar prints
// the n/m count, completes, and — crucially — throttles to at most one line per
// 10% decile even when advanced one unit at a time over a large total.
func TestStreaming_Progress_PrintsCountsThrottled(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	p := r.Phase("push")
	const total = 100
	for i := 0; i <= total; i++ {
		p.Progress(i, total, "")
	}
	require.NoError(t, r.Close())

	out := buf.String()
	assert.Contains(t, out, "push: 100/100", "completion line printed")
	lines := strings.Count(out, "push: ")
	assert.LessOrEqual(t, lines, 11, "throttled to ≤ one line per decile (0..10); got %d", lines)
	assert.GreaterOrEqual(t, lines, 2, "at least the first bucket and completion are printed; got %d", lines)
}

// TestStreaming_Progress_IncludesDetail asserts the optional detail is rendered
// in the throttled line.
func TestStreaming_Progress_IncludesDetail(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	r.Phase("push").Progress(5, 10, "5 KB")
	require.NoError(t, r.Close())
	assert.Contains(t, buf.String(), "push: 5/10 (5 KB)")
}

func TestStreaming_Skip_WritesContinuingWithout(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	r.Phase("graphiti").Skip("optional; degraded state")
	out := buf.String()
	assert.Contains(t, out, "continuing without graphiti")
	assert.Contains(t, out, "degraded")
}

// TestStreaming_WritesNoEscapeSequencesAtAll is the non-TTY half of the
// checklist's theming, and the reason New's renderer choice matters: off a
// terminal, an install's output is a CI log or a redirected file, where a
// cursor-up would corrupt the record and a color code would be noise. Unlike
// the checklist — whose cursor codes ARE its mechanism — the streaming renderer
// may emit no escape sequence of any kind.
//
// It exercises every write path the reporter has, because a single path that
// grew a color would be invisible to a test that only drove one of them.
func TestStreaming_WritesNoEscapeSequencesAtAll(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, strings.NewReader(""), false)
	require.IsType(t, &streaming{}, r, "precondition: a non-terminal writer must select the streaming renderer")

	p := r.Phase("postgres")
	p.Detail("applied Deployment")
	p.Progress(1, 2, "512 KB")
	p.Done()
	r.Phase("graphiti").Skip("optional; degraded state")
	r.Phase("spicedb").Fail()
	r.Info("informational")
	r.OK("all good")
	r.Warn("something noteworthy")
	require.NoError(t, r.Close())

	assert.NotContains(t, buf.String(), "\x1b[",
		"a non-terminal install log must carry no escape sequences at all:\n%q", buf.String())
}
