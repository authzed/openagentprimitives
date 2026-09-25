// Package safehttp provides an SSRF-guarded *http.Client.
//
// Many URL-fetching paths here follow attacker-influenceable URLs — an
// LLM-emitted fetch_url target, an MCP server URL, an OAuth token endpoint
// discovered from a remote document — which unguarded can reach the
// cloud-metadata endpoint (169.254.169.254), loopback, or in-cluster
// services. Client()'s dialer resolves the host, refuses the request if ANY
// resolved IP is private/loopback/link-local, then dials the concrete vetted
// IP so the address checked is the address connected to (closing the
// DNS-rebinding gap). Redirects are re-validated per hop.
//
// Stdlib-only by design: many packages import it, so it must stay
// dependency-free.
package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxRedirects caps how many redirect hops a guarded request may follow.
const maxRedirects = 10

// BlockedCIDRs are the special-use ranges net.IP's own predicates do NOT
// cover. IsPrivate is RFC1918 (10/8, 172.16/12, 192.168/16) plus the IPv6 ULA
// and nothing else, so each range below was silently reachable through an
// SSRF-guarded client.
//
// 100.64.0.0/10 is the one with a live exposure rather than a theoretical one.
// GKE supports it as a pod/service CIDR, so on such a cluster it reaches ANY
// pod — precisely the in-cluster reach this guard exists to deny — and the
// per-session NetworkPolicies do not compensate, because their egress IPBlock
// is 0.0.0.0/0 with no Except clause. It is also the range a Tailscale tailnet
// uses, and this repo ships a tailscale-authkey identity flow whose sidecar
// shares the runner pod's network namespace.
//
// Exported so an operator-facing knob can extend it: 100.64/10 is legitimately
// routable on some networks, and the list is the place to say so rather than a
// second predicate somewhere downstream.
var BlockedCIDRs = mustParseCIDRs(
	"100.64.0.0/10", // RFC 6598 shared address space (CGNAT; GKE pod range; tailnets)
	"192.0.0.0/24",  // RFC 6890 IETF protocol assignments
	"198.18.0.0/15", // RFC 2544 benchmarking
	"240.0.0.0/4",   // RFC 1112 reserved
	"64:ff9b::/96",  // RFC 6052 IPv4/IPv6 translation
	"2001:db8::/32", // RFC 3849 documentation
	"100::/64",      // RFC 6666 discard-only
)

// ::ffff:0:0/96 (IPv4-mapped IPv6) is deliberately NOT in that list. It cannot
// be expressed as an IPNet entry: net.IPNet.Contains calls To4 on the network
// number, ::ffff:0:0 renders as 0.0.0.0, and the /96 mask truncates to /0 — so
// the entry silently becomes "block everything". isBlockedIP handles the case
// directly instead, by re-judging a mapped address as the IPv4 address it
// carries.

func mustParseCIDRs(s ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(s))
	for _, c := range s {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("safehttp: bad blocked CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP reports whether ip is a destination an SSRF-guarded client
// must refuse: loopback, link-local unicast/multicast (covers the IPv4
// 169.254.0.0/16 cloud-metadata range and IPv6 fe80::/10), private
// ranges (RFC1918 and the IPv6 ULA fc00::/7), the unspecified address, and
// every range in BlockedCIDRs.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() ||
		isULA(ip) {
		return true
	}
	// An IPv4-mapped IPv6 address must be judged as the IPv4 address it
	// carries, or ::ffff:10.0.0.1 walks past every predicate above.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range BlockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isULA reports whether ip is an IPv6 unique-local address (fc00::/7).
// net.IP.IsPrivate covers fc00::/7 in modern Go (1.17+), so this is a
// defensive belt-and-suspenders check; keeping it explicit means the
// guarantee does not silently depend on the stdlib version.
func isULA(ip net.IP) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	return v6[0]&0xfe == 0xfc
}

// guardedDialContext resolves addr's host and refuses to connect if any
// resolved IP is blocked. On success it dials a concrete vetted IP
// directly — the IP checked is the IP connected to, which also defeats
// DNS rebinding.
func guardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("safehttp: bad address %q: %w", addr, err)
		}
		// addr's host may itself be an IP literal or a hostname.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("safehttp: resolve %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("safehttp: %q resolved to no addresses", host)
		}
		for _, ipa := range ips {
			if isBlockedIP(ipa.IP) {
				return nil, fmt.Errorf("safehttp: refusing to connect to %q: %s is a blocked (private/loopback/link-local) address", host, ipa.IP)
			}
		}
		// Every resolved IP passed; dial the first concrete one so the
		// connection target is exactly what we vetted.
		var lastErr error
		for _, ipa := range ips {
			conn, derr := base.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, fmt.Errorf("safehttp: dial %q: %w", host, lastErr)
	}
}

