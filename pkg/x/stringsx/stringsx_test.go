package stringsx_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

func TestCapRunes(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{
			name:     "under bound returns unchanged",
			in:       "short",
			maxRunes: 10,
			want:     "short",
		},
		{
			name:     "exactly at bound returns unchanged",
			in:       "12345",
			maxRunes: 5,
			want:     "12345",
		},
		{
			name:     "over bound truncates and appends ellipsis",
			in:       "1234567890",
			maxRunes: 5,
			want:     "1234…",
		},
		{
			name:     "multi-byte runes counted as one each",
			in:       strings.Repeat("é", 10),
			maxRunes: 5,
			want:     strings.Repeat("é", 4) + "…",
		},
		{
			name:     "newlines preserved, not stripped",
			in:       "line one\nline two\nline three",
			maxRunes: 100,
			want:     "line one\nline two\nline three",
		},
		{
			name:     "maxRunes zero returns s unchanged",
			in:       "anything",
			maxRunes: 0,
			want:     "anything",
		},
		{
			name:     "maxRunes negative returns s unchanged",
			in:       "anything",
			maxRunes: -1,
			want:     "anything",
		},
		{
			name:     "empty string returns empty",
			in:       "",
			maxRunes: 5,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stringsx.CapRunes(tc.in, tc.maxRunes))
		})
	}
}
