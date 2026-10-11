// Package browserpolicy defines the host-matching and allow/deny-decision
// rules shared by the browser toolbox's per-session egress policy: the
// reconciler that writes the policy ConfigMap, the gate that reloads and
// enforces it, and the runner-side tools that need to pre-check a host
// before issuing a request. It is pure and stdlib-only so every one of
// those can import it without creating a dependency cycle.
package browserpolicy

import (
	"fmt"
	"net/netip"
	"strings"
)

const (
	// ModeAllowlist means only hosts covered by a Site in PolicyDoc.Sites
	// are permitted; everything else is denied.
	ModeAllowlist = "allowlist"
	// ModeBlocklist means every host is permitted except those covered by
	// a Site in PolicyDoc.Sites.
	ModeBlocklist = "blocklist"
)

// Site is a single host entry in a PolicyDoc. Host is matched against a
// candidate host by Match: by equality always, and additionally by
// suffix-by-label (subdomains of Host) unless Exact is set.
type Site struct {
	Host  string `json:"host"`
	Exact bool   `json:"exact,omitempty"`
}

// PolicyDoc is the JSON document the reconciler writes into the per-session
// ConfigMap and the gate reloads on change.
type PolicyDoc struct {
	Mode  string `json:"mode"`
	Sites []Site `json:"sites"`
}

// Decision is the result of evaluating a host against a PolicyDoc.
type Decision struct {
	Allowed bool
	// Rule names why the decision came out the way it did. One of
	// "not in allowlist", "explicit denylist", "private or metadata
	// address", or "" when Allowed is true.
	Rule string
}

// CanonicalHost lowercases, strips exactly one trailing dot, and rejects
// anything with a scheme, port, path, space, or empty labels.
func CanonicalHost(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("browserpolicy: empty host")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("browserpolicy: host %q contains whitespace", raw)
	}
	if strings.Contains(raw, "://") {
		return "", fmt.Errorf("browserpolicy: host %q contains a scheme", raw)
	}
	if strings.ContainsAny(raw, "/\\?#") {
		return "", fmt.Errorf("browserpolicy: host %q contains a path", raw)
	}

	host := strings.ToLower(raw)

	// Strip exactly one trailing dot. A second trailing dot (or any other
	// empty label) is caught by the label-emptiness check below.
	host = strings.TrimSuffix(host, ".")

	// A bracketed IPv6 literal (e.g. "[::1]" or "[::1]:443") is rejected:
	// we don't accept bracket syntax at all here.
	if strings.Contains(host, "[") || strings.Contains(host, "]") {
		return "", fmt.Errorf("browserpolicy: host %q contains brackets", raw)
	}

	if host == "" {
		return "", fmt.Errorf("browserpolicy: host %q is empty after normalization", raw)
	}

	// An IP literal (v4 or v6) is a valid canonical host as-is; short-circuit
	// it BEFORE the colon check so plain IPv6 literals ("::1") survive, and
	// skip the label-emptiness checks that only make sense for DNS names.
	if _, err := netip.ParseAddr(host); err == nil {
		return host, nil
	}

	// Not an IP literal, so any remaining colon is a port (or colon
	// garbage), never IPv6 syntax. Rejecting ANY colon — not exactly one —
	// closes the "evil.com:80:81" bypass, which would otherwise pass through
	// unchanged and then silently fail to match a blocklisted "evil.com".
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("browserpolicy: host %q contains a port or stray colon", raw)
	}

	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return "", fmt.Errorf("browserpolicy: host %q has an empty label", raw)
		}
	}

	return host, nil
}

// Match reports whether host (already canonical) is covered by site:
// equality always; suffix-by-label (".{site.Host}" suffix) unless
// site.Exact.
func Match(site Site, host string) bool {
	if host == site.Host {
		return true
	}
	if site.Exact {
		return false
	}
	return strings.HasSuffix(host, "."+site.Host)
}

// cgnat is the CGNAT shared-address space (RFC 6598, 100.64.0.0/10). Carrier-
// grade NAT space is not "private" in net/netip's sense -- addr.IsPrivate()
// only covers RFC1918 -- so it needs its own explicit check here.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsPrivateOrMetadataHost reports whether host is an IP literal in
// RFC1918, loopback, link-local (incl. 169.254.169.254),
// unique-local/v6-equivalent space, the CGNAT shared-address space
// (100.64.0.0/10), or the unspecified address (0.0.0.0 / ::).
// Non-IP-literal hosts (DNS names) always report false here; the caller
// is responsible for resolving a name before it reaches this check, if
// that matters for the caller's threat model.
func IsPrivateOrMetadataHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	// Unmap a 4-in-6 address (e.g. ::ffff:0.0.0.0 or ::ffff:127.0.0.1) to its
	// IPv4 form up front. net/netip's IsPrivate/IsLoopback/IsLinkLocalUnicast
	// unmap internally, but IsUnspecified and cgnat.Contains do NOT, so a mapped
	// ::ffff:0.0.0.0 would otherwise slip past the unspecified check (it is
	// neither == 0.0.0.0 nor == ::) and reach loopback — on Linux dialing
	// 0.0.0.0 connects to localhost, re-opening SSRF to the pod itself,
	// including Chrome's own unauthenticated DevTools port. Unmapping once makes
	// every predicate below see the canonical form.
	addr = addr.Unmap()
	// IsUnspecified: on Linux, dialing 0.0.0.0 (or ::) connects to loopback,
	// so the unspecified address is a localhost-SSRF vector, not a no-op.
	return addr.IsPrivate() ||
		addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsUnspecified() ||
		cgnat.Contains(addr)
}

// Decide canonicalizes host and applies mode over sites. Private/metadata
// literals are denied in BOTH modes, before mode logic. A host that fails
// canonicalization is denied using the mode's own deny rule rather than
// surfacing the canonicalization error to the caller.
func Decide(mode string, sites []Site, rawHost string) Decision {
	host, err := CanonicalHost(rawHost)
	if err != nil {
		// A malformed host can't be evaluated against sites at all; fail
		// closed using the mode's own deny rule rather than surfacing the
		// canonicalization error to the caller.
		return Decision{Allowed: false, Rule: denyRuleFor(mode)}
	}

	if IsPrivateOrMetadataHost(host) {
		return Decision{Allowed: false, Rule: "private or metadata address"}
	}

	switch mode {
	case ModeAllowlist:
		for _, site := range sites {
			if Match(site, host) {
				return Decision{Allowed: true}
			}
		}
		return Decision{Allowed: false, Rule: "not in allowlist"}
	case ModeBlocklist:
		for _, site := range sites {
			if Match(site, host) {
				return Decision{Allowed: false, Rule: "explicit denylist"}
			}
		}
		return Decision{Allowed: true}
	default:
		return Decision{Allowed: false, Rule: denyRuleFor(mode)}
	}
}

// denyRuleFor returns the Rule a Decision carries when mode itself denies a
// host outright (a malformed host, or an unrecognized mode) rather than
// denying by site evaluation.
func denyRuleFor(mode string) string {
	if mode == ModeBlocklist {
		return "explicit denylist"
	}
	return "not in allowlist"
}
