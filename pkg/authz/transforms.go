package authz

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// transform is the signature for a named-transform function.
type transform func(string) string

// transformEntry is a registered transform plus the one property that decides
// whether it is safe to derive an authorization object id from a free-form
// value with it.
type transformEntry struct {
	fn transform
	// Injective: distinct inputs never produce the same output.
	//
	// This is an AUTHORIZATION property, not a tidiness one. A resource id is
	// the thing an approval is granted against, so two distinct targets that
	// collapse onto one id means approving the first silently authorizes the
	// second. `basename` collapses /etc/passwd and /home/u/passwd;
	// `spicedb_object_id` maps every illegal rune to "-" and dedupes runs, so
	// "a?b" and "a/../b" can meet. Both are fine for tidying an id that is
	// ALREADY a distinct resource, and unsafe for minting one from a value.
	//
	// sha256 is treated as injective on the strength of collision resistance,
	// which is the standard this whole design already leans on.
	injective bool

	// identityPreserving: distinct inputs MAY collapse, but only when they name
	// the SAME real-world resource.
	//
	// Injectivity is a proxy for the property that actually matters — "the value
	// on the card is the only thing that reaches the grant" — and for a
	// case-insensitive namespace it asks the wrong question. GitHub treats
	// Demo-Org/Demo-Repo and demo-org/demo-repo as ONE repository, so folding them
	// together merges two spellings, never two targets. Approving the first does
	// authorize the second, and that is correct: they are the same repository.
	//
	// This is NOT a softer injective. A transform may claim it only when its
	// domain guarantees the collapse is between aliases — `basename` collapses
	// /etc/passwd and /home/u/passwd, which are genuinely different files, and
	// could never claim it. Slot validation accepts either property; nothing
	// else treats them as interchangeable.
	identityPreserving bool
}

// transformRegistry is the closed set of transforms a permission spec
// may reference. New entries are added here; AgentClass validation
// rejects any name not present.
var transformRegistry = map[string]transformEntry{
	"lowercase":         {fn: strings.ToLower, injective: false},
	"remove_spaces":     {fn: removeSpaces, injective: false},
	"spicedb_object_id": {fn: spicedbObjectID, injective: false},
	"basename":          {fn: path.Base, injective: false},
	"sha256":            {fn: sha256Hex, injective: true},
	"normalize_url":     {fn: normalizeURL, injective: true},
	"spicedb_escape":    {fn: spicedbEscape, injective: true},

	// casefold_identity is lowercase, declared for namespaces where case does not
	// distinguish resources — a GitHub owner/name, a DNS label. Same function as
	// `lowercase` and deliberately a SEPARATE entry: the general string op stays
	// unsafe for minting ids, and choosing this name is an author asserting the
	// domain assumption where a reviewer can see it.
	//
	// Using it where case IS significant merges distinct resources and is a bug
	// this registry cannot catch for you.
	"casefold_identity": {fn: strings.ToLower, identityPreserving: true},
	"github_repo_id":    {fn: gitHubRepoID, identityPreserving: true},

	// github_repo_url_id keys the URL-side alias type. It folds every spelling
	// of one repository onto the canonical https form (reusing gitHubRepoID)
	// and then base64url-encodes it, unpadded.
	//
	// base64url rather than spicedb_escape: its alphabet is inside that
	// function's safe set and contains no '=', which spicedb_escape uses as
	// its escape introducer — so an encoded id can never be read as an escaped
	// one. Unpadded for the same reason.
	"github_repo_url_id": {fn: gitHubRepoURLID, identityPreserving: true},
}

// IsRegisteredTransform reports whether the named transform exists.
// Used by AgentClass validation; runtime applies via ApplyTransforms.
func IsRegisteredTransform(name string) bool {
	_, ok := transformRegistry[name]
	return ok
}

// IsInjectiveTransform reports whether the named transform is safe to derive an
// object id from a free-form value with. An unregistered name reports false —
// unknown means unsafe, not "assume the best".
func IsInjectiveTransform(name string) bool {
	e, ok := transformRegistry[name]
	return ok && e.injective
}

// NonInjectiveTransforms returns the names in the chain that may collapse
// distinct values onto one object id, in declaration order.
func NonInjectiveTransforms(names []string) []string {
	var out []string
	for _, n := range names {
		if !IsInjectiveTransform(n) {
			out = append(out, n)
		}
	}
	return out
}

// ApplyTransforms runs the named transforms in order against s.
// Returns an error naming the first unknown transform.
func ApplyTransforms(s string, names []string) (string, error) {
	for _, name := range names {
		e, ok := transformRegistry[name]
		if !ok {
			return "", fmt.Errorf("authz: unknown transform %q", name)
		}
		s = e.fn(s)
	}
	return s, nil
}

