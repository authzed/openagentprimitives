package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// checkStorable refuses an entry carrying text a durable backend cannot hold.
// Postgres rejects each of these with a data-exception SQLSTATE — NUL in jsonb
// (22P05), an unpaired surrogate escape in jsonb (22P02), NUL or invalid UTF-8
// in a text or jsonb column (22021) — and the refusal is about the entry, not
// the store, so a retry can never succeed. Checked at the facade rather than in
// the postgres backend so the same entry is refused on every backend, instead
// of being accepted by inmem/sqlite and failing only where it finally lands.
//
// This runs on every Put, so a field's label is only built once the field has
// failed.
func checkStorable(e Entry) error {
	fixed := [...]struct{ field, value string }{
		{"Scope.Kind", e.Scope.Kind},
		{"Scope.ID", e.Scope.ID},
		{"Kind", e.Kind},
		{"ID", e.ID},
	}
	for _, f := range fixed {
		if p := textProblem(f.value); p != "" {
			return fmt.Errorf("field %s %s", f.field, p)
		}
	}
	for i, t := range e.Tags {
		if p := textProblem(t); p != "" {
			return fmt.Errorf("field Tags[%d] %s", i, p)
		}
	}
	for i, l := range e.Links {
		link := [...]struct{ field, value string }{
			{"Relation", l.Relation},
			{"Scope.Kind", l.Scope.Kind},
			{"Scope.ID", l.Scope.ID},
			{"Kind", l.Kind},
			{"ID", l.ID},
		}
		for _, f := range link {
			if p := textProblem(f.value); p != "" {
				return fmt.Errorf("field Links[%d].%s %s", i, f.field, p)
			}
		}
	}
	if pv := e.Provenance; pv != nil {
		// Sig is bytes and travels base64-encoded, so it cannot carry either.
		prov := [...]struct{ field, value string }{
			{"Publisher", pv.Publisher},
			{"KeyID", pv.KeyID},
			{"PrevHash", pv.PrevHash},
		}
		for _, f := range prov {
			if p := textProblem(f.value); p != "" {
				return fmt.Errorf("field Provenance.%s %s", f.field, p)
			}
		}
	}
	if !utf8.Valid(e.Content) {
		return fmt.Errorf("content is not valid UTF-8")
	}
	return checkStorableJSON(e.Content)
}

// textProblem says what makes s unstorable in a text column, or "" when
// nothing does.
func textProblem(s string) string {
	switch {
	case !utf8.ValidString(s):
		return "is not valid UTF-8"
	case strings.IndexByte(s, 0) >= 0:
		return "contains a NUL character"
	default:
		return ""
	}
}

// checkStorableJSON refuses raw JSON holding a string escape a jsonb column
// refuses, or a raw NUL byte.
func checkStorableJSON(raw []byte) error {
	if bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("content contains a NUL byte")
	}
	var err error
	unstorableEscapes(raw, func(_, _ int, problem string) bool {
		err = fmt.Errorf("content contains %s", problem)
		return false
	})
	return err
}

// StorableJSON returns raw with every string escape a durable backend refuses
// replaced by \ufffd, and how many it replaced. It is for a writer that would
// rather keep a record with a placeholder in it than lose the record: the
// transcript, where one such character in a model's reply or a user's message
// would otherwise fail the whole turn. A caller that replaces anything owes a
// log line saying so.
//
// It must run BEFORE the entry is signed — it builds the bytes that get
// recorded, it does not rewrite a record. raw is expected to be json.Marshal
// output, which never holds a raw NUL byte or invalid UTF-8 (Marshal escapes
// the first and replaces the second), so escapes are the only shape left.
// Returns raw itself when there is nothing to replace.
func StorableJSON(raw json.RawMessage) (json.RawMessage, int) {
	var out []byte
	replaced, last := 0, 0
	unstorableEscapes(raw, func(start, end int, _ string) bool {
		out = append(out, raw[last:start]...)
		out = append(out, `\ufffd`...)
		last = end
		replaced++
		return true
	})
	if replaced == 0 {
		return raw, 0
	}
	return append(out, raw[last:]...), replaced
}

// unstorableEscapes walks raw JSON's string escapes and calls visit with the
// byte range of each one a jsonb column refuses: \u0000, and a UTF-16
// surrogate escape that is not part of a high+low pair. encoding/json accepts
// both as valid JSON — it checks only that an escape has four hex digits — so a
// RawMessage built by a client reaches the store with them intact. Escapes are
// walked rather than searched for, since `\\u0000` is an escaped backslash
// followed by the literal text "u0000". visit returning false stops the walk.
func unstorableEscapes(raw []byte, visit func(start, end int, problem string) bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		cp, ok := unicodeEscapeAt(raw, i)
		if !ok {
			i++ // skip the escaped character, so `\\` cannot start a second escape
			continue
		}
		problem := ""
		switch {
		case cp == 0:
			problem = `a NUL character (\u0000)`
		case cp >= 0xDC00 && cp <= 0xDFFF:
			problem = fmt.Sprintf(`an unpaired low surrogate (\u%04x)`, cp)
		case cp >= 0xD800 && cp <= 0xDBFF:
			if lo, ok := unicodeEscapeAt(raw, i+6); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				i += 6 // the low half is consumed with its high half
			} else {
				problem = fmt.Sprintf(`an unpaired high surrogate (\u%04x)`, cp)
			}
		}
		if problem != "" && !visit(i, i+6, problem) {
			return
		}
		i += 5
	}
}

// unicodeEscapeAt parses a \uXXXX escape starting at raw[i], reporting false
// when raw[i:] is not one.
func unicodeEscapeAt(raw []byte, i int) (uint64, bool) {
	if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
		return 0, false
	}
	cp, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
	return cp, err == nil
}
