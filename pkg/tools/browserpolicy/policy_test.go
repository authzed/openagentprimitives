package browserpolicy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

func TestCanonicalHost(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "lowercases: AA.com -> aa.com", in: "AA.com", want: "aa.com"},
		{name: "strips one trailing dot: aa.com. -> aa.com", in: "aa.com.", want: "aa.com"},
		{name: "port rejected: aa.com:8443 errors", in: "aa.com:8443", wantErr: true},
		{name: "scheme rejected: https://aa.com errors", in: "https://aa.com", wantErr: true},
		{name: "path rejected: aa.com/x errors", in: "aa.com/x", wantErr: true},
		{name: "space rejected", in: "aa .com", wantErr: true},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "double trailing dot rejected: empty label", in: "aa.com..", wantErr: true},
		{name: "ip literal passes canonicalization", in: "8.8.8.8", want: "8.8.8.8"},
		{name: "ipv6 literal passes canonicalization: ::1", in: "::1", want: "::1"},
		{name: "double colon-garbage rejected: evil.com:80:81 errors", in: "evil.com:80:81", wantErr: true},
		{name: "colon-colon garbage rejected: evil.com::81 errors", in: "evil.com::81", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := browserpolicy.CanonicalHost(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name string
		site browserpolicy.Site
		host string
		want bool
	}{
		{name: "exact host matches itself", site: browserpolicy.Site{Host: "aa.com"}, host: "aa.com", want: true},
		{name: "suffix-by-label: www.aa.com under aa.com", site: browserpolicy.Site{Host: "aa.com"}, host: "www.aa.com", want: true},
		{name: "lookalike notaa.com does NOT match aa.com", site: browserpolicy.Site{Host: "aa.com"}, host: "notaa.com", want: false},
		{name: "exact pin: www.aa.com does NOT match exact aa.com", site: browserpolicy.Site{Host: "aa.com", Exact: true}, host: "www.aa.com", want: false},
		{name: "exact pin still matches itself", site: browserpolicy.Site{Host: "aa.com", Exact: true}, host: "aa.com", want: true},
		{name: "deep subdomain matches", site: browserpolicy.Site{Host: "aa.com"}, host: "a.b.aa.com", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, browserpolicy.Match(tc.site, tc.host))
		})
	}
}

func TestDecide(t *testing.T) {
	sites := []browserpolicy.Site{{Host: "aa.com"}, {Host: "united.com", Exact: true}}
	cases := []struct {
		name, mode, host string
		wantAllowed      bool
		wantRule         string
	}{
		{name: "allowlist admits listed suffix", mode: "allowlist", host: "www.aa.com", wantAllowed: true},
		{name: "allowlist denies unlisted: rule 'not in allowlist'", mode: "allowlist", host: "evil.com", wantAllowed: false, wantRule: "not in allowlist"},
		{name: "blocklist denies listed: rule 'explicit denylist'", mode: "blocklist", host: "aa.com", wantAllowed: false, wantRule: "explicit denylist"},
		{name: "blocklist admits everything else", mode: "blocklist", host: "example.org", wantAllowed: true},
		{name: "metadata IP denied even in blocklist mode", mode: "blocklist", host: "169.254.169.254", wantAllowed: false, wantRule: "private or metadata address"},
		{name: "rfc1918 denied even when allowlisted", mode: "allowlist", host: "10.0.0.5", wantAllowed: false, wantRule: "private or metadata address"},
		{name: "uncanonical input is canonicalized: AA.com. in allowlist", mode: "allowlist", host: "AA.com.", wantAllowed: true},
		{name: "malformed host is denied, not errored through", mode: "allowlist", host: "aa .com", wantAllowed: false, wantRule: "not in allowlist"},
		{name: "colon-garbage on a blocklisted host fails closed: aa.com:80:81 denied", mode: "blocklist", host: "aa.com:80:81", wantAllowed: false, wantRule: "explicit denylist"},
		{name: "unspecified address denied: 0.0.0.0 is a loopback-dial vector", mode: "blocklist", host: "0.0.0.0", wantAllowed: false, wantRule: "private or metadata address"},
		{name: "v4-mapped-v6 unspecified denied: ::ffff:0.0.0.0 must not slip past as a loopback-dial vector", mode: "blocklist", host: "::ffff:0.0.0.0", wantAllowed: false, wantRule: "private or metadata address"},
		{name: "cgnat denied even in blocklist mode", mode: "blocklist", host: "100.64.1.1", wantAllowed: false, wantRule: "private or metadata address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := browserpolicy.Decide(tc.mode, sites, tc.host)
			assert.Equal(t, tc.wantAllowed, got.Allowed)
			assert.Equal(t, tc.wantRule, got.Rule)
		})
	}
}

func TestIsPrivateOrMetadataHost(t *testing.T) {
	cases := []struct {
		name, host string
		want       bool
	}{
		{name: "rfc1918 10.x is private", host: "10.1.2.3", want: true},
		{name: "rfc1918 192.168.x is private", host: "192.168.0.1", want: true},
		{name: "rfc1918 172.16.x is private", host: "172.16.0.1", want: true},
		{name: "link-local metadata address", host: "169.254.169.254", want: true},
		{name: "loopback v4", host: "127.0.0.1", want: true},
		{name: "loopback v6", host: "::1", want: true},
		{name: "unique-local v6", host: "fd00::1", want: true},
		{name: "unspecified v4: 0.0.0.0 dials loopback on Linux", host: "0.0.0.0", want: true},
		{name: "unspecified v6: :: dials loopback on Linux", host: "::", want: true},
		{name: "public v4 is not private", host: "8.8.8.8", want: false},
		{name: "dns name is not private", host: "aa.com", want: false},
		{name: "public v6 is not private", host: "2606:4700::1111", want: false},
		{name: "cgnat shared-address space (RFC 6598) is private", host: "100.64.1.1", want: true},
		{name: "v4-mapped-v6 cgnat is private", host: "::ffff:100.64.1.1", want: true},
		// A 4-in-6 mapped unspecified address must be caught: IsUnspecified does
		// NOT unmap, so without the up-front Unmap() this slipped through and
		// dialed loopback (SSRF to the pod, incl. Chrome's DevTools port).
		{name: "v4-mapped-v6 unspecified is private (SSRF guard)", host: "::ffff:0.0.0.0", want: true},
		{name: "v4-mapped-v6 loopback is private", host: "::ffff:127.0.0.1", want: true},
		{name: "v4-mapped-v6 metadata address is private", host: "::ffff:169.254.169.254", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, browserpolicy.IsPrivateOrMetadataHost(tc.host))
		})
	}
}
