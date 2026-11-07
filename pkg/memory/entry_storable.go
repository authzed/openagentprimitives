package memory

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// checkStorable refuses an entry carrying text a durable backend cannot hold.
// Postgres rejects each of these with a data-exception SQLSTATE — NUL in jsonb
// (22P05), an unpaired surrogate escape in jsonb (22P02), NUL or invalid UTF-8
// in a text column (22021) — and the refusal is about the entry, not the store,
// so a retry can never succeed. Checked at the facade rather than in the
// postgres backend so the same entry is refused on every backend, instead of
// being accepted by inmem/sqlite and failing only where it finally lands.
func checkStorable(e Entry) error {
	type text struct{ field, value string }
	texts := []text{
		{"Scope.Kind", e.Scope.Kind},
		{"Scope.ID", e.Scope.ID},
		{"Kind", e.Kind},
		{"ID", e.ID},
	}
	for i, t := range e.Tags {
		texts = append(texts, text{fmt.Sprintf("Tags[%d]", i), t})
	}
	for i, l := range e.Links {
		p := fmt.Sprintf("Links[%d].", i)
		texts = append(texts,
			text{p + "Relation", l.Relation},
			text{p + "Scope.Kind", l.Scope.Kind},
			text{p + "Scope.ID", l.Scope.ID},
			text{p + "Kind", l.Kind},
			text{p + "ID", l.ID},
		)
	}
	for _, t := range texts {
		if !utf8.ValidString(t.value) {
			return fmt.Errorf("field %s is not valid UTF-8", t.field)
		}
		if strings.IndexByte(t.value, 0) >= 0 {
			return fmt.Errorf("field %s contains a NUL character", t.field)
		}
	}
	return checkStorableJSON(e.Content)
}

// checkStorableJSON walks raw JSON's string escapes for the two a jsonb column
// refuses: \u0000, and a UTF-16 surrogate escape that is not part of a
// high+low pair. encoding/json accepts both as valid JSON — it checks only that
// an escape has four hex digits — so a RawMessage built by a client reaches the
// store with them intact. Escapes are walked rather than searched for, since
// `\\u0000` is an escaped backslash followed by the literal text "u0000".
func checkStorableJSON(raw []byte) error {
	if bytes.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("content contains a NUL byte")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		cp, ok := unicodeEscapeAt(raw, i)
		if !ok {
			i++ // skip the escaped character, so `\\` cannot start a second escape
			continue
		}
		switch {
		case cp == 0:
			return fmt.Errorf("content contains a NUL character (\\u0000)")
		case cp >= 0xDC00 && cp <= 0xDFFF:
			return fmt.Errorf("content contains an unpaired low surrogate (\\u%04x)", cp)
		case cp >= 0xD800 && cp <= 0xDBFF:
			lo, ok := unicodeEscapeAt(raw, i+6)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return fmt.Errorf("content contains an unpaired high surrogate (\\u%04x)", cp)
			}
			i += 6 // the low half is consumed with its high half
		}
		i += 5
	}
	return nil
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
