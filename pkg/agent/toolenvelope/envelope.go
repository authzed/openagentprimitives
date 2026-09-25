// Package toolenvelope defines the untrusted-tool-output envelope: the marked
// region the runner wraps every tool result in before the model sees it.
//
// It lives in its own package because the format has two consumers that must
// never disagree — the runner writes it, and steelthread capture reads it back
// out of session memory (the persisted tool_result carries the envelope, not
// the bare payload). A second copy of the tag would drift, and the symptom
// would be a captured bundle whose tool outputs carry a stray tag the model
// never actually saw.
package toolenvelope

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// NewNonce returns a fresh 16-hex-character nonce (8 crypto-random bytes). A
// caller wrapping content MUST generate the nonce AFTER the content is fixed —
// that ordering is what makes the boundary unforgeable, since the content cannot
// contain a value chosen later. It panics on crypto/rand failure: a predictable
// nonce silently defeats the boundary, which is worse than a crash.
func NewNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("toolenvelope: crypto/rand unavailable — cannot generate a safe nonce: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Tag is the element name. The nonce, not the name, is what makes the region
// unforgeable by tool output.
const Tag = "untrusted-tool-output"

// PtTag is the element name for a tool result that ALSO carries a provenance
// (pt-tag) id. It is the same nonce'd envelope as Tag — untrusted-data framing
// with an unforgeable boundary — plus an `id` attribute. One wrapper does both
// jobs, so a tagged result is never double-wrapped, and the same nonce that
// stops tool output escaping the untrusted region also stops it forging a
// provenance boundary (relabelling narrow data under a wide id).
const PtTag = "pt-untrusted"

// Wrap marks content as untrusted, using the caller's nonce.
func Wrap(content, nonce string) string {
	return open(nonce) + "\n" + content + "\n" + close_(nonce)
}

// WrapPt marks content as untrusted AND carries its provenance id, under one
// nonce. The nonce must be freshly chosen at wrap time (after content is fixed)
// so no byte of content can predict or match it — that is what makes the
// boundary, and therefore the id binding, unforgeable.
func WrapPt(content, nonce, id string) string {
	return openPt(nonce, id) + "\n" + content + "\n" + close_pt(nonce)
}

// PtRegion is one well-formed, nonce-matched pt-untrusted region: the id it
// claims and the exact content it wraps. The content is returned so a caller can
// bind it — verify the bytes against what the platform stored for that id, since
// a matched nonce alone proves only the boundary, not that these bytes belong
// under this id.
type PtRegion struct {
	ID      string
	Content string
}

// PtRegions returns the well-formed, NONCE-MATCHED pt-untrusted regions covering
// a payload (id + content each), and whether the whole payload (ignoring
// whitespace between regions) is covered by such regions.
//
// Nonce-matched is the whole point: an open marker's content is parsed as a
// single region up to the close marker carrying THE SAME nonce, so content that
// embeds a forged `</pt-untrusted nonce="GUESS">…<pt-untrusted nonce="GUESS" …>`
// cannot split the region or relabel part of it — the guess never matches the
// real nonce (chosen after the content was fixed). A model reusing a datum
// copies the whole real region verbatim, nonce included, so its provenance
// survives here unforgeably.
//
// The nonce does NOT prove the id belongs to the content — the model authors the
// reply and can pair any witnessed id with any content. The caller MUST verify
// each region's content against the stored content for its id (content binding)
// before trusting the id. fullyCovered is false when any non-whitespace byte
// lies outside a matched region, or when there are no regions.
func PtRegions(payload string) (regions []PtRegion, fullyCovered bool) {
	openMarker := "<" + PtTag + " nonce=\""
	i := 0
	covered := true
	for i < len(payload) {
		j := strings.Index(payload[i:], openMarker)
		if j < 0 {
			if strings.TrimSpace(payload[i:]) != "" {
				covered = false
			}
			break
		}
		if strings.TrimSpace(payload[i:i+j]) != "" {
			covered = false // non-whitespace before this region
		}
		rest := payload[i+j+len(openMarker):]
		q := strings.IndexByte(rest, '"')
		if q < 0 {
			covered = false
			break
		}
		nonce := rest[:q]
		afterNonce := rest[q+1:]
		const idAttr = ` id="`
		if !strings.HasPrefix(afterNonce, idAttr) {
			covered = false
			break
		}
		afterID := afterNonce[len(idAttr):]
		q2 := strings.IndexByte(afterID, '"')
		if q2 < 0 {
			covered = false
			break
		}
		id := afterID[:q2]
		afterClose := afterID[q2+1:]
		if !strings.HasPrefix(afterClose, ">") {
			covered = false
			break
		}
		content := afterClose[1:]
		closeMarker := close_pt(nonce)
		k := strings.Index(content, closeMarker)
		if k < 0 {
			covered = false // unmatched open
			break
		}
		regions = append(regions, PtRegion{ID: id, Content: strings.TrimSpace(content[:k])})
		// Advance past the close marker in absolute terms.
		consumed := len(payload[i:i+j]) + len(openMarker) + q + 1 + len(idAttr) + q2 + 1 + 1 + k + len(closeMarker)
		i += consumed
	}
	return regions, covered && len(regions) > 0
}

// Unwrap removes the envelope, reporting whether there was one.
//
// Returns the input unchanged with false when the content is not enveloped —
// which is a real case, not an error: a capture reads records written before
// this existed, and a caller must be able to tell "already bare" from "failed
// to parse" without either mangling the payload or refusing it.
//
// The OUTER tags are matched by a single shared nonce, exactly as the model is
// told to read them. Scanning for the first close tag would let a payload
// containing a forged one truncate its own capture.
// StripPt removes the markers of every well-formed, nonce-matched pt-untrusted
// region from s, keeping the wrapped content, and reports whether any region was
// stripped. It is how pt markup is removed before content leaves the model-facing
// side — from tool arguments (so a tool never sees markup) and from a reply (so a
// human never does). A forged marker inside a region is content and stays as
// inert data; a malformed or unmatched marker is passed through unchanged.
func StripPt(s string) (string, bool) {
	openMarker := "<" + PtTag + " nonce=\""
	const idAttr = ` id="`
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	found := false
	for {
		j := strings.Index(s[i:], openMarker)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String(), found
		}
		absOpen := i + j
		after := s[absOpen+len(openMarker):]
		q := strings.IndexByte(after, '"')
		if q < 0 {
			b.WriteString(s[i:])
			return b.String(), found
		}
		nonce := after[:q]
		afterNonce := after[q+1:]
		if !strings.HasPrefix(afterNonce, idAttr) {
			b.WriteString(s[i : absOpen+len(openMarker)]) // not a real open; keep the literal
			i = absOpen + len(openMarker)
			continue
		}
		afterID := afterNonce[len(idAttr):]
		q2 := strings.IndexByte(afterID, '"')
		if q2 < 0 || !strings.HasPrefix(afterID[q2+1:], ">") {
			b.WriteString(s[i : absOpen+len(openMarker)])
			i = absOpen + len(openMarker)
			continue
		}
		contentAbs := absOpen + len(openMarker) + q + 1 + len(idAttr) + q2 + 1 + 1
		closeMarker := close_pt(nonce)
		k := strings.Index(s[contentAbs:], closeMarker)
		if k < 0 {
			b.WriteString(s[i:contentAbs]) // unmatched open: keep marker literal, continue past it
			i = contentAbs
			continue
		}
		b.WriteString(s[i:absOpen])                                    // text before the region
		b.WriteString(strings.TrimSpace(s[contentAbs : contentAbs+k])) // the content, WrapPt's framing newlines removed
		found = true
		i = contentAbs + k + len(closeMarker)
	}
}

