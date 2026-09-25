// Package viewurn owns the view-URN grammar: the stable string that names the
// browser/TUI surface a session-view inbound came from. One Parse/Format pair
// in the whole codebase (the pkg/channels/channelkey rule) — a second grammar would
// drift and a drifted URN is both a Slack markup-injection and an LLM
// prompt-injection surface, because a Via string is rendered into a Slack
// message and into the agent's context.
//
// Grammar: urn:ap:view:<type>:<nss>  where <type> is a registered kind and
// each "/"-joined segment of <nss> matches ^[a-z0-9][a-z0-9-]{0,63}$ . A
// single-id type (artifact) has an <nss> of <id> or <id>/<sub> (1-2
// segments); a pair-id type (session) has an <nss> of <ns>/<name> or
// <ns>/<name>/<sub> (2-3 segments) — see idShape. Types with no natural id
// (chat, tui) format as urn:ap:view:<type> with no trailing colon.
package viewurn

import (
	"fmt"
	"regexp"
	"strings"
)

const Prefix = "urn:ap:view:"

const (
	TypeArtifact = "artifact"
	TypeChat     = "chat"
	TypeTUI      = "tui"
	// TypeSession is a session-scoped view (the session-view page): its id is
	// a "<ns>/<name>" pair identifying the AgentSession, not a single opaque
	// id like TypeArtifact's.
	TypeSession = "session"
)

// SubAnnotations is the artifact-view sub-segment marking a view message as a
// batch of element annotations rather than a plain typed message. Encoding it in
// the Via rather than a separate turn field means the annotation identity rides
// the SAME string already digest-signed on the turn — verifiable in the audit
// chain, not an unsigned side channel. The id segment can never contain the sub,
// because Format validates id and sub as independent, slash-free segments.
const SubAnnotations = "annotations"

// SubWidget is the session-view sub-segment that marks a view message as an
// MCP-UI widget action (tool/prompt/link/intent/notify — @mcp-ui/client's
// UIActionResult) rather than a plain typed message. Same rationale as
// SubAnnotations: urn:ap:view:session:<ns>/<name>/widget IS a widget action —
// the provenance rides the signed Via itself (verifiable, audit-chained), not
// an unsigned side channel.
const SubWidget = "widget"

// idShape describes what a registered type's id looks like: none (bare type),
// a single opaque segment (artifact), or a "<ns>/<name>" pair (session — the
// two halves identify an AgentSession, not one opaque id).
type idShape int

const (
	shapeNone   idShape = iota // bare: no id, no sub (chat, tui)
	shapeSingle                // one opaque segment id (+ optional /sub)   (artifact)
	shapePair                  // "<ns>/<name>" id (+ optional /sub)         (session)
)

// registeredTypes: the id shape each type carries. A type absent here is
// rejected — the allowlist is the guard.
var registeredTypes = map[string]idShape{
	TypeArtifact: shapeSingle,
	TypeChat:     shapeNone,
	TypeTUI:      shapeNone,
	TypeSession:  shapePair,
}

// nssRe pins the id and sub segments for shapeSingle types: lowercase
// alphanumerics + dashes, each segment 1..64 chars, at most one "/"-joined
// sub. This is what keeps markup, spaces, and path traversal out of a Via.
var nssRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}(/[a-z0-9][a-z0-9-]{0,63})?$`)

// segRe validates a single segment (no "/" allowed): lowercase alphanumerics +
// dashes, 1..64 chars. Used by Format to validate id and sub independently.
var segRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// pairSegRe validates the "<ns>/<name>" id shape used by shapePair types
// (session): exactly two segRe-valid segments joined by one "/". Same
// per-segment character class as segRe/nssRe — widened only in segment
// count, not in what a segment may contain.
var pairSegRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}/[a-z0-9][a-z0-9-]{0,63}$`)

