package redact

import (
	"fmt"
	"slices"
	"strings"
)

// Descriptor describes a redacted value: what kind of thing it was and why it's sensitive.
type Descriptor struct {
	// Description is the human-readable label shown in place of the value.
	Description string `json:"description"`
	// Kind says which input surface the value was bound from.
	Kind string `json:"kind"` // "flag" | "env" | "positional"
	// Name is the flag / variable / slot the value came from.
	Name string `json:"name"`
}

// Redactor assigns stable IDs to redacted values and emits an ID→Descriptor map.
// A fresh Redactor is used per Decision.
type Redactor struct {
	nextID  int
	byValue map[string]string // value -> token
	byToken map[string]Descriptor
	byID    map[string]Descriptor
}

func New() *Redactor {
	return &Redactor{
		byValue: map[string]string{},
		byToken: map[string]Descriptor{},
		byID:    map[string]Descriptor{},
	}
}

// Redact records the sensitive value and returns the redaction token.
// Repeated calls with the same value return the same token.
func (r *Redactor) Redact(value string, d Descriptor) string {
	if tok, ok := r.byValue[value]; ok {
		return tok
	}
	r.nextID++
	id := fmt.Sprintf("%d", r.nextID)
	tok := fmt.Sprintf(`<redacted id=%q/>`, id)
	r.byValue[value] = tok
	r.byToken[tok] = d
	r.byID[id] = d
	return tok
}

// RegisterSensitive is like Redact but does not return a token — used when you want
// to prime the Redactor with a known sensitive value before scanning free-form text.
// Still assigns an ID on first registration; subsequent calls are no-ops.
//
// The empty string is never registered: RedactInString skips value=="",
// so a registration for "" can never be redacted, and recording it as a
// "sensitive" descriptor is at best noise and at worst a fail-open lie —
// a caller that stringified an unhandled (e.g. composite) value to ""
// would believe it had protected a secret it did not. Guard it here so
// every caller is fail-closed by default.
func (r *Redactor) RegisterSensitive(value string, d Descriptor) {
	if value == "" {
		return
	}
	r.Redact(value, d)
}

// RedactInString scans s for any registered sensitive value and replaces occurrences with tokens.
//
// Longest value first, which is load-bearing rather than tidy: when one
// registered secret is a prefix of another (a token prefix registered beside
// the full token, which registerLeaves produces routinely), substituting the
// shorter one first consumes the prefix and strands the remainder in cleartext.
// Iterating r.byValue directly made that outcome depend on Go's randomized map
// order, so the same input leaked on most runs and redacted cleanly on the rest.
func (r *Redactor) RedactInString(s string) string {
	values := make([]string, 0, len(r.byValue))
	for value := range r.byValue {
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b) // stable for equal-length values
	})
	for _, value := range values {
		s = strings.ReplaceAll(s, value, r.byValue[value])
	}
	return s
}

// Emit returns the ID → Descriptor map for inclusion in a Decision.
func (r *Redactor) Emit() map[string]Descriptor {
	out := make(map[string]Descriptor, len(r.byID))
	for id, d := range r.byID {
		out[id] = d
	}
	return out
}
