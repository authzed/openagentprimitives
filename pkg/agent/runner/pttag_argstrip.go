package runner

import (
	"bytes"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
)

// stripArgTags removes pt-untrusted envelope markers from the args a TOOL will
// see. Tools do not know about pt markup; this is where it stops, between the
// gates and the tool's own Execute.
//
// Args are JSON and the markup rides inside string VALUES (quotes escaped in the
// raw bytes), so it walks the decoded string leaves. Numbers are decoded with
// UseNumber so a large-integer sibling of a tagged field (a Snowflake id, a
// nanosecond timestamp) survives the round-trip intact. On any parse failure, or
// when nothing was stripped, it returns the ORIGINAL bytes unchanged — the tool
// then sees exactly what the model sent, which is the safe fallback.
func stripArgTags(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return args
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return args
	}
	had := false
	walked := walkStripEnvelopes(v, &had)
	if !had {
		return args // nothing tagged; leave the exact bytes (no needless re-marshal)
	}
	out, err := json.Marshal(walked)
	if err != nil {
		return args
	}
	return json.RawMessage(out)
}

func walkStripEnvelopes(v any, had *bool) any {
	switch t := v.(type) {
	case string:
		s, found := toolenvelope.StripPt(t)
		if found {
			*had = true
		}
		return s
	case []any:
		for i, e := range t {
			t[i] = walkStripEnvelopes(e, had)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = walkStripEnvelopes(e, had)
		}
		return t
	default:
		return v
	}
}
