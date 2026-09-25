// Package csssanitize is the shared CSS string sanitizer, used by the html
// renderer (for <style> elements and style="..." attributes) and the css
// renderer (for standalone stylesheets). Sharing it is what keeps the two
// kinds' denylist semantics identical.
package csssanitize

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

// Sanitize is the SECOND layer of defense for agent-authored CSS. The PRIMARY
// boundary is the restrictive Content-Security-Policy the renderer injects
// (default-src 'none' + sandbox); this is defense-in-depth, removing the
// worst legacy script vectors so they are already gone should the CSP ever be
// loosened or bypassed.
//
// Design constraints:
//   - Untouched CSS is byte-identical. We do NOT re-serialize through the
//     parser — that reformats, breaking modern CSS like var(), custom
//     properties and clamp(). Instead we lex, track byte offsets, copy
//     unmodified spans verbatim, and splice out only a banned construct's
//     exact bytes.
//   - This is a DENYLIST, intentionally minimal and expected to GROW; a
//     property-level allowlist would break legitimate modern CSS.
//
// Denylist:
//  1. @import — pulls in a remote/external stylesheet.
//  2. url(...) whose target is neither a data: URI nor a #fragment. Everything
//     else goes: http/https/protocol-relative/javascript:/file:, relative
//     paths, and empty or unparsable BadURL forms.
//  3. expression(...) — legacy IE CSS-script.
//  4. behavior / -moz-binding properties — legacy script binding.
//
// KNOWN GAP: risky constructs hidden inside a custom-property *value*
// (--x: url(https://...)) are NOT scanned. This stylesheet-mode lexer emits a
// custom-property value as ordinary tokens rather than one opaque
// CustomPropertyValueToken, so we suspend the denylist between a
// CustomPropertyNameToken and the declaration's end and copy that span
// verbatim. Scanning inside would mean interpreting the value's grammar; the
// CSP backstops it.
func Sanitize(in string) (string, []channelassets.Warning) {
	if in == "" {
		return in, nil
	}

	s := &scanner{
		in:    in,
		lexer: css.NewLexer(parse.NewInput(bytes.NewReader([]byte(in)))),
	}

	var out strings.Builder
	out.Grow(len(in))

	// counts aggregates strips by construct name so we emit one Warning per
	// distinct construct with a total count.
	counts := map[string]int{}

	// prevSig is the last non-whitespace, non-comment token. It tells us whether
	// an ident sits at declaration start ({ / ; / } before it) — only there can
	// it be a PROPERTY rather than a selector. Its initial value is a never-read
	// dummy; prevSigSet gates every read, so atDeclStart stays false until the
	// first significant token is seen.
	prevSig := css.ErrorToken
	prevSigSet := false

	// inCustomProp tracks whether we are inside a custom-property declaration
	// (--foo: <value>), whose value the accepted gap leaves unscanned: every
	// token is emitted verbatim until the declaration ends. parenDepth tracks
	// nesting so a ';' inside parens does not end the declaration early.
	inCustomProp := false
	parenDepth := 0

	for {
		tt, sp, ok := s.next()
		if !ok {
			if err := s.lexer.Err(); err != nil && !errors.Is(err, io.EOF) {
				// Fail safe: do not drop CSS or panic on a lexer error. Keep the
				// remainder of the input as-is and surface a warning — the CSP
				// still applies as the primary boundary. (Repo rule: never
				// silently swallow errors.)
				out.WriteString(in[s.off:])
				return out.String(), append(warningsFrom(counts), channelassets.Warning{
					Kind:   "css",
					Name:   "unparsed",
					Action: "kept", // the tail was emitted as-is, NOT stripped
					Count:  1,
					Note:   "CSS could not be fully parsed; emitted as-is. CSP still applies as the primary boundary.",
				})
			}
			break
		}
		data := in[sp.start:sp.end]

		// Inside a custom-property value, emit everything verbatim and do not
		// apply the denylist (accepted gap, backstopped by the CSP). The value
		// ends at a ';' or '}' seen at the top paren level.
		if inCustomProp {
			switch tt {
			case css.FunctionToken, css.LeftParenthesisToken:
				parenDepth++
			case css.RightParenthesisToken:
				if parenDepth > 0 {
					parenDepth--
				}
			case css.SemicolonToken:
				if parenDepth == 0 {
					inCustomProp = false
				}
			case css.RightBraceToken:
				if parenDepth == 0 {
					inCustomProp = false
				}
			}
			out.WriteString(data)
			if tt != css.WhitespaceToken && tt != css.CommentToken {
				prevSig, prevSigSet = tt, true
			}
			continue
		}

		switch tt {
		case css.CustomPropertyNameToken:
			// Begin a custom-property declaration; its value is left verbatim.
			inCustomProp = true
			parenDepth = 0

		case css.AtKeywordToken:
			if strings.EqualFold(data, "@import") {
				// Remove @import through its terminating ';' (or up to '}' / end).
				s.skipThroughDeclEnd()
				counts["@import"]++
				prevSig, prevSigSet = css.SemicolonToken, true
				continue
			}

		case css.IdentToken:
			// behavior / -moz-binding are only banned in PROPERTY position: at
			// declaration start (prev significant token { / ; / }) AND
			// immediately followed (ignoring whitespace) by a ColonToken. A
			// selector like .behavior{} reaches here with prevSig == DelimToken
			// (the '.') and is left untouched.
			name := strings.ToLower(data)
			if (name == "behavior" || name == "-moz-binding") && atDeclStart(prevSig, prevSigSet) {
				if s.peekColonAcrossTrivia() {
					// Confirmed property: drop the ident and the rest of the
					// declaration through ';' (or up to '}' / end).
					s.skipThroughDeclEnd()
					counts[name]++
					prevSig, prevSigSet = css.SemicolonToken, true
					continue
				}
				// Not a property (no colon follows): fall through and emit the
				// ident verbatim. peekColonAcrossTrivia pushed back what it read.
			}

		case css.FunctionToken:
			if strings.EqualFold(data, "expression(") {
				// Remove the whole balanced call. The FunctionToken already
				// opened depth 1.
				s.skipBalanced()
				counts["expression()"]++
				prevSig, prevSigSet = css.RightParenthesisToken, true
				continue
			}

		case css.URLToken:
			// Unquoted/quoted url(...) arrives as ONE token. Strip unless the
			// inner target is a data: URI or a #fragment.
			if !urlTargetAllowed(data) {
				counts["url()"]++
				prevSig, prevSigSet = css.RightParenthesisToken, true
				continue
			}

		case css.BadURLToken:
			// Malformed url(...) (e.g. url(javascript:alert(1)) ). The closing
			// ')' is a SEPARATE RightParenthesisToken that follows; swallow it
			// too so the splice is clean.
			counts["url()"]++
			s.skipTrailingRightParen()
			prevSig, prevSigSet = css.RightParenthesisToken, true
			continue
		}

		// Default: keep these bytes verbatim.
		out.WriteString(data)

		if tt != css.WhitespaceToken && tt != css.CommentToken {
			prevSig, prevSigSet = tt, true
		}
	}

	return out.String(), warningsFrom(counts)
}

