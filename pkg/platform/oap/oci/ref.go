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

	// Credential, if non-nil, authenticates against CredentialHost (basic or
	// bearer, per the distribution auth flow). Nil means anonymous access,
	// which is fine for public pulls and the loopback test registry.
	Credential *auth.Credential

	// CredentialHost is the one registry host ("host" or "host:port", as in
	// registry.ParseReference(ref).Registry) Credential is offered to. A ref
	// naming any other host is accessed anonymously: the ref often comes from
	// someone other than the credential's owner (a pasted install link), and
	// keying on the ref's own host would hand the credential to whichever
	// registry that someone chose. Required when Credential is set, and must
	// pass ValidateCredentialHost. Matched by hostsEqual: case-insensitive,
	// port included, so "ghcr.io" does not cover "ghcr.io:8443".
	CredentialHost string

	// RequireCredentialHost turns a ref on a host other than CredentialHost
	// into ErrCredentialHostMismatch instead of anonymous access. Set it when
	// the caller picked ref itself (push, sign), where a mismatch is a typo
	// that would otherwise surface as an unexplained 401. Leave it unset when
	// ref may come from someone else (pull, install), where going anonymous
	// is the protection.
	RequireCredentialHost bool

	// AllowPlainHTTPCredential permits sending Credential over PlainHTTP to a
	// non-loopback CredentialHost, such as a LAN or docker-compose registry
	// with no TLS. Without it that is ErrCredentialOverPlainHTTP, since anyone
	// on the network path can read the credential. Loopback never needs it.
	AllowPlainHTTPCredential bool

	// AllowHosts is an explicit allowlist of registry hosts (as they appear
	// in registry.ParseReference(ref).Registry — "host" or "host:port") that
	// bypass guardHost's refusal entirely, including the always-on
	// link-local/metadata refusal. Use this for a deliberately-trusted
	// internal registry that would otherwise be classified as private-range
	// or `.internal`. Matched by hostsEqual against the ref's "host:port" and
	// its bare host, so a portless entry covers every port: a broader match
	// than CredentialHost's on purpose, since trusting a host to be dialed is
	// a weaker grant than handing it a secret.
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
//     The unspecified address (0.0.0.0, ::) is refused here too: dialing it
//     reaches the local host, and no registry is legitimately addressed by it.
//  3. Private-range IP literal (10/8, 172.16/12, 192.168/16, IPv6 ULA fc00::/7)
//     or loopback (127/8, ::1, "localhost") — refused UNLESS opts.PlainHTTP.
//     Local dev registries reached over PlainHTTP are legitimate and common;
//     requiring PlainHTTP here means an attacker cannot silently redirect a
//     "real" HTTPS pull at an internal host without the caller already having
//     opted out of TLS.
//  4. Anything else (a public hostname/IP) — allowed.
//
// Classification is string/IP-literal based, inspecting ref's Registry field as
// written, after registryHostAddr strips the port or bare IPv6 brackets and
// unmaps IPv4-in-IPv6 ("[::ffff:169.254.169.254]" is the metadata IP). It does NOT resolve DNS and re-check the resolved address, so a
// hostname that looks public but resolves to 169.254.169.254 — or is switched
// to do so after this check (DNS rebinding) — is not caught here. Closing that
// needs a resolve-then-dial-pinned transport, deeper than this ref-literal gate.
func guardHost(ref string, allow []string, plainHTTP bool) error {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	host := parsed.Registry // "host" or "host:port"
	hostOnly, addr, isIP := registryHostAddr(host)

	for _, a := range allow {
		if hostsEqual(a, host) || hostsEqual(a, hostOnly) {
			return nil
		}
	}

	lowerHost := strings.ToLower(hostOnly)

	// Rule 2: link-local / metadata / unspecified — always refused, PlainHTTP
	// does not relax this.
	if isIP && addr.IsLinkLocalUnicast() {
		return fmt.Errorf("%w: %q is a link-local address (cloud metadata endpoints live here; never dialed, even with PlainHTTP)", ErrHostNotTrusted, host)
	}
	if isIP && addr.IsUnspecified() {
		return fmt.Errorf("%w: %q is the unspecified address (dials the local host; never dialed unless allow-listed)", ErrHostNotTrusted, host)
	}
	if !isIP && isMetadataHostname(lowerHost) {
		return fmt.Errorf("%w: %q looks like a cloud-metadata/internal hostname (never dialed unless allow-listed)", ErrHostNotTrusted, host)
	}

	// Rule 3: private-range / loopback — refused unless PlainHTTP.
	if isLoopbackHost(host) {
		if plainHTTP {
			return nil
		}
		return fmt.Errorf("%w: %q is loopback (set PlainHTTP for a trusted local/dev registry, or allow-list the host)", ErrHostNotTrusted, host)
	}
	if isIP && addr.IsPrivate() {
		if plainHTTP {
			return nil
		}
		return fmt.Errorf("%w: %q is a private-range address (set PlainHTTP for a trusted local/dev registry, or allow-list the host)", ErrHostNotTrusted, host)
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
	useCred, err := credentialApplies(repo.Reference.Registry, opts)
	if err != nil {
		return nil, err
	}
	if useCred {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: auth.StaticCredential(repo.Reference.Registry, *opts.Credential),
		}
	}
	return repo, nil
}

