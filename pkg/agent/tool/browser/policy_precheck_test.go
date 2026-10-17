package browser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// buildToolbox returns a ResolvedBrowserToolbox with the given access mode and
// effective sites, named "demo" (the LLM-facing prefix the error text embeds).
func buildToolbox(mode string, sites ...spiceboxv1alpha1.BrowserEffectiveSite) spiceboxv1alpha1.ResolvedBrowserToolbox {
	return spiceboxv1alpha1.ResolvedBrowserToolbox{
		Name: "demo",
		Ref:  "demo-browser",
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access:      spiceboxv1alpha1.BrowserAccess{Mode: mode},
			Interaction: "readonly",
			State:       "ephemeral",
		},
		EffectiveSites: sites,
	}
}

// toolBySuffix returns the synthesized tool whose LLM name ends in "_<suffix>".
func toolBySuffix(t *testing.T, tools []agenttool.Tool, suffix string) agenttool.Tool {
	t.Helper()
	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "_"+suffix) {
			return tl
		}
	}
	t.Fatalf("no tool with suffix %q in %v", suffix, toolNames(tools))
	return nil
}

func toolNames(tools []agenttool.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}

// execNavigate synthesizes a toolbox and runs its navigate tool with the given
// URL, returning the Result. gateURL is unreachable on purpose: every case here
// must be refused BEFORE any Chrome connection is attempted.
func execNavigate(t *testing.T, rt spiceboxv1alpha1.ResolvedBrowserToolbox, url string) agenttool.Result {
	t.Helper()
	tools, err := Synthesize(rt, "http://127.0.0.1:1", "tok", false)
	require.NoError(t, err)
	nav := toolBySuffix(t, tools, "navigate")
	args, err := json.Marshal(map[string]any{"url": url})
	require.NoError(t, err)
	res, err := nav.Execute(context.Background(), args, &agenttool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error; refusals are tool errors")
	return res
}

func TestNavigateRefusesSchemes(t *testing.T) {
	rt := buildToolbox("blocklist") // blocklist with no sites admits every host by name
	cases := []string{
		"file:///etc/passwd",
		"chrome://settings",
		"about:blank",
		"data:text/html,x",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			res := execNavigate(t, rt, url)
			assert.True(t, res.IsError, "non-http(s) scheme must be a tool error")
			assert.Equal(t, "only http and https URLs can be opened", res.Content)
		})
	}
}

func TestNavigatePolicyPrecheck(t *testing.T) {
	t.Run("allowlist denies unlisted host: rule 'not in allowlist'", func(t *testing.T) {
		rt := buildToolbox("allowlist", spiceboxv1alpha1.BrowserEffectiveSite{Host: "good.test", Source: "explicit"})
		res := execNavigate(t, rt, "https://evil.com/x")
		assert.True(t, res.IsError)
		assert.Contains(t, res.Content, "(rule: not in allowlist)")
		assert.Contains(t, res.Content, "host evil.com is not allowed by browser toolbox demo")
	})
	t.Run("blocklist denies listed host: rule 'explicit denylist'", func(t *testing.T) {
		rt := buildToolbox("blocklist", spiceboxv1alpha1.BrowserEffectiveSite{Host: "blocked.test", Source: "explicit"})
		res := execNavigate(t, rt, "https://blocked.test/path")
		assert.True(t, res.IsError)
		assert.Contains(t, res.Content, "(rule: explicit denylist)")
		assert.Contains(t, res.Content, "host blocked.test is not allowed by browser toolbox demo")
	})
}