// normalizeURL canonicalises a URL so that one approval covers the spellings
// that denote the SAME target, without ever letting two DIFFERENT targets meet.
//
// The direction of risk is counter-intuitive and decides every judgement call
// here. Under-normalising fails CLOSED: this is an allowlist, so an unrecognised
// spelling finds no grant and is denied — a usability cost, and re-spelling a
// denied URL also finds no grant, so it is not an evasion path. OVER-normalising
// fails OPEN: two distinct targets hashing equal means an approval for one
// silently authorises the other. So when in doubt, normalise LESS.
//
// Applied (each provably the same target):
//   - scheme and host lower-cased, ASCII only — both are case-insensitive per
//     RFC 3986. ASCII-only because Unicode case folding can map distinct
//     hostnames together, and an IDN homoglyph pair must stay distinct.
//   - dot-segments resolved on the ESCAPED path, so %2F is never treated as a
//     separator and an encoded slash cannot be folded into a real one.
//   - a default port for the scheme dropped (:80 http, :443 https). Note this
//     is scheme-aware: https://h:80 keeps its port, because there it is real.
//   - fragment dropped — it never reaches the server.
//
// Deliberately PRESERVED verbatim: userinfo (case-sensitive, and
// http://evil.com@good.com/ must never collapse onto http://good.com/), the
// query (stripping it would widen one grant to cover many requests), percent
// encoding, trailing slashes, empty interior segments, and the host's own
// spelling beyond ASCII case. An unparseable input is returned unchanged rather
// than guessed at.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Not an absolute URL this can reason about. Unchanged is the
		// fail-closed answer: it simply will not match a grant.
		return raw
	}

	scheme := asciiLower(u.Scheme)
	host := asciiLower(u.Hostname())
	port := u.Port()
	if defaultPortFor(scheme) == port {
		port = ""
	}

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	if u.User != nil {
		b.WriteString(u.User.String())
		b.WriteByte('@')
	}
	if strings.Contains(host, ":") {
		b.WriteString("[" + host + "]") // IPv6 literal
	} else {
		b.WriteString(host)
	}
	if port != "" {
		b.WriteString(":")
		b.WriteString(port)
	}
	b.WriteString(removeDotSegments(u.EscapedPath()))
	if u.ForceQuery || u.RawQuery != "" {
		b.WriteString("?")
		b.WriteString(u.RawQuery)
	}
	return b.String()
}

// defaultPortFor returns the port that is implied by the scheme, or "" when the
// scheme has none this cares about. Unknown schemes keep whatever port they
// carry — dropping one there could merge two distinct endpoints.
func defaultPortFor(scheme string) string {
	switch scheme {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	default:
		return ""
	}
}

// asciiLower lower-cases ASCII letters and leaves every other byte alone.
// strings.ToLower would apply Unicode case folding, which can map two distinct
// hostnames onto one — the exact collision this transform must not create.
func asciiLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// removeDotSegments implements RFC 3986 §5.2.4 against an already-escaped path.
//
// Hand-rolled rather than path.Clean because Clean also strips trailing slashes,
// and "/a/" and "/a" are routinely different resources — collapsing them is a
// widening this must not do.
// An empty segment is carried through as a segment rather than tracked as a
// separate "trailing slash" flag. The flag version is not idempotent: for
// "///" it both appended the final empty segment AND re-added a slash, so each
// pass ate one — "///" → "//" → "/". A fuzz target caught it, which is the
// argument for having one.
func removeDotSegments(p string) string {
	if p == "" {
		return ""
	}
	abs := strings.HasPrefix(p, "/")
	segs := strings.Split(p, "/")
	if abs {
		segs = segs[1:] // the leading empty segment is the absolute marker
	}
	out := make([]string, 0, len(segs))
	for i, s := range segs {
		last := i == len(segs)-1
		switch s {
		case ".":
			// A dot segment in final position still denotes the directory, so
			// it leaves the trailing slash behind (RFC 3986 §5.2.4).
			if last {
				out = append(out, "")
			}
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		default:
			// Includes "": an interior one is a real empty segment (//) and a
			// final one is the trailing slash. Both are preserved.
			out = append(out, s)
		}
	}
	res := strings.Join(out, "/")
	if abs {
		res = "/" + res
	}
	return res
}

func removeSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// spicedbObjectID coerces an arbitrary string into the SpiceDB
// object-ID charset [a-zA-Z0-9_/\-=|+]. Disallowed runes become "-",
// consecutive "-" deduped, leading/trailing "-" trimmed.
func spicedbObjectID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '/' || r == '-' ||
			r == '=' || r == '|' || r == '+'
		if !ok {
			r = '-'
		}
		if r == '-' && prevDash {
			continue
		}
		b.WriteRune(r)
		prevDash = r == '-'
	}
	return strings.Trim(b.String(), "-")
}

