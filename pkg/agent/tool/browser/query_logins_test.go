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

// buildToolboxWithCatalog returns a ResolvedBrowserToolbox named "demo" with
// the given identity/catalog/mode/effective-sites, mirroring buildToolbox
// (policy_precheck_test.go) but adding the identity + catalog fields
// query_logins needs.
func buildToolboxWithCatalog(identity string, catalog []spiceboxv1alpha1.BrowserLoginRef, mode string, sites ...spiceboxv1alpha1.BrowserEffectiveSite) spiceboxv1alpha1.ResolvedBrowserToolbox {
	rt := buildToolbox(mode, sites...)
	rt.IdentityName = identity
	rt.Catalog = catalog
	return rt
}

// execQueryLogins synthesizes rt and runs its query_logins tool with the
// given args, returning the Result. No live Chrome connection is ever
// attempted: query_logins reads only rt.Catalog/EffectiveSites, so the gate
// URL is unreachable on purpose, same discipline as execNavigate.
func execQueryLogins(t *testing.T, rt spiceboxv1alpha1.ResolvedBrowserToolbox, args map[string]any) agenttool.Result {
	t.Helper()
	tools, err := Synthesize(rt, "http://127.0.0.1:1", "tok", false, nil)
	require.NoError(t, err)
	ql := toolBySuffix(t, tools, "query_logins")
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := ql.Execute(context.Background(), raw, &agenttool.SessionContext{})
	require.NoError(t, err, "Execute must not return a Go error; refusals are tool errors")
	return res
}

func aaLogin() spiceboxv1alpha1.BrowserLoginRef {
	return spiceboxv1alpha1.BrowserLoginRef{
		ID:    "item-1",
		Label: "AA Advantage",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "aa.com"}},
	}
}

func unitedLogin() spiceboxv1alpha1.BrowserLoginRef {
	return spiceboxv1alpha1.BrowserLoginRef{
		ID:    "item-2",
		Label: "United MileagePlus",
		Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "united.com"}, {Host: "mileageplus.united.com", Exact: true}},
	}
}

