package provenance

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// The `<pt id="...">…</pt>` markup wraps each datum in an agent's context with
// the tag whose audience governs it.
//
// SCANNED, NOT MATCHED, and that is the fix for three of the four defects the
// prototype shipped rather than a stylistic preference:
//
//   - A regex cannot parse nesting, and nesting is the NORMAL shape here: a
//     derived tag is required to embed its redacted sources verbatim. The
//     prototype's non-greedy `.*?` closed the outer tag at the first `</pt>`
//     and stranded the rest of its content outside any tag, which then read as
//     a partially-untagged payload.
//   - Go's `.` excludes `\n` without `(?s)`, so every multi-line tool output —
//     which is most of them — matched nothing, and the check silently fell back
//     to session-wide. A scanner has no such mode.
//   - Reconstructing content from capture groups is what let the prototype's
//     replacement (`"$2"`, against a pattern with one group) delete the data it
//     documented itself as preserving. A scanner copies the bytes between
//     markers, so the content cannot go missing.
const (
	openPrefix = `<pt id="`
	openSuffix = `">`
	closeTag   = `</pt>`
)

// span is one parsed tag occurrence.
type span struct {
	id    TagID
	depth int
}

// ParseTags returns the TOP-LEVEL tag ids covering s, in order of appearance.
//
// Top-level, not every id: a derived tag embeds its sources, and its own
// `reader` already resolves their intersection through `derived_from` in
// SpiceDB. Returning the nested ids as if they were peers would ask the check
// to re-derive an intersection the schema computes, and the two could disagree.
//
// Malformed markup is an ERROR, never an empty list. "No tags" and "I could
// not tell" must not be the same answer: a caller that received nil for a
// broken payload would proceed as though it were plain text, and the choice to
// fall back to the coarse check would have been made by a parse bug rather
// than by anyone.
func ParseTags(s string) ([]TagID, error) {
	spans, err := scan(s)
	if err != nil {
		return nil, err
	}
	var out []TagID
	for _, sp := range spans {
		if sp.depth == 0 {
			out = append(out, sp.id)
		}
	}
	return out, nil
}

// StripTags removes the markup and returns the content it wrapped, at every
// nesting level.
func StripTags(s string) (string, error) {
	if _, err := scan(s); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if id, next, ok := readOpen(s, i); ok {
			_ = id
			i = next
			continue
		}
		if strings.HasPrefix(s[i:], closeTag) {
			i += len(closeTag)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), nil
}

// HasOnlyPTags reports whether every non-whitespace byte of s sits inside at
// least one tag.
//
// This is the fallback predicate, and it is the single most important line of
// the port. What made the prototype sound was never the prompt instructing the
// model to preserve tags — models do not reliably obey — it was this check:
// unless the payload is FULLY covered, the caller falls back to the
// conservative session-wide taint comparison.
//
// It answers false for anything it cannot vouch for, including malformed
// markup and an empty payload. The asymmetry is deliberate: a wrong `false`
// over-blocks, which is visible and recoverable; a wrong `true` skips the
// per-datum check on content nobody accounted for, which is silent.
func HasOnlyPTags(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	if _, err := scan(s); err != nil {
		return false
	}
	depth := 0
	i := 0
	for i < len(s) {
		if _, next, ok := readOpen(s, i); ok {
			depth++
			i = next
			continue
		}
		if strings.HasPrefix(s[i:], closeTag) {
			depth--
			i += len(closeTag)
			continue
		}
		r := rune(s[i])
		if depth == 0 && !unicode.IsSpace(r) {
			return false
		}
		i++
	}
	return true
}

// IntersectAudiences returns the readers present in EVERY set — the audience of
// a payload assembled from several tagged data.
//
// An intersection, which is the whole soundness argument. The prototype
// computed a UNION here while computing an intersection at tag creation, so
// concatenating a datum readable by A with one readable by B produced something
// deemed viewable by both. That is a laundering primitive, and it is exactly
// the operation an agent performs when it answers from two sources.
//
// No sets at all yields nobody, not everybody. "For all" over an empty
// collection is vacuously true in logic and catastrophically wrong here: it
// would make an uncovered payload world-readable at precisely the moment there
// was no evidence about it. Same trap as `.all()` over an empty relation in
// the schema, and answered the same way.
func IntersectAudiences(sets [][]string) []string {
	if len(sets) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, set := range sets {
		seen := make(map[string]struct{}, len(set))
		for _, r := range set {
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			counts[r]++
		}
	}
	var out []string
	for r, n := range counts {
		if n == len(sets) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// readOpen reports whether an open tag begins at s[i], returning its id and
// the index just past it.
func readOpen(s string, i int) (TagID, int, bool) {
	if !strings.HasPrefix(s[i:], openPrefix) {
		return "", 0, false
	}
	rest := s[i+len(openPrefix):]
	end := strings.Index(rest, openSuffix)
	if end < 0 {
		return "", 0, false
	}
	id := rest[:end]
	if id == "" || strings.ContainsAny(id, "<>\"") {
		return "", 0, false
	}
	return TagID(id), i + len(openPrefix) + end + len(openSuffix), true
}

// scan walks s once, validating that every open tag closes and no close tag is
// stray, and returns each occurrence with the depth it opened at.
func scan(s string) ([]span, error) {
	var spans []span
	var stack []TagID
	i := 0
	for i < len(s) {
		if id, next, ok := readOpen(s, i); ok {
			spans = append(spans, span{id: id, depth: len(stack)})
			stack = append(stack, id)
			i = next
			continue
		}
		if strings.HasPrefix(s[i:], closeTag) {
			if len(stack) == 0 {
				return nil, fmt.Errorf("provenance: markup has a closing </pt> with no open tag at byte %d", i)
			}
			stack = stack[:len(stack)-1]
			i += len(closeTag)
			continue
		}
		i++
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("provenance: markup leaves tag %q unclosed", stack[len(stack)-1])
	}
	return spans, nil
}