// spicedbEscape encodes an arbitrary string into the SpiceDB object-ID charset
// INJECTIVELY: every distinct input gets a distinct id, and the id stays
// readable enough that a human can recognize what they approved.
//
// It exists because the two properties an approval surface needs were split
// across the transforms that already existed. spicedb_object_id is readable but
// folds every illegal rune onto "-" (so ".com" and ",com" meet); sha256 is
// injective but shows a human nothing. A card displays the value that was
// approved while the grant is written on the transformed id, so a chain that is
// not injective makes the displayed value a claim that is not true — some OTHER
// value also reaches that grant. The agent authors the slot value, so it, not
// the operator, is the party that would go looking for the second preimage.
//
// "=" is the escape character because SpiceDB's object-id charset is
// [a-zA-Z0-9_/\-=|+]: percent-encoding is unavailable ("%" is not legal), and
// "=" is punctuation the charset does permit. Each unsafe BYTE becomes "=XX"
// (uppercase hex), which covers UTF-8 without a separate rune path, and "="
// itself escapes to "=3D" — so "=" never appears literally in the output and
// the encoding decodes back to exactly one input.
func spicedbEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		safe := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '/' || c == '-' || c == '|' || c == '+'
		if safe {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('=')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// UnsafeSlotTransforms returns the names in the chain that may collapse
// distinct RESOURCES onto one object id, in declaration order.
//
// The check a slot declaration must pass. Broader than NonInjectiveTransforms
// because injectivity is a proxy: what a slot needs is that the value shown on
// the card is the only thing reaching the grant, and a transform that folds
// only aliases of one resource satisfies that without being injective.
func UnsafeSlotTransforms(names []string) []string {
	var out []string
	for _, n := range names {
		e, ok := transformRegistry[n]
		if !ok || (!e.injective && !e.identityPreserving) {
			out = append(out, n)
		}
	}
	return out
}

// gitHubRepoID folds every spelling of one GitHub repository onto a single
// canonical id, and leaves everything else exactly as it found it.
//
// It exists because two toolkits name the same repository in different
// vocabularies: git sees a remote URL, gh sees OWNER/NAME. Without a shared
// canonical form the two derive different object ids, so approving a phase
// that reaches one repository writes two grants and shows a human two targets
// where there is one — which is what a reviewer actually saw on a card.
//
// The foldings are all pure spelling of the SAME repository: the scp form
// (git@github.com:o/r), a trailing `.git`, a trailing slash, case (GitHub
// treats owner and name case-insensitively), and the bare OWNER/NAME pair gh
// accepts. It is therefore identityPreserving and NOT injective — two inputs
// deliberately land on one id because they are one repository. Do not mark it
// injective to make it eligible somewhere: that flag means "cannot collide",
// and collapsing is the whole job here.
//
// NOTE this deliberately changes git.yaml's older rule that `.git` and scp
// spellings stay distinct ids. That rule meant cloning with `.git` and pushing
// without it needed two separate grants for one repository.
//
// Anything that is not a github.com repository — a GitLab or self-hosted
// remote, git.yaml's `workspace` sentinel, an empty or malformed value —
// returns unchanged, so the host-agnostic git toolkit can run this on every
// remote without re-keying any non-GitHub repository.
func gitHubRepoID(raw string) string {
	const canonical = "https://github.com/"

	owner, name, ok := gitHubOwnerName(raw)
	if !ok {
		return raw
	}
	return canonical + owner + "/" + name
}

// gitHubRepoURLID is gitHubRepoID's canonical https form, base64url-encoded
// (unpadded) so it keys a SEPARATE resource type from the plain-text
// github_repo_id alias without ever colliding with it or with anything
// spicedb_escape produces. See the registry comment above for why base64url
// specifically.
//
// Dispatches on gitHubOwnerName's own ok bool, not on comparing gitHubRepoID's
// output against raw. That string-equality check looks like a valid proxy for
// "not a github.com repository" but isn't: gitHubRepoID also returns its
// input verbatim when the input is ALREADY its own canonical form —
// "https://github.com/acme/widgets" with lowercase owner/name and no `.git`
// — which is the single most natural way to write a GitHub URL. A proxy that
// cannot tell those two reasons apart leaves that input unencoded, so it
// fails to fold onto the same id as its other spellings (scp, `.git`
// suffix) and fails to decode in resourcedisplay.DeriveB64URLPath, reproducing the
// type-name-fallback regression this transform exists to prevent.
func gitHubRepoURLID(raw string) string {
	owner, name, ok := gitHubOwnerName(raw)
	if !ok {
		// Not a github.com repository: pass through, same as gitHubRepoID.
		return raw
	}
	return base64.RawURLEncoding.EncodeToString([]byte("https://github.com/" + owner + "/" + name))
}

// gitHubOwnerName pulls the (owner, name) pair out of any spelling this
// package recognizes as a github.com repository. The bool is false for
// everything else, which is what keeps the pass-through honest.
func gitHubOwnerName(raw string) (string, string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", false
	}

	switch {
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || asciiLower(u.Hostname()) != "github.com" {
			return "", "", false
		}
		s = u.EscapedPath()
	default:
		// A bare OWNER/NAME, the form `gh --repo` takes. An `@` or `:` means
		// this is some OTHER host's scp-style remote (git@gitlab.com:o/r), and
		// splitting it on the first slash would silently rewrite a GitLab
		// repository into a GitHub one.
		if strings.ContainsAny(s, "@:") {
			return "", "", false
		}
	}

	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	owner, name, found := strings.Cut(s, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		// Not an OWNER/NAME pair: a bare owner, a deep API path, or junk.
		return "", "", false
	}
	return asciiLower(owner), asciiLower(name), true
}
