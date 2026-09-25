package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

func TestOpenBrowserHappy(t *testing.T) {
	rec := browsertest.Record(t)

	out, err := tools.OpenBrowserRun(context.Background(),
		json.RawMessage(`{"url":"https://github.com/settings"}`))
	require.NoError(t, err, "OpenBrowserRun")
	assert.Equal(t, "https://github.com/settings", rec.Last(), "opener URL")
	assert.Contains(t, out, "opened", "output should report opened")
}

func TestOpenBrowserRejectsNonHTTP(t *testing.T) {
	rec := browsertest.Record(t)

	_, err := tools.OpenBrowserRun(context.Background(),
		json.RawMessage(`{"url":"file:///etc/passwd"}`))
	require.Error(t, err, "expected error on file:// scheme")
	assert.Zero(t, rec.Count(),
		"the scheme is refused BEFORE anything is handed to the opener — an error "+
			"returned after the open would still have read the file")
}
