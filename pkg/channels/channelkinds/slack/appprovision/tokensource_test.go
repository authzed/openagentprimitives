package appprovision

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasteSource_ReturnsWhatWasPasted(t *testing.T) {
	s, ok := SourceFor(KeyPaste)
	require.True(t, ok, "the paste source must be registered")

	assert.True(t, s.Available(), "pasting is always possible")
	assert.True(t, s.NeedsPastedToken(), "the paste source is the one that needs a token collected")
	assert.Empty(t, s.UnattendedReason(),
		"a token that arrives as an answer needs nobody at the terminal: this is the source --non-interactive uses")

	got, err := s.Token(context.Background(), "  xoxe.xoxp-demo  ")
	require.NoError(t, err, "Token")
	assert.Equal(t, "xoxe.xoxp-demo", got, "surrounding whitespace is trimmed")
}

func TestPasteSource_EmptyTokenIsRefused(t *testing.T) {
	s, ok := SourceFor(KeyPaste)
	require.True(t, ok)

	_, err := s.Token(context.Background(), "   ")
	require.Error(t, err, "an empty paste must not be read as a token")
}

// writeStubSlack installs a fake `slack` binary that prints tok on stdout, and
// points the source at it. Mirrors how pkg/platform/cloud tests stand in for gcloud.
func writeStubSlack(t *testing.T, tok string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "slack-stub")
	script := "#!/bin/sh\necho " + tok + "\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755), "write the stub")

	prev := slackBin
	slackBin = path
	t.Cleanup(func() { slackBin = prev })
}

func TestSlackCLISource_ReadsTheTokenFromTheBinary(t *testing.T) {
	writeStubSlack(t, "xoxe.xoxp-from-cli")

	s, ok := SourceFor(KeySlackCLI)
	require.True(t, ok, "the slack-cli source must be registered")

	assert.False(t, s.NeedsPastedToken(), "the CLI source collects nothing from the user")
	got, err := s.Token(context.Background(), "")
	require.NoError(t, err, "Token")
	assert.Equal(t, "xoxe.xoxp-from-cli", got, "trailing newline is trimmed")
}

// TestSlackCLISource_CannotServeAnUnattendedRun pins the rule: this source
// mints its token through Slack's ticket flow, so a run with nobody at the
// terminal can never complete it. "Needs no pasted token" is a DIFFERENT fact,
// and reading one as the other refuses a --non-interactive run for the wrong
// reason.
func TestSlackCLISource_CannotServeAnUnattendedRun(t *testing.T) {
	// Available regardless: the rule is about who is watching, not about
	// whether the binary is installed.
	writeStubSlack(t, "xoxe.xoxp-from-cli")

	s, ok := SourceFor(KeySlackCLI)
	require.True(t, ok)

	why := s.UnattendedReason()
	require.NotEmpty(t, why, "the CLI source must declare that it needs a person")
	assert.Contains(t, why, "challenge code", "and say what that person is there to do")
}

func TestSlackCLISource_UnavailableWhenTheBinaryIsMissing(t *testing.T) {
	prev := slackBin
	slackBin = filepath.Join(t.TempDir(), "definitely-not-installed")
	t.Cleanup(func() { slackBin = prev })

	s, ok := SourceFor(KeySlackCLI)
	require.True(t, ok)
	assert.False(t, s.Available(), "a source whose binary is absent must not be offered")
}

func TestSources_AreSortedAndComplete(t *testing.T) {
	var keys []string
	for _, s := range Sources() {
		keys = append(keys, s.Key())
		assert.NotEmpty(t, s.Label(), "every source needs a label for the select")
	}
	assert.Equal(t, []string{KeyPaste, KeySlackCLI}, keys, "sorted by key, both registered")
}
