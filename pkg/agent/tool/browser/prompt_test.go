package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestPromptSectionNilWithoutIdentity(t *testing.T) {
	rt := buildToolboxWithCatalog("", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "blocklist")
	assert.Nil(t, PromptSection(rt), "a toolbox with no bound identity offers no query_logins, so it must contribute no fragment")
}

func TestPromptSectionReflectsCatalogSize(t *testing.T) {
	cases := []struct {
		name    string
		catalog []spiceboxv1alpha1.BrowserLoginRef
		want    string
	}{
		{name: "zero logins", catalog: nil, want: "0 stored logins"},
		{name: "one login (singular)", catalog: []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, want: "1 stored login;"},
		{name: "two logins (plural)", catalog: []spiceboxv1alpha1.BrowserLoginRef{aaLogin(), unitedLogin()}, want: "2 stored logins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := buildToolboxWithCatalog("my-identity", tc.catalog, "blocklist")
			sec := PromptSection(rt)
			require.NotNil(t, sec)
			assert.Contains(t, sec.Body, tc.want)
			assert.Contains(t, sec.Body, "query_logins finds them by host or label")
		})
	}
}

func TestPromptSectionAllowlistCountsAllowedHosts(t *testing.T) {
	rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "allowlist",
		spiceboxv1alpha1.BrowserEffectiveSite{Host: "aa.com", Source: "explicit"},
		spiceboxv1alpha1.BrowserEffectiveSite{Host: "united.com", Source: "explicit"},
	)
	sec := PromptSection(rt)
	require.NotNil(t, sec)
	assert.Contains(t, sec.Body, "In allowlist mode, 2 hosts are allowed")
	assert.Contains(t, sec.Body, "query_logins also answers whether a given host is one of them")
}

func TestPromptSectionAllowlistSingularHost(t *testing.T) {
	rt := buildToolboxWithCatalog("my-identity", nil, "allowlist",
		spiceboxv1alpha1.BrowserEffectiveSite{Host: "aa.com", Source: "explicit"},
	)
	sec := PromptSection(rt)
	require.NotNil(t, sec)
	assert.Contains(t, sec.Body, "In allowlist mode, 1 host is allowed")
}

func TestPromptSectionBlocklistOmitsHostCount(t *testing.T) {
	rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "blocklist",
		spiceboxv1alpha1.BrowserEffectiveSite{Host: "blocked.test", Source: "explicit"},
	)
	sec := PromptSection(rt)
	require.NotNil(t, sec)
	assert.NotContains(t, sec.Body, "allowlist")
	assert.NotContains(t, sec.Body, "allowed")
}

// TestPromptSectionIsOneLine pins the spec's "one line" requirement: no
// embedded newline in the body.
func TestPromptSectionIsOneLine(t *testing.T) {
	rt := buildToolboxWithCatalog("my-identity", []spiceboxv1alpha1.BrowserLoginRef{aaLogin()}, "allowlist",
		spiceboxv1alpha1.BrowserEffectiveSite{Host: "aa.com", Source: "explicit"},
	)
	sec := PromptSection(rt)
	require.NotNil(t, sec)
	assert.NotContains(t, sec.Body, "\n")
}
