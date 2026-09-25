package apcmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestDurationSinceShort pins what every AGE column in the CLI renders. The
// sentinels are the interesting half: a CR with no creation timestamp and a
// clock that ran backwards both have to say so, because the alternative is an
// age column confidently reporting "56y" or a negative one.
func TestDurationSinceShort(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time says so instead of dating from the epoch", in: time.Time{}, want: "<unknown>"},
		{name: "future time says so instead of rendering negative", in: now.Add(time.Hour), want: "<now>"},
		{name: "seconds stay in seconds", in: now.Add(-42 * time.Second), want: "42s"},
		{name: "under an hour truncates to whole minutes", in: now.Add(-90 * time.Second), want: "1m"},
		{name: "under a day truncates to whole hours", in: now.Add(-3 * time.Hour), want: "3h"},
		{name: "a day or more truncates to whole days", in: now.Add(-49 * time.Hour), want: "2d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DurationSinceShort(tc.in))
		})
	}
}

// TestHumanBytes pins the presentation the CLI's size columns depend on: binary
// buckets, no space, no IEC "i", and a clamp rather than an unsigned wrap for a
// count that should never be negative but has no type stopping it.
func TestHumanBytes(t *testing.T) {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{name: "zero renders as an empty size, not a blank cell", in: 0, want: "0B"},
		{name: "negative clamps instead of wrapping to 16 EiB", in: -1, want: "0B"},
		{name: "single digit stays in bytes", in: 7, want: "7B"},
		{name: "sub-kibibyte stays in bytes", in: 512, want: "512B"},
		{name: "kibibytes carry one decimal", in: kib + kib/2, want: "1.5KB"},
		{name: "double-digit mebibytes drop the decimal", in: 20 * mib, want: "20MB"},
		{name: "gibibytes use the binary bucket, not the SI one", in: gib, want: "1.0GB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HumanBytes(tc.in))
		})
	}
}
