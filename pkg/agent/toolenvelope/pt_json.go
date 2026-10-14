package toolenvelope

import (
	"bytes"
	"encoding/json"
)

// StripPtJSON removes pt-untrusted envelope markers from every string value in
// a JSON document, keeping the wrapped content, and reports whether anything
// was stripped. It is StripPt for structured payloads: tool arguments (so a
// tool never sees markup) and agent-authored components (so a human never
// does). Both consumers share this one walker so they cannot drift.
//
// The markup rides inside string VALUES (quotes escaped in the raw bytes), so
// it walks the decoded string leaves. Numbers are decoded with UseNumber so a
// large-integer sibling of a tagged field (a Snowflake id, a nanosecond
// timestamp) survives the round-trip intact. On any parse failure, or when
// nothing was stripped, it returns the ORIGINAL bytes unchanged.
func StripPtJSON(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw, false
	}
	had := false
	walked := walkStripPt(v, &had)
	if !had {
		return raw, false // nothing tagged; leave the exact bytes (no needless re-marshal)
	}
	out, err := json.Marshal(walked)
	if err != nil {
		return raw, false
	}
	return json.RawMessage(out), true
}

func walkStripPt(v any, had *bool) any {
	switch t := v.(type) {
	case string:
		s, found := StripPt(t)
		if found {
			*had = true
		}
		return s
	case []any:
		for i, e := range t {
			t[i] = walkStripPt(e, had)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = walkStripPt(e, had)
		}
		return t
	default:
		return v
	}
}
