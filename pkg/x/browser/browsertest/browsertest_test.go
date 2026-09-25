package browsertest_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/x/browser"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

func TestRecord_CapturesURLsAndReportsSuccess(t *testing.T) {
	rec := browsertest.Record(t)

	require.NoError(t, browser.Open("https://example.invalid/one"))
	require.NoError(t, browser.Open("https://example.invalid/two"))

	assert.Equal(t, []string{"https://example.invalid/one", "https://example.invalid/two"}, rec.URLs())
	assert.Equal(t, "https://example.invalid/two", rec.Last())
	assert.Equal(t, 2, rec.Count(), "Count distinguishes one open from an open per retry")
}

func TestFail_RecordsTheURLItRefused(t *testing.T) {
	boom := errors.New("no browser on this machine")
	rec := browsertest.Fail(t, boom)

	err := browser.Open("https://example.invalid/headless")

	assert.ErrorIs(t, err, boom, "Fail reports the error the test asked for")
	assert.Equal(t, []string{"https://example.invalid/headless"}, rec.URLs(),
		"a refused open is still an attempt, and the URL a flow tried is what the test asserts on")
}

func TestUse_DrivesTheFlowAndStillRecords(t *testing.T) {
	driven := ""
	rec := browsertest.Use(t, func(u string) error { driven = u; return nil })

	require.NoError(t, browser.Open("https://example.invalid/consent"))

	assert.Equal(t, "https://example.invalid/consent", driven, "the driver sees the URL")
	assert.Equal(t, 1, rec.Count(), "Use records as well as drives, so a driver need not also count")
}

func TestCleanupRestores_SuppressionIsTheFloorAfterASubtest(t *testing.T) {
	t.Run("installs a recorder", func(t *testing.T) {
		browsertest.Record(t)
		require.NoError(t, browser.Open("https://example.invalid/inner"))
	})

	// The subtest's t.Cleanup has run. Nothing is installed, so the parent must
	// see suppression rather than the subtest's recorder — an opener that
	// outlived its test is how a later test silently stops testing anything.
	assert.ErrorIs(t, browser.Open("https://example.invalid/after"), browser.ErrSuppressed,
		"the recorder must not outlive the test that installed it")
}

func TestEmptyRecorder_LastIsEmptyRatherThanPanicking(t *testing.T) {
	rec := browsertest.Record(t)
	assert.Empty(t, rec.Last(), "Last on an unused recorder is empty, not a panic")
	assert.Zero(t, rec.Count())
	assert.Empty(t, rec.URLs())
}
