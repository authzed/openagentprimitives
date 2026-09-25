package apcmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

// DurationSinceShort renders the age of t as a single-unit, column-friendly
// string ("42s", "7m", "3h", "12d") for the CLI's list tables. A zero time
// reads "<unknown>"; a time in the future reads "<now>".
func DurationSinceShort(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	if d < 0 {
		return "<now>"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// HumanBytes renders a byte count in binary units with the compact, spaceless
// suffixes the CLI's artifact and bundle listings use ("512B", "1.5KB",
// "3.2MB").
//
// The bucketing, rounding and unit choice are humanize.IBytes'; only the
// presentation is ours — IEC spells binary units "KiB", and a size column that
// has to stay narrow enough to sit beside a key and an age does not have room
// for the extra character or the separating space.
//
// A negative count is clamped rather than converted: uint64(-1) is 16 EiB, so
// an unguarded conversion would report a nonsense size instead of an obviously
// empty one.
func HumanBytes(b int64) string {
	if b <= 0 {
		return "0B"
	}
	s := humanize.IBytes(uint64(b))
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "iB", "B")
}
