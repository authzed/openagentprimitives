package webui_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

func TestOriginAndAuthLevelStrings(t *testing.T) {
	assert.Equal(t, "trusted", webui.OriginTrusted.String())
	assert.Equal(t, "sandbox", webui.OriginSandbox.String())
	assert.Equal(t, "none", webui.AuthNone.String())
	assert.Equal(t, "authenticated", webui.AuthAuthenticated.String())
	assert.Equal(t, "authorized", webui.AuthAuthorized.String())
}
