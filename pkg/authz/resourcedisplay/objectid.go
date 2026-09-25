// Package resourcedisplay turns a SpiceDB object id into something a human
// reads — a short label, and, when the id is itself a URL, a link target.
//
// It exists because two independent surfaces need the SAME derivation from
// the same ids: pkg/authz/plangate renders an approval card's resource line,
// and pkg/web/admind's Directory panel renders a synced scope's row. Both
// read ids this repo mints (base64url of a canonical https URL, most
// notably), and both must fall back identically when an id does not decode.
// A second copy of base64-and-URL handling in the console is exactly the
// hand-rolled near-equivalent CLAUDE.md's prefer-a-library rule rejects —
// these functions live here so there is one.
//
// Every function here is total and fallback-shaped: an input it cannot make
// sense of derives the EMPTY string, never a guess and never a partial
// render. The empty result is what tells a caller to fall back to whatever
// it showed before — the raw id, or a declared type name.
package resourcedisplay

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// DeriveURLPath shortens an absolute URL to its path, trimmed of leading and
// trailing slashes: "https://github.com/demo-org/demo-repo" ->
// "demo-org/demo-repo". A value that does not parse as an absolute URL (no
// host) or whose path is empty derives nothing — this is a label, not a
// best-effort guess, and an empty result is what tells the caller to fall
// back instead.
func DeriveURLPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

// DecodeB64URL decodes an unpadded base64url object id, reporting whether it
// decoded at all. Unpadded RawURLEncoding is the one encoding this repo mints
// such ids with (pkg/authz/transforms.go's gitHubRepoURLID and the GitHub
// directory sync's own repoURLObjectID), so accepting a second encoding here
// would accept ids nothing in this repo can produce.
//
// A false result means "this id is not one of ours" — the caller renders the
// raw id rather than anything derived from a partial decode.
func DecodeB64URL(raw string) (string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// DeriveB64URLPath decodes a base64url object id and then takes the URL path,
// so a surface shows `acme/widgets` rather than an opaque blob.
//
// Decoding FIRST is the whole point: url.Parse on the encoded form finds no
// host, returns "", and every card falls back to printing the type name —
// the regression toolkits/gh.yaml's own comment records from when ids were
// not full URLs. Anything undecodable yields "" and the caller falls back,
// which is the same failure this deriver has always had for an unparseable
// value.
func DeriveB64URLPath(raw string) string {
	decoded, ok := DecodeB64URL(raw)
	if !ok {
		return ""
	}
	return DeriveURLPath(decoded)
}

// DeriveLastSegment takes the final "/"-separated segment of the raw value.
// Unlike DeriveURLPath this does not require the value to be a URL at all —
// it works equally on a path-shaped identifier ("org/proj/issues/42") or a
// bare token with no slash, in which case the whole value is its own single
// segment.
func DeriveLastSegment(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if trimmed == "" {
		return ""
	}
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

// EligibleHref decides whether raw is safe to render as a real link, and if
// so returns it UNCHANGED — never transformed, never re-encoded. That is
// what makes text == href a structural guarantee rather than a convention to
// maintain: the only string this function can ever return is the exact input
// it was given, so a caller that displays the same raw value as both the
// link's text and its target can never make them diverge.
//
// https only, by design — no http, no javascript:, no data:, no file:. The
// classic link-spoofing attack pairs an innocent-looking label with a
// dangerous target; restricting to a single, inert scheme and returning the
// input verbatim removes the transformation step where such a substitution
// could ever be introduced.
func EligibleHref(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return ""
	}
	return raw
}

// ObjectIDDecoder names one member of the CLOSED set of ways an object id
// turns into a human title and a link target. It is a named string type, not
// a free-form one, for the same reason plangate's labelDerivers map is a
// closed registry: every entry is a function this package authored, so a
// declaration elsewhere can only SELECT a presentation, never invent one.
//
// An unrecognized decoder derives nothing. That is deliberate and is the safe
// direction: a kind naming a decoder that does not exist shows raw ids,
// exactly as a kind that declared none does.
type ObjectIDDecoder string

// DecoderB64URL reads the object id as unpadded base64url of an absolute
// https URL — the encoding pkg/authz/transforms.go's github_repo_url_id
// transform and the GitHub directory sync's repoURLObjectID both mint.
const DecoderB64URL ObjectIDDecoder = "b64url"

// Decode returns the human title and the link href for raw under this
// decoder.
//
// Both are empty when the decoder is unrecognized or raw does not decode —
// one answer, not two, because a title without the value it came from is a
// guess. A title with an EMPTY href is legitimate and expected: the id
// decoded to something nameable that is not an https URL (an http one, say),
// so it is nameable but not linkable.
func (d ObjectIDDecoder) Decode(raw string) (title, href string) {
	switch d {
	case DecoderB64URL:
		decoded, ok := DecodeB64URL(raw)
		if !ok {
			return "", ""
		}
		title = DeriveURLPath(decoded)
		if title == "" {
			// Decoded, but to something with no path to name. Falling back to
			// the raw id is the caller's job; returning the decoded-but-unnamed
			// string here would render a bare host as though it were a label.
			return "", ""
		}
		return title, EligibleHref(decoded)
	case DecoderB64Text:
		decoded, ok := DecodeB64URL(raw)
		if !ok {
			return "", ""
		}
		// NEVER an href, under any input. See DecoderB64Text's own doc: this
		// decoder's payload is text somebody typed into a directory, and a
		// link whose text and target were authored by different parties is
		// the whole shape of a spoofed link. Returning "" here is what makes
		// "a display name cannot become a link target" a property of the code
		// rather than of every caller remembering to check.
		return SanitizeLabelText(decoded), ""
	default:
		return "", ""
	}
}