func TestQueryLoginsOfferedOnlyWithIdentity(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		want     bool
	}{
		{name: "no identity: not offered", identity: "", want: false},
		{name: "identity bound: offered", identity: "my-identity", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := buildToolboxWithCatalog(tc.identity, []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "blocklist")
			tools, err := Synthesize(rt, "http://127.0.0.1:1", "tok", false, nil)
			require.NoError(t, err)
			got := false
			for _, tl := range tools {
				if tl.Name() == "demo_query_logins" {
					got = true
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestQueryLoginsOfferedInBothModes proves the gate is identity-only,
// independent of Interaction (act vs readonly) — unlike the acting tools
// Phase C adds elsewhere, this observation tool is symmetric across modes.
func TestQueryLoginsOfferedInBothModes(t *testing.T) {
	for _, mode := range []string{"readonly", "act"} {
		t.Run(mode, func(t *testing.T) {
			rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "blocklist")
			rt.Spec.Interaction = mode
			tools, err := Synthesize(rt, "http://127.0.0.1:1", "tok", false, nil)
			require.NoError(t, err)
			found := false
			for _, tl := range tools {
				if tl.Name() == "demo_query_logins" {
					found = true
				}
			}
			assert.True(t, found, "query_logins must be offered in %s mode", mode)
		})
	}
}

func TestQueryLoginsFilterMatchesHostOrLabel(t *testing.T) {
	catalog := []spiceboxv1alpha1.BrowserLoginRef{aaLogin(), unitedLogin()}
	cases := []struct {
		name       string
		filter     string
		wantLabels []string
	}{
		{name: "empty filter returns all", filter: "", wantLabels: []string{"AA Advantage", "United MileagePlus"}},
		{name: "matches host substring, case-insensitive", filter: "AA.COM", wantLabels: []string{"AA Advantage"}},
		{name: "matches a secondary host", filter: "mileageplus", wantLabels: []string{"United MileagePlus"}},
		{name: "matches label substring, case-insensitive", filter: "advantage", wantLabels: []string{"AA Advantage"}},
		{name: "no match", filter: "nonexistent", wantLabels: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := buildToolboxWithCatalog("my-identity", catalog, "blocklist")
			res := execQueryLogins(t, rt, map[string]any{"filter": tc.filter})
			require.False(t, res.IsError, "result: %s", res.Content)

			var entries []queryLoginsEntry
			require.NoError(t, json.Unmarshal([]byte(firstJSONLine(t, res.Content)), &entries))
			var gotLabels []string
			for _, e := range entries {
				gotLabels = append(gotLabels, e.Label)
			}
			assert.Equal(t, tc.wantLabels, gotLabels)
		})
	}
}

// TestQueryLoginsEntryShapeIsHostsAndLabelOnly pins the output shape to
// exactly {hosts, label} — nothing else about the catalog item, in
// particular never its opaque id.
func TestQueryLoginsEntryShapeIsHostsAndLabelOnly(t *testing.T) {
	rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "blocklist")
	res := execQueryLogins(t, rt, map[string]any{})
	require.False(t, res.IsError)

	var raw []map[string]any
	require.NoError(t, json.Unmarshal([]byte(firstJSONLine(t, res.Content)), &raw))
	require.Len(t, raw, 1)
	keys := make(map[string]bool)
	for k := range raw[0] {
		keys[k] = true
	}
	assert.True(t, keys["hosts"])
	assert.True(t, keys["label"])
	assert.Len(t, raw[0], 2, "entry must carry nothing beyond hosts+label; got keys %v", raw[0])
	assert.NotContains(t, res.Content, "item-1", "the catalog's opaque id must never be exposed")
}

func TestQueryLoginsPagination(t *testing.T) {
	// Build enough entries to span 3 pages at loginsPerPage=20: 45 entries.
	var catalog []spiceboxv1alpha1.BrowserLoginRef
	for i := 0; i < 45; i++ {
		catalog = append(catalog, spiceboxv1alpha1.BrowserLoginRef{
			ID:    "item",
			Label: "site",
			Hosts: []spiceboxv1alpha1.BrowserLoginHost{{Host: "site.test"}},
		})
	}
	rt := buildToolboxWithCatalog("my-identity", catalog, "blocklist")

	res1 := execQueryLogins(t, rt, map[string]any{})
	require.False(t, res1.IsError)
	assert.Contains(t, res1.Content, "page 1 of 3")
	var page1 []queryLoginsEntry
	require.NoError(t, json.Unmarshal([]byte(firstJSONLine(t, res1.Content)), &page1))
	assert.Len(t, page1, 20)

	res2 := execQueryLogins(t, rt, map[string]any{"page": 2})
	require.False(t, res2.IsError)
	assert.Contains(t, res2.Content, "page 2 of 3")
	var page2 []queryLoginsEntry
	require.NoError(t, json.Unmarshal([]byte(firstJSONLine(t, res2.Content)), &page2))
	assert.Len(t, page2, 20)

	res3 := execQueryLogins(t, rt, map[string]any{"page": 3})
	require.False(t, res3.IsError)
	assert.Contains(t, res3.Content, "page 3 of 3")
	var page3 []queryLoginsEntry
	require.NoError(t, json.Unmarshal([]byte(firstJSONLine(t, res3.Content)), &page3))
	assert.Len(t, page3, 5)

	res4 := execQueryLogins(t, rt, map[string]any{"page": 4})
	assert.True(t, res4.IsError)
	assert.Contains(t, res4.Content, "out of range")
}

func TestQueryLoginsHostPolicyAnswer(t *testing.T) {
	t.Run("allowed host, allowlist mode", func(t *testing.T) {
		rt := buildToolboxWithCatalog("my-identity", nil, "allowlist",
			spiceboxv1alpha1.BrowserEffectiveSite{Host: "good.test", Source: "explicit"})
		res := execQueryLogins(t, rt, map[string]any{"host": "good.test"})
		require.False(t, res.IsError)
		assert.Contains(t, res.Content, "host good.test is allowed by this toolbox's policy")
	})
	t.Run("denied host, allowlist mode: rule 'not in allowlist'", func(t *testing.T) {
		rt := buildToolboxWithCatalog("my-identity", nil, "allowlist",
			spiceboxv1alpha1.BrowserEffectiveSite{Host: "good.test", Source: "explicit"})
		res := execQueryLogins(t, rt, map[string]any{"host": "evil.com"})
		require.False(t, res.IsError)
		assert.Contains(t, res.Content, "host evil.com is NOT allowed by this toolbox's policy (rule: not in allowlist)")
	})
	t.Run("denied host, blocklist mode: rule 'explicit denylist'", func(t *testing.T) {
		rt := buildToolboxWithCatalog("my-identity", nil, "blocklist",
			spiceboxv1alpha1.BrowserEffectiveSite{Host: "blocked.test", Source: "explicit"})
		res := execQueryLogins(t, rt, map[string]any{"host": "blocked.test"})
		require.False(t, res.IsError)
		assert.Contains(t, res.Content, "host blocked.test is NOT allowed by this toolbox's policy (rule: explicit denylist)")
	})
	t.Run("host check and filter compose in one call", func(t *testing.T) {
		rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "allowlist",
			spiceboxv1alpha1.BrowserEffectiveSite{Host: "aa.com", Source: "explicit"})
		res := execQueryLogins(t, rt, map[string]any{"host": "aa.com", "filter": "advantage"})
		require.False(t, res.IsError)
		assert.Contains(t, res.Content, "host aa.com is allowed by this toolbox's policy")
		assert.Contains(t, res.Content, "AA Advantage")
	})
}

// firstJSONLine extracts the JSON-array block from a queryLogins result body
// (the block before the trailing "page N of M" footer, and after an optional
// leading host-policy line) — blocks are separated by a blank line.
func firstJSONLine(t *testing.T, content string) string {
	t.Helper()
	parts := strings.Split(content, "\n\n")
	require.GreaterOrEqual(t, len(parts), 2, "expected at least a JSON block and a page footer in %q", content)
	return parts[len(parts)-2]
}
