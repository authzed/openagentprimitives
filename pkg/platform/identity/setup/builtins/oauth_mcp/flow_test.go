package oauth_mcp_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/oauth_mcp"
)

func TestName(t *testing.T) {
	assert.Equal(t, "oauth-mcp", oauth_mcp.New().Name(), "Name must be 'oauth-mcp'")
}

// This package used to carry a SetOpenBrowser seam of its own, and a test that
// its override could be installed and cleared. Both are gone: the flow calls
// browser.Open directly, which suppresses itself inside a test binary, and the
// install/restore property is asserted once in pkg/x/browser rather than once
// per flow that used to own a copy of the seam. What remains here — driving the
// OAuth round trip through a recorder — is in screens_test.go.
