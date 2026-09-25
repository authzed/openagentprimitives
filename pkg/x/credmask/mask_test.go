package credmask_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

func TestMask(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "GitHub PAT ghp_ prefix → prefix+****+last4",
			in:   "ghp_AbCdEfGhIjKlMnOp",
			want: "ghp_****MnOp",
		},
		{
			name: "github_pat_ prefix → prefix+****+last4",
			in:   "github_pat_11ABCDEF0123456789abcdef",
			want: "github_pat_****cdef",
		},
		{
			name: "sk_live_ prefix → prefix+****+last4",
			in:   "sk_live_abc123def456",
			want: "sk_live_****f456",
		},
		{
			name: "xoxb- prefix → prefix+****+last4",
			in:   "xoxb-1234567890-abcdefghij",
			want: "xoxb-****ghij",
		},
		{
			name: "mixed-case candidate prefix → rejected, ****+last4",
			in:   "aB3dE_fGhIjKlMnOpQrSt",
			want: "****QrSt",
		},
		{
			name: "no delimiter in first 12 chars → ****+last4",
			in:   "0123456789abcdef0123456789abcdef",
			want: "****cdef",
		},
		{
			name: "all-uppercase, no delimiter → ****+last4",
			in:   "AKIAIOSFODNN7EXAMPLE",
			want: "****MPLE",
		},
		{
			name: "len < 12 → ****",
			in:   "short_x",
			want: "****",
		},
		{
			name: "sk_live_ prefix but remainder < 8 → prefix rejected, ****+last4",
			in:   "sk_live_abcd",
			want: "****abcd",
		},
		{
			name: "empty string → ****",
			in:   "",
			want: "****",
		},
		{
			name: "newlines stripped before masking, no delimiter → ****+last4",
			in:   "with\nnewlines\nstripped",
			want: "****pped",
		},
		{
			name: "non-printable byte → ****",
			in:   "with\x00binary",
			want: "****",
		},
		// Byte-length boundary: 11 chars is still too short, 12 is the threshold.
		{
			name: "11 chars (below length threshold) → ****",
			in:   "abcdefghijk",
			want: "****",
		},
		{
			name: "12 chars (at length threshold) → ****+last4",
			in:   "abcdefghijkl",
			want: "****ijkl",
		},
		// Non-ASCII bytes are rejected entirely: byte-slicing on a non-ASCII string
		// can split a multi-byte rune, producing invalid UTF-8 in status fields.
		// Real credentials are ASCII; returning **** for anything non-ASCII is safe.
		{
			name: "non-ASCII byte anywhere → ****",
			in:   "abcdefghi\xc3\xa9xyz",
			want: "****",
		},
		{
			name: "non-ASCII byte in would-be vendor prefix → ****",
			in:   "pr\xc3\xa9_fixe_secret_value",
			want: "****",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credmask.Mask(tc.in), "Mask(%q)", tc.in)
		})
	}
}