// ErrCredentialOverPlainHTTP is returned when a credential would be sent to a
// non-loopback host over plain HTTP, where anyone on the network path can
// read it, without Options.AllowPlainHTTPCredential.
var ErrCredentialOverPlainHTTP = errors.New("oap/oci: refusing to send registry credentials over plain HTTP to a non-loopback host")

// ErrCredentialHostMismatch is returned under Options.RequireCredentialHost
// when ref names a host other than Options.CredentialHost.
var ErrCredentialHostMismatch = errors.New("oap/oci: ref's registry host is not the credential's host")

// ValidateCredentialHost checks that host is a bare registry host exactly as
// a ref would spell it ("ghcr.io", "localhost:5000", "[::1]:5000"), so it can
// ever match one. Parsing goes through registry.ParseReference; on top of its
// rules, an empty port ("ghcr.io:") and a trailing dot ("ghcr.io.") are
// refused because no ref is written that way and the host would silently never
// match.
func ValidateCredentialHost(host string) error {
	parsed, err := registry.ParseReference(host + "/x")
	if err != nil || parsed.Registry != host {
		return fmt.Errorf("oap/oci: credential host %q is not a bare registry host such as ghcr.io or localhost:5000", host)
	}
	hostOnly := host
	if h, port, splitErr := net.SplitHostPort(host); splitErr == nil {
		if port == "" {
			return fmt.Errorf("oap/oci: credential host %q has an empty port", host)
		}
		hostOnly = h
	}
	if strings.HasSuffix(hostOnly, ".") {
		return fmt.Errorf("oap/oci: credential host %q has a trailing dot", host)
	}
	return nil
}

// credentialApplies reports whether opts.Credential should be offered to
// host. It is false for anonymous access and, unless RequireCredentialHost
// makes it an error, for any host other than opts.CredentialHost. A
// credential with no valid CredentialHost is an error rather than "offer it
// everywhere", and so is a matching host reached over plain HTTP unless that
// host is loopback or the caller set AllowPlainHTTPCredential.
func credentialApplies(host string, opts Options) (bool, error) {
	if opts.Credential == nil {
		return false, nil
	}
	if opts.CredentialHost == "" {
		return false, errors.New("oap/oci: Credential set without CredentialHost")
	}
	if err := ValidateCredentialHost(opts.CredentialHost); err != nil {
		return false, err
	}
	if !hostsEqual(host, opts.CredentialHost) {
		if opts.RequireCredentialHost {
			return false, fmt.Errorf("%w: ref is on %q, credential is for %q", ErrCredentialHostMismatch, host, opts.CredentialHost)
		}
		return false, nil
	}
	if opts.PlainHTTP && !isLoopbackHost(host) && !opts.AllowPlainHTTPCredential {
		return false, fmt.Errorf("%w: %q", ErrCredentialOverPlainHTTP, host)
	}
	return true, nil
}

