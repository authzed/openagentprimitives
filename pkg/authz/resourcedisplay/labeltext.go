package resourcedisplay

import (
	"encoding/base64"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the TEXT half of this package: an object id whose payload is a
// name somebody typed into a directory, rather than a URL this repo minted.
// objectid.go's derivations all start from a value with a shape we control (an
// https URL, a path); these start from free-form input and are written
// defensively for that reason.

// DecoderB64Text reads the object id as unpadded base64url of a display name
// — the encoding EncodeB64Text mints, and the one a directory sync's #label
// tuple carries as its `string` subject id.
//
// Distinct from DecoderB64URL rather than a special case of it, because the
// two payloads have opposite trust properties and must not share a code path.
// A b64url id decodes to a URL this repo constructed from a forge's canonical
// fields, so deriving a link target from it is safe. A b64text id decodes to
// whatever a person named a channel, so this decoder derives a title and
// NEVER an href, whatever the text happens to look like.
const DecoderB64Text ObjectIDDecoder = "b64text"

// MaxLabelTextBytes bounds the UTF-8 length of a display name carried in a
// label object id.
//
// It is a presentation bound first and a storage bound second, and both
// matter. SpiceDB caps an object id at 1024 characters, and unpadded base64url
// expands by 4/3 — so 256 bytes of name encodes to 342 characters and can
// never be the reason a write is rejected. A console row is also not a
// document: a directory whose group is named by a paragraph gets the first 256
// bytes of it and an ellipsis, which is strictly more useful than the opaque
// id it replaced and strictly less disruptive than a list where one row is
// mostly one name.
const MaxLabelTextBytes = 256

// SanitizeLabelText reduces a directory-authored display name to text that is
// safe to store as an object id's payload and safe to render in a console.
//
// The input is hostile by assumption. A Slack channel name and a 1Password
// group's displayName are typed by whoever can create one upstream, they land
// in SpiceDB, and they come back out into a browser and into operator logs.
// Three things are removed, and the choice is to DROP rather than to escape,
// because escaped text still has to be un-escaped by every surface that
// renders it and one of them will eventually forget:
//
//   - Invalid UTF-8 derives nothing at all. Salvaging it would mint
//     replacement characters that render as a name the directory never had.
//   - Every non-graphic rune — Unicode categories Cc (C0/C1 controls, newline
//     and tab among them) and Cf (format) — is dropped. Cf is the one that
//     matters most and the one an ad-hoc "strip control characters" pass
//     always misses: the bidi overrides U+202A-U+202E / U+2066-U+2069 reorder
//     the text AROUND them, so a name can be made to read as a different name
//     entirely, and the zero-width characters make two distinct rows render
//     identically.
//   - Length beyond MaxLabelTextBytes, truncated on a RUNE boundary (never
//     mid-sequence, which would produce exactly the invalid UTF-8 the first
//     rule rejects) and marked with an ellipsis, so a shortened name does not
//     present itself as a whole one.
//
// The empty result is this package's usual "fall back to the raw id" signal: a
// name that is entirely non-graphic has nothing left to render, and a blank
// title would be worse than the id it replaced.
//
// Idempotent — sanitizing already-sanitized text returns it unchanged — which
// is what lets it run at BOTH ends, on the way into SpiceDB and again on the
// way back out. Sanitizing only on write would trust every row already stored,
// including rows written by an older build of this code.
func SanitizeLabelText(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if unicode.IsGraphic(r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) <= MaxLabelTextBytes {
		return out
	}

	// Walk rune starts and keep the last one that still leaves room for the
	// ellipsis. range over a string yields byte offsets of rune starts, which
	// is what makes this a rune-boundary cut without decoding anything twice.
	const ellipsis = "…"
	budget := MaxLabelTextBytes - len(ellipsis)
	cut := 0
	for i := range out {
		if i > budget {
			break
		}
		cut = i
	}
	return strings.TrimSpace(out[:cut]) + ellipsis
}

// EncodeB64Text renders a display name as the object id DecoderB64Text reads
// back: unpadded base64url of the SANITIZED text.
//
// Encoded rather than stored verbatim because a SpiceDB object id admits only
// a restricted alphabet, and a directory's display name admits spaces, accents
// and emoji. base64url's alphabet is a strict subset of what an object id
// allows, so any name that survives SanitizeLabelText encodes to a legal id —
// which is what makes "the write was rejected" unreachable by naming a channel
// badly. It is also the encoding this repo already mints object ids with (see
// DecodeB64URL), so there is one encoding here, not two.
//
// ok is false when the name has nothing renderable left after sanitizing. A
// caller must not write a label tuple in that case: an id that decodes to an
// empty title is a row rendered blank, and the raw id it would have replaced
// is more useful than that.
func EncodeB64Text(name string) (objectID string, ok bool) {
	clean := SanitizeLabelText(name)
	if clean == "" {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString([]byte(clean)), true
}
