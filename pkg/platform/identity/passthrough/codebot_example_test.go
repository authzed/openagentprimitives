package passthrough

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	embeddedtoolkits "github.com/authzed/openagentprimitives/toolkits"
)

func TestEmbeddedToolkitsDeclareCredentialTitles(t *testing.T) {
	want := map[string]map[string]string{ // toolkit → envVar → title
		"gh":           {"GITHUB_TOKEN": "GitHub"},
		"claude-oauth": {"CLAUDE_CODE_OAUTH_TOKEN": "Claude"},
	}
	for _, tk := range embeddedtoolkits.All() {
		envs, ok := want[tk.Name]
		if !ok {
			continue
		}
		for _, e := range tk.Env.Allowed {
			if title, ok := envs[e.Name]; ok {
				assert.Equal(t, title, e.Title, "%s/%s title", tk.Name, e.Name)
				assert.NotEmpty(t, e.Description, "%s/%s description", tk.Name, e.Name)
			}
		}
	}
	require.NotEmpty(t, embeddedtoolkits.All())
}
