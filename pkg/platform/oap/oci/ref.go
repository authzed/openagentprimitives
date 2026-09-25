// Package oci pushes and pulls .oap agent containers (the OCI-layout tar
// produced by pkg/platform/oap.Pack) to and from OCI-compliant registries, using
// oras-go/v2 for the registry-protocol plumbing.
package oci

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"strings"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Options configures how Push/Pull reach the remote registry.
type Options struct {
	// PlainHTTP forces plain HTTP (no TLS) — for loopback/dev registries.
	// Real deployments should leave this false so oras-go's default HTTPS
	// transport is used. It also relaxes guardHost's private-range/loopback
	// refusal (but never the link-local/metadata refusal — see guardHost).
	PlainHTTP bool

	// Credential, if non-nil, authenticates against ref's registry host
	// (basic or bearer, per the distribution auth flow). Nil means anonymous
	// access, which is fine for public pulls and the loopback test registry.
	Credential *auth.Credential

	// AllowHosts is an explicit allowlist of registry hosts (as they appear
	// in registry.ParseReference(ref).Registry — "host" or "host:port") that
	// bypass guardHost's refusal entirely, including the always-on
	// link-local/metadata refusal. Use this for a deliberately-trusted
	// internal registry that would otherwise be classified as private-range
	// or `.internal`.
	AllowHosts []string
}

// ErrHostNotTrusted is the sentinel wrapped by guardHost's refusal errors, so
// callers can distinguish "this ref was blocked by the SSRF/host-trust guard"
// from other repository()/parse failures with errors.Is.
var ErrHostNotTrusted = errors.New("oap/oci: registry host not trusted")

// guardHost is the SSRF / host-trust gate repository() runs before ever dialing
// ref's registry: it classifies ref's host and refuses hosts that are plausible
// SSRF targets, unless the caller explicitly allow-listed that exact host (or
// host:port). In order:
//
//  1. Allow-listed (exact match on ref's "host" or "host:port") — always OK,
//     even a link-local/metadata literal. The allowlist is an explicit,
//     caller-asserted trust decision and wins over every other rule.
//  2. Link-local IP literal (169.254.0.0/16, which includes the cloud metadata
//     endpoint 169.254.169.254, or IPv6 fe80::/10), OR a metadata-shaped
//     hostname (`.internal`/`.local` suffix, or the bare names "metadata" /
//     "metadata.google.internal") — ALWAYS refused, even with PlainHTTP. Cloud
//     metadata SSRF is the top threat this guard exists for, so PlainHTTP —
//     which legitimately relaxes the next rule — must not relax this one.
//  3. Private-range IP literal (10/8, 172.16/12, 192.168/16, IPv6 ULA fc00::/7)
//     or loopback (127/8, ::1, "localhost") — refused UNLESS opts.PlainHTTP.
//     Local dev registries reached over PlainHTTP are legitimate and common;
//     requiring PlainHTTP here means an attacker cannot silently redirect a
//     "real" HTTPS pull at an internal host without the caller already having
//     opted out of TLS.
//  4. Anything else (a public hostname/IP) — allowed.
//
// Classification is string/IP-literal based, inspecting ref's Registry field as
// written. It does NOT resolve DNS and re-check the resolved address, so a
// hostname that looks public but resolves to 169.254.169.254 — or is switched
// to do so after this check (DNS rebinding) — is not caught here. Closing that
// needs a resolve-then-dial-pinned transport, deeper than this ref-literal gate.
func guardHost(ref string, allow []string, plainHTTP bool) error {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	host := parsed.Registry // "host" or "host:port"
	hostOnly := host
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		hostOnly = h
	}

	for _, a := range allow {
		if a == host || a == hostOnly {
			return nil
		}
	}

	lowerHost := strings.ToLower(hostOnly)

	addr, ipErr := netip.ParseAddr(hostOnly)
	isIP := ipErr == nil

	// Rule 2: link-local / metadata — always refused, PlainHTTP does not
	// relax this.
	if isIP && addr.IsLinkLocalUnicast() {
		return fmt.Errorf("%w: %q is a link-local address (cloud metadata endpoints live here; never dialed, even with PlainHTTP)", ErrHostNotTrusted, host)
	}
	if !isIP && isMetadataHostname(lowerHost) {
		return fmt.Errorf("%w: %q looks like a cloud-metadata/internal hostname (never dialed unless allow-listed)", ErrHostNotTrusted, host)
	}

	// Rule 3: private-range / loopback — refused unless PlainHTTP.
	if isIP && (addr.IsPrivate() || addr.IsLoopback()) {
		if plainHTTP {
			return nil
		}
		return fmt.Errorf("%w: %q is a private/loopback address (set PlainHTTP for a trusted local/dev registry, or allow-list the host)", ErrHostNotTrusted, host)
	}
	if !isIP && lowerHost == "localhost" {
		if plainHTTP {
			return nil
		}
		return fmt.Errorf("%w: %q is loopback (set PlainHTTP for a trusted local/dev registry, or allow-list the host)", ErrHostNotTrusted, host)
	}

	return nil
}