// hostsEqual is the one equality rule for registry hosts across this package
// (AllowHosts, CredentialHost): case-insensitive, since DNS names are, and
// otherwise exact, so a port must match when both sides carry one.
func hostsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// isLoopbackHost reports whether host ("host" or "host:port") is "localhost"
// or a loopback IP literal. guardHost and credentialApplies both classify
// loopback through it, so the SSRF gate and the credential gate cannot drift.
func isLoopbackHost(host string) bool {
	hostOnly, addr, isIP := registryHostAddr(host)
	if isIP {
		return addr.IsLoopback()
	}
	return strings.EqualFold(hostOnly, "localhost")
}

// registryHostAddr splits a registry host as a ref spells it ("ghcr.io",
// "10.0.0.5:5000", "[fd00::1]:5000", "[fd00::1]") into the bare host and, for
// an IP literal, its address. net.SplitHostPort alone is not enough: it
// rejects a bracketed IPv6 literal with no port, which would leave the
// brackets on and let "[fe80::1]" pass as a hostname. An IPv4-in-IPv6 address
// is unmapped so "[::ffff:10.0.0.1]" classifies as the IPv4 address it is.
func registryHostAddr(host string) (hostOnly string, addr netip.Addr, isIP bool) {
	hostOnly = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		hostOnly = host[1 : len(host)-1]
	}
	addr, err := netip.ParseAddr(hostOnly)
	if err != nil {
		return hostOnly, netip.Addr{}, false
	}
	return hostOnly, addr.Unmap(), true
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

// splitPrefix validates prefix and returns its registry host ("host[:port]")
// and repository path (with the trailing "/"). A prefix is "host[:port]/path/…/":
// trailing slash required, no scheme, tag or digest, a non-empty host, and at
// least one non-empty path segment.
func splitPrefix(prefix string) (host, repoPath string, err error) {
	bad := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("invalid ref prefix %q: %s", prefix, why)
	}
	if !strings.HasSuffix(prefix, "/") {
		return bad("must end with '/'")
	}
	if strings.Contains(prefix, "://") {
		return bad("must not include a scheme")
	}
	host, repoPath, found := strings.Cut(prefix, "/")
	if !found || host == "" {
		return bad("missing registry host")
	}
	if repoPath == "" {
		return bad("needs at least one path segment")
	}
	if strings.ContainsAny(repoPath, ":@") {
		return bad("must not include a tag or digest")
	}
	for _, seg := range strings.Split(strings.TrimSuffix(repoPath, "/"), "/") {
		if seg == "" {
			return bad("empty path segment")
		}
	}
	// RefHasPrefix compares hosts case-exactly (and the key pin does the
	// same), so an uppercase entry would admit a link the pin treats as
	// unpinned. Refuse it rather than normalize.
	if host != strings.ToLower(host) {
		return bad("registry host must be lowercase")
	}
	// The prefix plus a probe repository name must parse as a real reference,
	// so host and path characters are held to the same rules as refs.
	parsed, perr := registry.ParseReference(prefix + "x")
	if perr != nil {
		return bad(perr.Error())
	}
	if parsed.Registry != host || parsed.Repository+"/" != repoPath+"x/" {
		return bad("does not round-trip through reference parsing")
	}
	return host, repoPath, nil
}

// ValidatePrefix reports whether prefix is a well-formed ref prefix:
// "host[:port]/path/…/" with a required trailing slash, no scheme, tag or
// digest, a non-empty host and at least one path segment.
func ValidatePrefix(prefix string) error {
	_, _, err := splitPrefix(prefix)
	return err
}

// RefHasPrefix reports whether ref lives under prefix. ref is parsed with
// registry.ParseReference and compared field-wise: the registry must equal the
// prefix host exactly (case- and port-sensitive, so "GHCR.io" or "ghcr.io:443"
// never match "ghcr.io"), and repository+"/" must start with the prefix path,
// so "org/agents-evil/x" does not match "org/agents/". Unparseable refs and
// invalid prefixes are errors, never matches.
func RefHasPrefix(ref, prefix string) (bool, error) {
	host, repoPath, err := splitPrefix(prefix)
	if err != nil {
		return false, err
	}
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return false, fmt.Errorf("parse oci ref %q: %w", ref, err)
	}
	return parsed.Registry == host && strings.HasPrefix(parsed.Repository+"/", repoPath), nil
}