// span is the byte range [start,end) a token occupied in the original input.
type span struct{ start, end int }

// pushedTok is a token held in the pushback stack.
type pushedTok struct {
	tt css.TokenType
	sp span
	ok bool
}

// scanner wraps the css.Lexer with byte-offset tracking and a pushback stack.
// The lexer itself has no rewind; the pushback lets the property-position check
// peek across trivia for a ':' and put back everything it read when the ident
// turns out to be a selector rather than a property.
type scanner struct {
	in    string
	lexer *css.Lexer
	off   int // byte offset of the next byte not yet returned by next()

	pushed []pushedTok // LIFO; popped before pulling from the lexer
}

// next returns the next token type, its byte span in the input, and ok=false at
// end of input (or lexer error). It drains the pushback stack first.
func (s *scanner) next() (css.TokenType, span, bool) {
	if n := len(s.pushed); n > 0 {
		p := s.pushed[n-1]
		s.pushed = s.pushed[:n-1]
		return p.tt, p.sp, p.ok
	}
	tt, data := s.lexer.Next()
	if tt == css.ErrorToken {
		return tt, span{s.off, s.off}, false
	}
	sp := span{s.off, s.off + len(data)}
	s.off += len(data)
	return tt, sp, true
}

// push returns a token to the stack so a later next() re-yields it (LIFO).
func (s *scanner) push(tt css.TokenType, sp span, ok bool) {
	s.pushed = append(s.pushed, pushedTok{tt, sp, ok})
}

