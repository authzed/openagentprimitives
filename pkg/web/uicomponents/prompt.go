// prompt.go is the placeholder grammar for an Action.Prompt: prose with {key}
// holes in it.
//
// Deliberately not the {"$param": …} object form the args templates use. An
// args template is a JSON document being filled in, where an object-shaped
// placeholder is unambiguous and nests naturally. A prompt is a SENTENCE —
// "Tell me about {name}", not a tree with a string in it.
package uicomponents

import (
	"slices"
	"strings"
)

// PromptRefs returns the sorted, deduplicated placeholder names in a prompt.
//
// A name is the text between a `{` and the next `}` on the same run, with no
// nesting and no escape form. That is the whole grammar, and it is small on
// purpose: every additional form is one this package, the validator, the
// browser's substitution, and the shared golden must carry forever.
//
// An unterminated `{` yields nothing, and braces around anything containing
// another `{` are not a placeholder. Both are treated as ordinary prose rather
// than as errors: a prompt is text a human wrote, `{` is a character that
// legitimately appears in text, and refusing a document over one would be a
// worse trade than leaving it in the sentence.
func PromptRefs(prompt string) []string {
	var out []string
	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '{' {
			continue
		}
		end := strings.IndexByte(prompt[i+1:], '}')
		if end < 0 {
			break // unterminated: the rest is prose
		}
		name := prompt[i+1 : i+1+end]
		i += end + 1
		if name == "" || strings.ContainsAny(name, "{") {
			continue
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// FillPrompt substitutes each {key} for its value.
//
// A key with no value is left as the literal placeholder rather than replaced
// with an empty string. "Tell me about {name}" reaching the agent unfilled is
// visibly broken and gets asked about; "Tell me about " is a grammatical
// sentence about nothing, which the agent will answer confidently and wrongly.
// Validation already rejects a prompt naming something nothing can supply, so
// reaching this state at all means a value was declared and then not sent —
// worth seeing, not worth papering over.
func FillPrompt(prompt string, values map[string]string) string {
	if prompt == "" || len(values) == 0 {
		return prompt
	}
	var b strings.Builder
	for i := 0; i < len(prompt); i++ {
		if prompt[i] != '{' {
			b.WriteByte(prompt[i])
			continue
		}
		end := strings.IndexByte(prompt[i+1:], '}')
		if end < 0 {
			b.WriteString(prompt[i:])
			break
		}
		name := prompt[i+1 : i+1+end]
		if v, ok := values[name]; ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString(prompt[i : i+1+end+1])
		}
		i += end + 1
	}
	return b.String()
}