// isMetadataHostname reports whether host (already lowercased) matches one
// of the common cloud-metadata / internal-service hostname shapes: an
// explicit ".internal"/".local" suffix (GCP/mDNS internal-DNS conventions),
// or the bare names cloud providers conventionally resolve their metadata
// endpoint under.
func isMetadataHostname(host string) bool {
	if host == "metadata" || host == "metadata.google.internal" {
		return true
	}
	return strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local")
}

// repository builds the oras remote.Repository client for ref
// ("host[:port]/name[:tag|@digest]"). Ref parsing is delegated entirely to
// oras-go's remote.NewRepository.
//
// guardHost runs BEFORE remote.NewRepository ever dials: it refuses SSRF-shaped
// hosts unless the caller allow-listed the host or, for the private/loopback
// case only, opted into PlainHTTP. See guardHost for the full classification
// and its DNS-rebinding caveat.
func repository(ref string, opts Options) (*remote.Repository, error) {
	if err := guardHost(ref, opts.AllowHosts, opts.PlainHTTP); err != nil {
		return nil, err
	}
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	repo.PlainHTTP = opts.PlainHTTP
	if opts.Credential != nil {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: auth.StaticCredential(repo.Reference.Registry, *opts.Credential),
		}
	}
	return repo, nil
}

// PinnedRef rewrites ref to address the exact manifest named by dig: same
// registry and repository, any tag or existing digest replaced by "@<dig>".
// Callers use it to pin a fetch to a digest they just cryptographically
// verified, so a mutable tag cannot be re-resolved to different content between
// the verify and the fetch (a TOCTOU gap). dig must be a syntactically valid
// digest — a non-digest string is rejected rather than silently becoming a tag.
// Parsing goes through registry.ParseReference, never hand-rolled splitting.
func PinnedRef(ref, dig string) (string, error) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	if _, err := digest.Parse(dig); err != nil {
		return "", fmt.Errorf("pin ref %q: invalid digest %q: %w", ref, dig, err)
	}
	return parsed.Registry + "/" + parsed.Repository + "@" + dig, nil
}

// DefaultLocalName returns the conventional local filename for ref: the last
// path segment of the OCI repository name (host and any tag/digest dropped)
// plus oap.DefaultExtension — "registry.example/team/pm-agent:v1" becomes
// "pm-agent.oap". `oap agent pull` uses it as the default -o path.
//
// Parsing goes through registry.ParseReference, not string splitting: a naive
// split on ":" breaks on "localhost:5000/team/pm-agent:v1", where the host
// itself contains a colon.
func DefaultLocalName(ref string) (string, error) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	return path.Base(parsed.Repository) + oap.DefaultExtension, nil
}
