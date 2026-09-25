package safehttp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{name: "IPv4 loopback 127.0.0.1 is blocked", ip: "127.0.0.1", blocked: true},
		{name: "IPv6 loopback ::1 is blocked", ip: "::1", blocked: true},
		{name: "cloud-metadata link-local 169.254.169.254 is blocked", ip: "169.254.169.254", blocked: true},
		{name: "IPv6 link-local fe80::1 is blocked", ip: "fe80::1", blocked: true},
		{name: "RFC1918 10.0.0.1 is blocked", ip: "10.0.0.1", blocked: true},
		{name: "RFC1918 172.16.0.1 is blocked", ip: "172.16.0.1", blocked: true},
		{name: "RFC1918 192.168.1.1 is blocked", ip: "192.168.1.1", blocked: true},
		{name: "IPv6 ULA fd00::1 is blocked", ip: "fd00::1", blocked: true},
		{name: "unspecified 0.0.0.0 is blocked", ip: "0.0.0.0", blocked: true},

		// net.IP.IsPrivate covers only RFC1918, so these ranges were silently
		// reachable. 100.64.0.0/10 is the live one: GKE supports it as a
		// pod/service CIDR, so on such a cluster it reaches ANY pod — exactly
		// the in-cluster reach this guard exists to deny — and the per-session
		// NetworkPolicies do not compensate, because their egress IPBlock is
		// 0.0.0.0/0 with no Except. It is also the range a Tailscale tailnet
		// uses, and this repo ships a tailscale-authkey identity flow whose
		// sidecar shares the runner pod's network namespace.
		{name: "RFC6598 shared address space 100.64.0.1 is blocked", ip: "100.64.0.1", blocked: true},
		{name: "RFC6598 upper bound 100.127.255.255 is blocked", ip: "100.127.255.255", blocked: true},
		{name: "RFC6890 IETF protocol assignments 192.0.0.1 is blocked", ip: "192.0.0.1", blocked: true},
		{name: "RFC2544 benchmarking 198.18.0.1 is blocked", ip: "198.18.0.1", blocked: true},
		{name: "RFC1112 reserved 240.0.0.1 is blocked", ip: "240.0.0.1", blocked: true},

		// The neighbours of 100.64/10 must stay reachable: both are ordinary
		// public space, and blocking them would be an outage, not a hardening.
		{name: "public 100.63.255.255 just below the shared range is allowed", ip: "100.63.255.255", blocked: false},
		{name: "public 100.128.0.0 just above the shared range is allowed", ip: "100.128.0.0", blocked: false},

		// An IPv4-mapped IPv6 address must be judged as the IPv4 address it
		// carries. ::ffff:0:0/96 cannot be a BlockedCIDRs entry — IPNet.Contains
		// truncates it to 0.0.0.0/0 and blocks the internet — so isBlockedIP
		// converts instead, and these pin that it actually happens.
		{name: "IPv4-mapped RFC1918 ::ffff:10.0.0.1 is blocked", ip: "::ffff:10.0.0.1", blocked: true},
		{name: "IPv4-mapped shared-range ::ffff:100.64.0.1 is blocked", ip: "::ffff:100.64.0.1", blocked: true},
		{name: "IPv4-mapped public ::ffff:93.184.216.34 is allowed", ip: "::ffff:93.184.216.34", blocked: false},
		{name: "public IPv4 93.184.216.34 is allowed", ip: "93.184.216.34", blocked: false},
		{name: "public IPv6 2606:2800:220:1:248:1893:25c8:1946 is allowed", ip: "2606:2800:220:1:248:1893:25c8:1946", blocked: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip, "test IP %q must parse", tc.ip)
			assert.Equal(t, tc.blocked, isBlockedIP(ip))
		})
	}
}