// pairNssRe pins the nss for shapePair types: the "<ns>/<name>" pair plus an
// optional third "/<sub>" segment. Anything beyond three segments, or any
// segment violating the lowercase/dash class, fails closed.
var pairNssRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}/[a-z0-9][a-z0-9-]{0,63}(/[a-z0-9][a-z0-9-]{0,63})?$`)

// URN is a parsed view URN.
type URN struct {
	Type string
	ID   string // "" for bare types (chat, tui)
	Sub  string // "" when no /sub segment
}

// Format builds a view URN, validating type + segments. sub may be "".
func Format(typ, id, sub string) (string, error) {
	shape, ok := registeredTypes[typ]
	if !ok {
		return "", fmt.Errorf("viewurn: unregistered type %q", typ)
	}
	switch shape {
	case shapeNone:
		if id != "" || sub != "" {
			return "", fmt.Errorf("viewurn: type %q takes no id/sub (got id=%q sub=%q)", typ, id, sub)
		}
		return Prefix + typ, nil
	case shapePair:
		if !pairSegRe.MatchString(id) {
			return "", fmt.Errorf("viewurn: id %q must be a <ns>/<name> pair", id)
		}
	default: // shapeSingle
		if id == "" {
			return "", fmt.Errorf("viewurn: type %q requires an id", typ)
		}
		if !segRe.MatchString(id) {
			return "", fmt.Errorf("viewurn: id %q violates grammar", id)
		}
	}
	nss := id
	if sub != "" {
		if !segRe.MatchString(sub) {
			return "", fmt.Errorf("viewurn: sub %q violates grammar", sub)
		}
		nss = id + "/" + sub
	}
	return Prefix + typ + ":" + nss, nil
}

// Parse validates and decomposes a view URN. Fail-closed: any deviation from
// the grammar is an error, never a partial/guessed result.
func Parse(s string) (URN, error) {
	if !strings.HasPrefix(s, Prefix) {
		return URN{}, fmt.Errorf("viewurn: missing prefix %q", Prefix)
	}
	rest := s[len(Prefix):]
	typ, nss, hasColon := strings.Cut(rest, ":")
	shape, ok := registeredTypes[typ]
	if !ok {
		return URN{}, fmt.Errorf("viewurn: unregistered type %q", typ)
	}
	if shape == shapeNone {
		if hasColon {
			return URN{}, fmt.Errorf("viewurn: bare type %q must not carry a value", typ)
		}
		return URN{Type: typ}, nil
	}
	if !hasColon || nss == "" {
		return URN{}, fmt.Errorf("viewurn: type %q requires an id", typ)
	}
	if shape == shapePair {
		if !pairNssRe.MatchString(nss) {
			return URN{}, fmt.Errorf("viewurn: id/sub %q violates grammar", nss)
		}
		// pairNssRe guarantees exactly 2 or 3 "/"-joined segments: the id is
		// the first two (the ns/name pair), the sub (if any) is the third.
		parts := strings.SplitN(nss, "/", 3)
		id := parts[0] + "/" + parts[1]
		sub := ""
		if len(parts) == 3 {
			sub = parts[2]
		}
		return URN{Type: typ, ID: id, Sub: sub}, nil
	}
	if !nssRe.MatchString(nss) {
		return URN{}, fmt.Errorf("viewurn: id/sub %q violates grammar", nss)
	}
	id, sub, _ := strings.Cut(nss, "/")
	return URN{Type: typ, ID: id, Sub: sub}, nil
}

// IsAnnotations reports whether via names an artifact-view annotation batch
// (urn:ap:view:artifact:<id>/annotations). Fail-closed: an unparseable/other Via
// is not an annotation batch. This is the structural, VERIFIABLE recognition the
// runner uses instead of scanning the turn text for a marker (which a plain
// message could spoof) — the Via is server-minted and digest-signed on the turn.
func IsAnnotations(via string) bool {
	u, err := Parse(via)
	return err == nil && u.Type == TypeArtifact && u.Sub == SubAnnotations
}

// Describe renders a human phrase for a view URN, for the agent's LLM context
// and Slack echoes. Returns "" for "" or anything that does not parse — it is
// called on stored data (already validated at write time) but stays robust.
func Describe(s string) string {
	u, err := Parse(s)
	if err != nil {
		return ""
	}
	switch u.Type {
	case TypeArtifact:
		switch {
		case u.Sub == SubAnnotations:
			return "annotations on the artifact view of " + u.ID
		case u.Sub != "":
			return "the artifact view of " + u.ID + "/" + u.Sub
		default:
			return "the artifact view of " + u.ID
		}
	case TypeSession:
		// The ns/name pair is deliberately NOT rendered: this phrase goes into a
		// Slack message and into the agent's LLM context, and neither surface
		// carries internal resource coordinates. The reader already knows which
		// session they are in.
		if u.Sub == SubWidget {
			return "a widget action in the session view"
		}
		return "the session view"
	case TypeChat:
		return "the web chat"
	case TypeTUI:
		return "the terminal"
	default:
		return ""
	}
}
