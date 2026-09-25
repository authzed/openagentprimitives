// htmltotext.go reduces an HTML page's markup to plain text before fetch
// measures it against its size cap. Design R4 requires the reduction to
// happen BEFORE the cap is applied — otherwise markup consumes the byte
// budget the prose needs, and a page that is 90% tags returns almost no
// actual content.
//
// This walks golang.org/x/net/html's parse tree — an existing DIRECT
// dependency (go.mod), already used for HTML manipulation elsewhere in this
// exact codebase (pkg/channels/channelassets/html) — rather than a
// hand-rolled tag-stripping regex, the shape CLAUDE.md's "prefer a library
// over a hand-rolled utility" rule explicitly rejects.
//
// bluemonday (also already a dependency, and this repo's own choice for
// HTML sanitization) was the first candidate and was rejected for this
// specific job after a direct comparison: bluemonday.StrictPolicy().Sanitize
// strips every tag but inserts no whitespace at block boundaries, so
// "<p>A</p><p>B</p>" collapses to "AB" — two paragraphs glued into one
// unreadable word run. It also leaves HTML entities escaped ("&amp;" stays
// "&amp;"), because it sanitizes the token stream rather than parsing text
// nodes. Walking x/net/html's own tree costs a few more lines than a single
// Sanitize call, but keeps block-level elements from running into each
// other and gets entity decoding for free from the parser's own text nodes.
package main

import (
	"strings"

	"golang.org/x/net/html"
)

// blockElements insert a line break where they close, so unrelated prose
// blocks (paragraphs, list items, table rows, headings) don't run into each
// other as one unbroken word.
var blockElements = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "ul": true, "ol": true, "blockquote": true, "pre": true,
	"section": true, "article": true, "header": true, "footer": true, "hr": true,
}

// skipContentElements are dropped WITH their text content, not merely
// unwrapped. This MUST mirror bluemonday's own setOfElementsToSkipContent
// (see pkg/channels/channelassets/html/renderer.go's skipContentElements,
// which documents the same set for the same reason) — otherwise inline
// script/style/title text would reach the model disguised as page prose.
var skipContentElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "noframes": true,
	"noembed": true, "iframe": true, "frame": true, "frameset": true,
	"object": true, "title": true, "nostyle": true,
}

// htmlToText reduces body's markup to plain text, decoding entities and
// inserting a line break at each block element's close. html.Parse follows
// the HTML5 parsing algorithm, which defines error recovery for every byte
// sequence — it can only fail if the underlying io.Reader errors, which a
// strings.Reader never does — so this always returns something rather than
// an error a caller would have to handle.
func htmlToText(body string) string {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		// Defensive only (see doc comment above): fall back to the raw body
		// rather than losing it, so a fetch never fails outright on a page
		// this parser could not make sense of.
		return body
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipContentElements[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockElements[n.Data] {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	return collapseBlankLines(b.String())
}

// collapseBlankLines trims trailing whitespace from each line and collapses
// runs of two or more consecutive blank lines (adjacent empty block
// elements — a nested <ul><li> often produces several in a row) down to
// one, so the extracted text reads as paragraphs rather than a page mostly
// made of blank lines.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
