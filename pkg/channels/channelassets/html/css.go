package html

import (
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/internal/csssanitize"
)

// sanitizeCSSNodes runs the CSS denylist over every <style> element's text and
// every style="..." attribute in the parsed tree, rewriting them in place and
// aggregating the warnings. It is invoked from injectCSP, which already walks
// the parsed document.
func sanitizeCSSNodes(n *xhtml.Node) []channelassets.Warning {
	var ws []channelassets.Warning
	if n.Type == xhtml.ElementNode {
		if n.DataAtom == atom.Style {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == xhtml.TextNode {
					clean, w := csssanitize.Sanitize(c.Data)
					c.Data = clean
					ws = append(ws, w...)
				}
			}
		}
		for i, a := range n.Attr {
			if strings.EqualFold(a.Key, "style") {
				clean, w := csssanitize.Sanitize(a.Val)
				n.Attr[i].Val = clean
				ws = append(ws, w...)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		ws = append(ws, sanitizeCSSNodes(c)...)
	}
	return ws
}
