package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCheckStorableJSON pins the escape walk: the shapes jsonb refuses are
// caught, and the look-alikes it stores fine — an escaped backslash before
// literal "u0000", a correctly paired surrogate — are not.
func TestCheckStorableJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "plain text: storable", raw: `{"text":"hello"}`},
		{name: "empty content: storable", raw: ``},
		{name: "U+FFFD replacement char: storable", raw: `{"text":"a\ufffdb"}`},
		{name: "escaped backslash then literal u0000: storable", raw: `{"text":"C:\\u0000"}`},
		{name: "paired surrogate escape: storable", raw: `{"text":"\ud83d\ude00"}`},
		{name: "\\u0000 escape: refused as NUL", raw: `{"text":"a\u0000b"}`, wantErr: "NUL character"},
		{name: "\\u0000 after an escaped backslash: refused as NUL", raw: `{"text":"\\\u0000"}`, wantErr: "NUL character"},
		{name: "raw NUL byte: refused", raw: "{\"text\":\"a\x00b\"}", wantErr: "NUL byte"},
		{name: "lone high surrogate: refused", raw: `{"text":"\ud83dx"}`, wantErr: "unpaired high surrogate"},
		{name: "high surrogate at end of input: refused", raw: `"\ud83d`, wantErr: "unpaired high surrogate"},
		{name: "high followed by non-low escape: refused", raw: `{"text":"\ud83d\u0041"}`, wantErr: "unpaired high surrogate"},
		{name: "lone low surrogate: refused", raw: `{"text":"\ude00"}`, wantErr: "unpaired low surrogate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStorableJSON([]byte(tc.raw))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
