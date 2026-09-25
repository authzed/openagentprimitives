package validator

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContainsEnum(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		target string
		want   bool
	}{
		{"empty", ``, "irreversible", false},
		{"string match", `"irreversible"`, "irreversible", true},
		{"string nomatch", `"benign"`, "irreversible", false},
		{"array match", `["benign","irreversible"]`, "irreversible", true},
		{"array nomatch", `["benign"]`, "irreversible", false},
		{"null", `null`, "irreversible", false},
		{"junk", `{"foo":1}`, "irreversible", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ContainsEnum(json.RawMessage(c.raw), c.target)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestContainsEnumField(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		field  string
		target string
		want   bool
	}{
		{"empty", ``, "outcomes", "irreversible", false},
		{"present string", `{"outcomes":"irreversible"}`, "outcomes", "irreversible", true},
		{"present array", `{"destination":["public","internal"]}`, "destination", "public", true},
		{"absent field", `{"sensitivity":["user"]}`, "outcomes", "irreversible", false},
		{"malformed object", `"not an object"`, "outcomes", "irreversible", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ContainsEnumField(json.RawMessage(c.raw), c.field, c.target)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestContainsEnumFieldChecked verifies the parse-error-aware variant
// distinguishes "field absent / no match" (parseOK=true) from "the
// metadata blob is not a JSON object" (parseOK=false) — the latter must
// fail CLOSED at the caller rather than be silently treated as no-match.
func TestContainsEnumFieldChecked(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		field       string
		target      string
		wantMatch   bool
		wantParseOK bool
	}{
		{"empty is parseable no-match", ``, "outcomes", "irreversible", false, true},
		{"present string matches", `{"outcomes":"irreversible"}`, "outcomes", "irreversible", true, true},
		{"present array matches", `{"destination":["public","internal"]}`, "destination", "public", true, true},
		{"absent field no-match parseable", `{"sensitivity":["user"]}`, "outcomes", "irreversible", false, true},
		{"non-object blob is unparseable", `"not an object"`, "outcomes", "irreversible", false, false},
		{"truncated json is unparseable", `{"outcomes":`, "outcomes", "irreversible", false, false},
		{"array-at-top is unparseable", `["irreversible"]`, "outcomes", "irreversible", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, parseOK := ContainsEnumFieldChecked(json.RawMessage(c.raw), c.field, c.target)
			assert.Equal(t, c.wantMatch, got, "match")
			assert.Equal(t, c.wantParseOK, parseOK, "parseOK")
		})
	}
}
