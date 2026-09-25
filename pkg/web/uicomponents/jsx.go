package uicomponents

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// The markers a hook prints in place of, or ahead of, its content. read_view
// hands these to the agent, and plan 4's compiler treats each as a comment.
const (
	jsxEmptyMarker   = "{/* empty */}"
	jsxClearedMarker = "{/* empty: you cleared this hook */}"
	jsxYoursMarker   = "{/* your fill */}"
)

// hookPropOrder is the attribute order a hook prints its contract in — the
// order an author writes it and a reader expects it. Every other prop, on
// every node, prints sorted by name.
var hookPropOrder = []string{"name", "intent", "allowedComponents", "step", "title"}

// JSX prints a declaration's view as JSX in the platform's element
// vocabulary: the author's tree with every oap:generative hook in place and
// each hook's CURRENT content inside it. It is what read_view hands the
// agent — a page a model reads more reliably than the node grammar's nested
// JSON, and the same text an author writes for a bundle (the compiler is the
// inverse; see the spec's §9).
//
// agentComposed names the hooks whose content is the agent's own
// (View.AgentComposed); those are marked so the agent can tell its fills from
// the author's defaults, and a cleared one says it was cleared. A hook with no
// content prints an explicit empty marker rather than nothing, because an
// absent region and an empty one are different facts to a model deciding
// what to write next.
//
// The output is deterministic — props sorted by name (a hook's contract
// first, in hookPropOrder), bindings after props — and carries no bound
// VALUE: only the declaration, exactly as the JSON form does.
//
// Attribute grammar, which the compiler must accept exactly:
//   - a string containing none of  " \ < > & { }  and no control character or
//     line/paragraph separator prints as name="value";
//   - any other string prints as name={"<JSON string>"};
//   - every non-string JSON value prints as name={<compact JSON>};
//   - bindings print last, as one attribute bindings={<compact JSON object>};
//   - a node with no children is self-closing; a hook never is.
func JSX(d Declaration, agentComposed []string) string {
	if d.View == nil {
		return ""
	}
	var b strings.Builder
	writeJSXNode(&b, *d.View, 0, agentComposed)
	return strings.TrimSuffix(b.String(), "\n")
}

func writeJSXNode(b *strings.Builder, n Node, depth int, agentComposed []string) {
	indent := strings.Repeat("  ", depth)
	hook, isHook := hookOf(n)

	b.WriteString(indent + "<" + n.Component)
	writeJSXAttrs(b, n, isHook)
	if !isHook && len(n.Children) == 0 {
		b.WriteString(" />\n")
		return
	}
	b.WriteString(">\n")

	if isHook {
		yours := slices.Contains(agentComposed, hook.Name)
		inner := strings.Repeat("  ", depth+1)
		switch {
		case len(n.Children) == 0 && yours:
			b.WriteString(inner + jsxClearedMarker + "\n")
		case len(n.Children) == 0:
			b.WriteString(inner + jsxEmptyMarker + "\n")
		case yours:
			b.WriteString(inner + jsxYoursMarker + "\n")
		}
	}
	for _, ch := range n.Children {
		writeJSXNode(b, ch, depth+1, agentComposed)
	}
	b.WriteString(indent + "</" + n.Component + ">\n")
}

// writeJSXAttrs prints props (a hook's contract first, the rest sorted), then
// bindings as one JSON attribute.
func writeJSXAttrs(b *strings.Builder, n Node, isHook bool) {
	keys := make([]string, 0, len(n.Props))
	for k := range n.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if isHook {
		ordered := make([]string, 0, len(keys))
		for _, k := range hookPropOrder {
			if _, ok := n.Props[k]; ok {
				ordered = append(ordered, k)
			}
		}
		for _, k := range keys {
			if !slices.Contains(hookPropOrder, k) {
				ordered = append(ordered, k)
			}
		}
		keys = ordered
	}
	for _, k := range keys {
		b.WriteString(" " + k + "=" + jsxAttrValue(n.Props[k]))
	}
	if len(n.Bindings) > 0 {
		b.WriteString(" bindings={" + string(jsonNoHTMLEscape(n.Bindings)) + "}")
	}
}

// jsxAttrValue renders one prop. A string that needs no escaping prints as a
// plain quoted attribute; anything else prints as a braced JSON literal, which
// is also a valid JS literal — so the compiler's reader is one JSON parse.
func jsxAttrValue(raw json.RawMessage) string {
	// A JSON null unmarshals into a string as a no-op with no error, so it
	// must be recognised before the string attempt or it would print as "".
	if string(bytes.TrimSpace(raw)) == "null" {
		return "{null}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if !needsBracedString(s) {
			return `"` + s + `"`
		}
		return "{" + string(jsonNoHTMLEscape(s)) + "}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		// Props on a parsed node are well-formed JSON, so this is a RawMessage
		// built by hand. Printing it verbatim keeps the fault visible in the
		// output instead of hiding the prop.
		return "{" + string(raw) + "}"
	}
	return "{" + buf.String() + "}"
}

// needsBracedString answers whether a string must print as name={"<JSON
// string>"} rather than bare inside name="…".
//
// Two reasons, and the second is why this is wider than the JSX grammar
// strictly requires. The punctuation set is what would end the attribute or
// open a JSX expression. The rune predicate covers every character that has
// no legible form inside quotes — a raw C0 control, or a line/paragraph
// separator — which prints as an invisible or line-breaking byte the agent
// reads as noise and a reader cannot see at all. Sending those through the
// JSON encoder turns each into a visible \uXXXX escape instead. It subsumes
// the newline, CR and tab this used to name individually: unicode.IsControl
// reports true for all three.
func needsBracedString(s string) bool {
	return strings.ContainsAny(s, "\"\\<>&{}") ||
		strings.ContainsFunc(s, func(r rune) bool {
			return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
		})
}

// jsonNoHTMLEscape marshals v without the default HTML escaping, so `<`, `>`
// and `&` inside a string reach the agent as themselves. json.Encoder adds a
// trailing newline; it is trimmed. Marshalling a string or a map of Bindings
// cannot fail; the error branch keeps the printer total rather than trusted.
func jsonNoHTMLEscape(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