// checkRedirect re-validates each redirect hop. If the redirect target
// host is an IP literal it is rejected when blocked; hostname targets
// are caught by the guarded dialer at connect time anyway. Redirects
// are capped at maxRedirects hops.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("safehttp: stopped after %d redirects", maxRedirects)
	}
	host := req.URL.Hostname()
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return fmt.Errorf("safehttp: refusing redirect to blocked address %s", host)
	}
	return nil
}

// GuardHost reports whether host — typically (*url.URL).Hostname(), with no
// scheme or port — is a destination an SSRF-sensitive admission-time content
// check must refuse, WITHOUT performing a DNS lookup: an IP literal in a
// blocked range (see isBlockedIP — loopback, link-local unicast/multicast,
// which covers the 169.254.169.254 cloud-metadata endpoint, private
// RFC1918/ULA, and unspecified), "localhost", a metadata/internal-shaped
// hostname (the bare names "metadata" and "metadata.google.internal", or a
// ".internal"/".local" suffix — the same classification
// pkg/platform/oap/oci's registry guard uses for the same threat), or a
// Kubernetes in-cluster Service DNS name (a ".svc"/".svc.cluster.local"/
// ".cluster.local" suffix, or a bare name with no dot at all — the cluster
// resolver's search-domain expansion tries those before ever treating a name
// as absolute).
//
// This is a pre-dial, static content check, not a substitute for Client()'s
// guarded dialer: a hostname that looks public but is DNS-rebound to a
// blocked address at connect time is caught there, not here — the same
// division checkRedirect already draws for a redirect hop (an IP literal is
// checked immediately; a hostname is checked at connect time, so the address
// vetted is the address actually dialed).
func GuardHost(host string) error {
	if host == "" {
		return fmt.Errorf("safehttp: empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("safehttp: host %q is a blocked (private/loopback/link-local) address", host)
		}
		return nil
	}
	lower := strings.ToLower(host)
	switch {
	case lower == "localhost" || strings.HasSuffix(lower, ".localhost"):
		return fmt.Errorf("safehttp: host %q is loopback", host)
	case lower == "metadata" || lower == "metadata.google.internal":
		return fmt.Errorf("safehttp: host %q is a cloud-metadata hostname", host)
	case strings.HasSuffix(lower, ".internal") || strings.HasSuffix(lower, ".local"):
		return fmt.Errorf("safehttp: host %q looks like a cloud-metadata/internal hostname", host)
	case strings.HasSuffix(lower, ".svc") || strings.HasSuffix(lower, ".svc.cluster.local") || strings.HasSuffix(lower, ".cluster.local"):
		return fmt.Errorf("safehttp: host %q is a Kubernetes in-cluster Service name", host)
	case !strings.Contains(lower, "."):
		return fmt.Errorf("safehttp: host %q has no dot and would resolve via the cluster resolver's search domain", host)
	}
	return nil
}

// Client returns an *http.Client whose transport dials only non-private,
// non-loopback, non-link-local destinations, re-validates redirects, and
// times out after 30s overall. The returned client is safe for
// concurrent use; callers may copy it and tighten Timeout if they need a
// shorter deadline.
func Client() *http.Client {
	base := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           guardedDialContext(base),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
		Timeout:       30 * time.Second,
	}
}