func TestGuardHost(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		blocked bool
	}{
		{name: "public hostname is allowed", host: "example.com", blocked: false},
		{name: "public IPv4 literal is allowed", host: "93.184.216.34", blocked: false},
		{name: "loopback IPv4 literal is blocked", host: "127.0.0.1", blocked: true},
		{name: "cloud-metadata IP literal is blocked", host: "169.254.169.254", blocked: true},
		{name: "RFC1918 IP literal is blocked", host: "10.0.0.5", blocked: true},
		{name: "bare localhost is blocked", host: "localhost", blocked: true},
		{name: "a .localhost hostname is blocked", host: "foo.localhost", blocked: true},
		{name: "the bare metadata hostname is blocked", host: "metadata", blocked: true},
		{name: "metadata.google.internal is blocked", host: "metadata.google.internal", blocked: true},
		{name: "a .internal hostname is blocked", host: "svc.internal", blocked: true},
		{name: "a .local hostname is blocked", host: "printer.local", blocked: true},
		{name: "a bare .svc Kubernetes Service name is blocked", host: "myserver.myns.svc", blocked: true},
		{name: "a full .svc.cluster.local Service name is blocked", host: "myserver.myns.svc.cluster.local", blocked: true},
		{name: "a dotless bare hostname is blocked", host: "internal-api", blocked: true},
		{name: "empty host is blocked", host: "", blocked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardHost(tc.host)
			if tc.blocked {
				assert.Error(t, err, "GuardHost(%q) must be refused", tc.host)
			} else {
				assert.NoError(t, err, "GuardHost(%q) must be allowed", tc.host)
			}
		})
	}
}

func TestClientRefusesLoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// httptest.NewServer always binds 127.0.0.1 — the guarded dialer
	// must refuse to connect.
	resp, err := Client().Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "guarded client must refuse a loopback destination")
}

func TestClientRefusesRedirectToBlockedAddress(t *testing.T) {
	// Unit-test the CheckRedirect policy directly: a redirect hop whose
	// target host is the cloud-metadata IP literal must be rejected.
	cl := Client()
	require.NotNil(t, cl.CheckRedirect, "guarded client must set CheckRedirect")

	req := &http.Request{URL: &url.URL{Scheme: "http", Host: "169.254.169.254"}}
	err := cl.CheckRedirect(req, nil)
	assert.Error(t, err, "redirect to a blocked IP literal must be rejected")

	// A public IP-literal redirect target is allowed by the policy
	// (the dialer is still the backstop for hostnames).
	okReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "93.184.216.34"}}
	assert.NoError(t, cl.CheckRedirect(okReq, nil))
}

func TestClientRejectsTooManyRedirects(t *testing.T) {
	cl := Client()
	require.NotNil(t, cl.CheckRedirect)

	pub := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	via := make([]*http.Request, maxRedirects)
	assert.Error(t, cl.CheckRedirect(pub, via), "must reject after the redirect cap")
}

func TestHardenServer(t *testing.T) {
	t.Run("sets slow-loris defenses on a zero-value server", func(t *testing.T) {
		s := &http.Server{}
		HardenServer(s)
		assert.Equal(t, serverReadHeaderTimeout, s.ReadHeaderTimeout, "ReadHeaderTimeout is the slow-loris defense")
		assert.Equal(t, serverIdleTimeout, s.IdleTimeout)
		// Streaming-safe: response/body timeouts are left for the caller.
		assert.Zero(t, s.ReadTimeout, "ReadTimeout left 0 so large bodies are not truncated")
		assert.Zero(t, s.WriteTimeout, "WriteTimeout left 0 so streaming/SSE responses are not truncated")
	})

	t.Run("preserves caller-set values", func(t *testing.T) {
		s := &http.Server{ReadHeaderTimeout: 1, IdleTimeout: 2}
		HardenServer(s)
		assert.EqualValues(t, 1, s.ReadHeaderTimeout, "existing value not overwritten")
		assert.EqualValues(t, 2, s.IdleTimeout)
	})
}
