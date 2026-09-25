package html

import (
	"bytes"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

var _ channelassets.RefRewriter = Renderer{}

// artifactRefPrefix is the scheme this renderer's sanitizer leaves intact on
// img[src]/link[href] (see isArtifactURLValue) — the ONLY prefix RewriteRefs
// cares about. Everything after it, case preserved and including any "#tag"
// suffix, is the handle handed to resolve verbatim; this kind never interprets
// or strips a tag, which is entirely the caller's concern (the bundler's
// resolveBundleAsset in pkg/memory/httpsrv/bundle.go strips it for two of the
// three handle forms).
const artifactRefPrefix = "artifact:"

// RewriteRefs implements channelassets.RefRewriter for the html kind: it walks
// the document's img[src] and link[href] attributes — the only two contexts
// the sanitizer lets an artifact: value survive on, see stripDataNavURLs in
// renderer.go — and calls resolve(handle) for each artifact:HANDLE value.
// keep==true rewrites the attribute to replacement; keep==false drops the
// attribute alone, leaving the element and the rest of the document intact.
// resolve is memoized per distinct handle, required for callers whose resolve
// has side effects (the bundler fetches and records secondary bytes on first
// resolution).
//
// Content that fails to parse is returned unchanged with a nil resolved list,
// same as a document with no refs. That path is practically unreachable:
// x/net/html's Parse only errors on the underlying io.Reader, and content is
// always an in-memory byte slice here.
func (Renderer) RewriteRefs(content []byte, resolve func(handle string) (string, bool)) ([]byte, []string) {
	// No artifact: reference anywhere → return the input bytes UNCHANGED rather
	// than re-parse and re-serialize well-formed markup on every ref-less
	// serve, keeping the common case a true no-op as the contract requires.
	if !bytes.Contains(content, []byte("artifact:")) {
		return content, nil
	}
	doc, err := xhtml.Parse(bytes.NewReader(content))
	if err != nil {
		return content, nil
	}

	type cacheEntry struct {
		replacement string
		keep        bool
	}
	cache := map[string]cacheEntry{}
	var resolved []string
	cachedResolve := func(handle string) (string, bool) {
		if e, ok := cache[handle]; ok {
			return e.replacement, e.keep
		}
		replacement, keep := resolve(handle)
		cache[handle] = cacheEntry{replacement: replacement, keep: keep}
		if keep {
			resolved = append(resolved, handle)
		}
		return replacement, keep
	}

	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.DataAtom {
			case atom.Img:
				rewriteRefAttr(n, "src", cachedResolve)
			case atom.Link:
				rewriteRefAttr(n, "href", cachedResolve)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var buf bytes.Buffer
	if err := xhtml.Render(&buf, doc); err != nil {
		return content, nil
	}
	return buf.Bytes(), resolved
}

// rewriteRefAttr resolves the named attribute's artifact: value (if any) via
// resolve and either rewrites it to the replacement or drops the attribute
// entirely when resolve reports keep=false. Non-artifact: values (http(s),
// data:, …) are left untouched.
func rewriteRefAttr(n *xhtml.Node, key string, resolve func(string) (string, bool)) {
	for i, a := range n.Attr {
		if !strings.EqualFold(a.Key, key) {
			continue
		}
		handle, isRef := parseArtifactRef(a.Val)
		if !isRef {
			return
		}
		replacement, keep := resolve(handle)
		if !keep {
			n.Attr = append(n.Attr[:i], n.Attr[i+1:]...)
			return
		}
		n.Attr[i].Val = replacement
		return
	}
}

// parseArtifactRef reports whether v is an artifact: reference (tolerant of
// leading whitespace and scheme case, mirroring isArtifactURLValue) and, if
// so, returns everything after the scheme unchanged — the handle, optionally
// with a "#tag" suffix.
func parseArtifactRef(v string) (handle string, ok bool) {
	trimmed := strings.TrimLeft(v, " \t\r\n\f")
	if len(trimmed) <= len(artifactRefPrefix) || !strings.EqualFold(trimmed[:len(artifactRefPrefix)], artifactRefPrefix) {
		return "", false
	}
	return trimmed[len(artifactRefPrefix):], true
}