func Unwrap(s string) (string, bool) {
	// An untrusted-tool-output envelope (no id).
	prefix := "<" + Tag + " nonce=\""
	if strings.HasPrefix(s, prefix) {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return s, false
		}
		head := s[:nl]
		if !strings.HasSuffix(head, "\">") {
			return s, false
		}
		nonce := head[len(prefix) : len(head)-len("\">")]
		tail := "\n" + close_(nonce)
		if !strings.HasSuffix(s, tail) {
			return s, false
		}
		return s[nl+1 : len(s)-len(tail)], true
	}
	// A pt-untrusted envelope: same shape plus an id attribute. Capture reads
	// tagged tool outputs back out of memory through here, so it must round-trip
	// both forms or a captured bundle's tool output would be mangled.
	ptPrefix := "<" + PtTag + " nonce=\""
	if strings.HasPrefix(s, ptPrefix) {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return s, false
		}
		head := s[:nl] // <pt-untrusted nonce="N" id="X">
		rest := head[len(ptPrefix):]
		q := strings.IndexByte(rest, '"')
		if q < 0 {
			return s, false
		}
		nonce := rest[:q]
		tail := "\n" + close_pt(nonce)
		if !strings.HasSuffix(s, tail) {
			return s, false
		}
		return s[nl+1 : len(s)-len(tail)], true
	}
	return s, false
}

func open(nonce string) string   { return "<" + Tag + " nonce=\"" + nonce + "\">" }
func close_(nonce string) string { return "</" + Tag + " nonce=\"" + nonce + "\">" }
func openPt(nonce, id string) string {
	return "<" + PtTag + " nonce=\"" + nonce + "\" id=\"" + id + "\">"
}
func close_pt(nonce string) string { return "</" + PtTag + " nonce=\"" + nonce + "\">" }