// peekColonAcrossTrivia reports whether the next significant token (ignoring
// whitespace/comments) is a ColonToken — i.e. whether the ident just seen is in
// property position. On true it consumes through the colon (the caller commits
// to stripping the whole declaration). On false it is non-consuming: every
// token it read (the trivia and the first significant token) is pushed back in
// original order so the main loop re-emits the ident's tail byte-identically.
func (s *scanner) peekColonAcrossTrivia() bool {
	var read []pushedTok
	for {
		tt, sp, ok := s.next()
		if tt == css.WhitespaceToken || tt == css.CommentToken {
			read = append(read, pushedTok{tt, sp, ok})
			continue
		}
		if ok && tt == css.ColonToken {
			return true // ident [trivia] ':' — a property; trivia+colon consumed.
		}
		// Not a colon: restore everything we pulled, in original order. Push the
		// significant token first, then trivia in reverse, so the LIFO stack
		// re-yields trivia-then-token.
		s.push(tt, sp, ok)
		for i := len(read) - 1; i >= 0; i-- {
			s.push(read[i].tt, read[i].sp, read[i].ok)
		}
		return false
	}
}

// skipThroughDeclEnd consumes (emitting nothing) up to and including the next
// ';', or up to but NOT including a '}' (the brace belongs to the block and is
// pushed back), or end of input.
func (s *scanner) skipThroughDeclEnd() {
	for {
		tt, sp, ok := s.next()
		if !ok {
			return
		}
		if tt == css.RightBraceToken {
			s.push(tt, sp, ok) // preserve the closing brace
			return
		}
		if tt == css.SemicolonToken {
			return
		}
	}
}

// skipBalanced consumes a balanced parenthesised tail (emitting nothing), given
// that the opening FunctionToken (depth 1) was already consumed.
func (s *scanner) skipBalanced() {
	depth := 1
	for depth > 0 {
		tt, _, ok := s.next()
		if !ok {
			return
		}
		switch tt {
		case css.FunctionToken, css.LeftParenthesisToken:
			depth++
		case css.RightParenthesisToken:
			depth--
		}
	}
}

// skipTrailingRightParen consumes a single ')' if it is the next token,
// emitting nothing; otherwise it pushes the token back. A BadURLToken does not
// include its closing paren, so we drop it here to keep the splice clean.
func (s *scanner) skipTrailingRightParen() {
	tt, sp, ok := s.next()
	if ok && tt == css.RightParenthesisToken {
		return
	}
	s.push(tt, sp, ok)
}

// atDeclStart reports whether the previous significant token places the next
// ident at the start of a declaration (where it could be a property name).
func atDeclStart(prevSig css.TokenType, set bool) bool {
	if !set {
		return false // start of input is a selector context, not a declaration
	}
	switch prevSig {
	case css.LeftBraceToken, css.SemicolonToken, css.RightBraceToken:
		return true
	}
	return false
}

// urlTargetAllowed reports whether a url(...) token's target is permitted: only
// data: URIs and #fragment references survive. Everything else (http, https,
// protocol-relative, javascript:, file:, relative paths) is stripped.
func urlTargetAllowed(token string) bool {
	// token is the whole "url(...)". Extract the inner target.
	inner := token
	if i := strings.IndexByte(inner, '('); i >= 0 {
		inner = inner[i+1:]
	}
	if j := strings.LastIndexByte(inner, ')'); j >= 0 {
		inner = inner[:j]
	}
	inner = strings.TrimSpace(inner)
	inner = trimMatchingQuotes(inner)
	inner = strings.TrimSpace(inner)

	lower := strings.ToLower(inner)
	if strings.HasPrefix(lower, "data:") {
		return true
	}
	if len(inner) > 0 && inner[0] == '#' {
		return true
	}
	return false
}

// trimMatchingQuotes removes a single surrounding pair of matching ' or " quotes.
func trimMatchingQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// warningsFrom turns the aggregated strip counts into Warnings (one per
// distinct construct). Order is not significant to callers.
func warningsFrom(counts map[string]int) []channelassets.Warning {
	if len(counts) == 0 {
		return nil
	}
	notes := map[string]string{
		"@import":      "external @import is removed; inline the styles instead",
		"url()":        "only url(data:...) and url(#fragment) are kept; embed assets as data: URIs",
		"expression()": "CSS expression() is legacy IE script and is removed",
		"behavior":     "behavior is legacy script binding and is removed",
		"-moz-binding": "-moz-binding is legacy script binding and is removed",
	}
	out := make([]channelassets.Warning, 0, len(counts))
	for name, n := range counts {
		out = append(out, channelassets.Warning{
			Kind:   "css",
			Name:   name,
			Action: "stripped",
			Count:  n,
			Note:   notes[name],
		})
	}
	// counts is a map, so this loop's order is randomized per call and two
	// sanitizations of identical bytes returned the same warnings in different
	// orders. The list reaches a model through artifact_prepare and a CRD
	// status, so the order is observable; see channelassets.SortWarnings.
	channelassets.SortWarnings(out)
	return out
}
