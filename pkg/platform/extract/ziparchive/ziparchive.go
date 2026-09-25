// Package ziparchive is the extract.Exploder for ZIP containers.
//
// It is the only place in the system where a hostile archive is interpreted,
// and it runs inside extractord: a pod with no egress, no credentials and no
// Kubernetes access. That placement is structural, not a risk judgement —
// stdlib archive/zip is far tamer than a document parser, but the operator is
// the most privileged process there is and untrusted bytes do not belong in
// it.
//
// Everything here is written against one assumption: the archive is
// adversarial. Header-declared sizes are hints, names are attacker-chosen, and
// entry types are whatever the author wrote. The only facts are the bytes that
// come out, counted as they come.
package ziparchive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// sniffBytes is how much of a member http.DetectContentType is shown. Its own
// contract is "considers at most the first 512 bytes".
const sniffBytes = 512

// Backend is the zip exploder.
type Backend struct{}

func init() { extract.RegisterExploder(Backend{}) }

// MIMEs claims both spellings browsers and chat transports actually send.
func (Backend) MIMEs() []string {
	return []string{"application/zip", "application/x-zip-compressed"}
}

// Explode walks the archive's central directory and yields regular members.
//
// The walk enforces lim as it copies, never against f.UncompressedSize64: that
// number is written by whoever built the archive and a bomb simply lies about
// it. Enforcing on it would be a check the attacker fills in.
func (Backend) Explode(r io.ReaderAt, size int64, lim extract.Limits, yield func(extract.Member) error) (extract.Summary, error) {
	sum := extract.Summary{Skipped: map[extract.SkipReason]int{}}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		// Deliberately does NOT wrap err: archive/zip's messages can quote
		// bytes from the input, and this pod must not emit file content.
		return sum, fmt.Errorf("%w", extract.ErrArchiveMalformed)
	}

	// The deadline covers everything below, including the walk itself. It is
	// also checked inside a member's Read, because a large member can blow it
	// mid-copy; both are needed, since a walk made entirely of SKIPPED entries
	// never reaches a Read and a single huge member never reaches the next
	// iteration.
	deadline := time.Now().Add(lim.MaxWallClock)

	// Cheap pre-filter on the DECLARED total. Usable to REFUSE and never to
	// accept: the value is written by whoever built the archive, so an
	// over-declaration is an honest bomb we can reject before producing a byte,
	// while an under-declaration is a lie the streaming bound below still
	// catches. Without it a single-member bomb is only caught after the member
	// is yielded, which downgrades a clean refusal into a partial response.
	declared := make([]uint64, 0, len(zr.File))
	for _, f := range zr.File {
		declared = append(declared, f.UncompressedSize64)
	}
	if declaredTotalExceeds(declared, lim.MaxUncompressedTotal) {
		return sum, fmt.Errorf("%w (declared uncompressed total)", extract.ErrArchiveBomb)
	}

	seen := make(map[string]bool, len(zr.File))
	var archiveTotal int64
	// trip is shared by every reader this walk hands out, so a bound blown by
	// one member is visible to the loop no matter which reader hit it.
	trip := &tripState{}

	for _, f := range zr.File {
		// Each skip is cheap — a name check and a map lookup, no decompression
		// — but a bound that does not cover the loop it is meant to bound is
		// not a bound.
		if time.Now().After(deadline) {
			return sum, fmt.Errorf("%w (wall clock)", extract.ErrArchiveBomb)
		}

		if lim.MaxMembers > 0 && sum.Members >= lim.MaxMembers {
			sum.Truncated = true
			sum.TruncatedReason = "member count"
			break
		}

		// Non-regular entries are SKIPPED, never followed. A symlink inside an
		// archive is a traversal primitive, not a file, and a directory entry
		// carries no bytes worth storing.
		if !f.Mode().IsRegular() {
			sum.Skipped[extract.SkipNonRegular]++
			continue
		}

		name, ok := safeName(f.Name)
		if !ok {
			sum.Skipped[extract.SkipTraversalName]++
			continue
		}
		// Dropped rather than overwritten: silently replacing a row lets a
		// hostile archive hide one file behind a benign one.
		if seen[name] {
			sum.Skipped[extract.SkipNameCollision]++
			continue
		}

		mimeType, sniffErr := sniff(f)
		if sniffErr != nil {
			// A member whose first bytes cannot be read is not worth failing
			// the archive over, and its cause is the same class as a member
			// that is simply corrupt.
			sum.Skipped[extract.SkipOverMemberBytes]++
			continue
		}

		member := extract.Member{
			Name: name,
			Size: int64(f.UncompressedSize64), // a hint, corrected as bytes are read
			MIME: mimeType,
		}

		// Clamped to the archive size: a member cannot hold more compressed
		// bytes than the file containing it, and archive/zip does not check the
		// declaration against reality.
		compressed := usableCompressedSize(f.CompressedSize64, size)
		zf := f
		// A FRESH reader per Open, sharing the archive total and the trip
		// state. One reader reused across calls would leak the previous
		// ReadCloser and carry its byte count forward, tripping the member
		// bound early on a legitimate re-read.
		//
		// A re-read is not a way around anything: the member counter restarts,
		// but every byte still counts once toward the ARCHIVE total, which is
		// the bound that makes repeated Opens converge on a refusal.
		member.Open = func() (io.ReadCloser, error) {
			rc, oerr := zf.Open()
			if oerr != nil {
				return nil, fmt.Errorf("%w", extract.ErrArchiveMalformed)
			}
			return &boundedReader{
				inner:      rc,
				lim:        lim,
				deadline:   deadline,
				compressed: compressed,
				archive:    &archiveTotal,
				trip:       trip,
			}, nil
		}

		seen[name] = true
		sum.Members++
		if yerr := yield(member); yerr != nil {
			return sum, yerr
		}
		if trip.err != nil {
			return sum, trip.err
		}
	}
	return sum, nil
}

// boundedReader enforces every byte bound INSIDE Read, which is what makes a
// bomb stop mid-copy rather than after it. A check performed once, before or
// after the copy, would let the bytes exist first — and the bytes existing is
// the whole attack.
type boundedReader struct {
	inner      io.ReadCloser
	lim        extract.Limits
	deadline   time.Time
	compressed int64
	archive    *int64
	member     int64
	trip       *tripState
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.trip.err != nil {
		return 0, b.trip.err
	}
	// The bomb that stays under every byte bound by decompressing slowly is
	// invisible to a size check, so time is bounded on the same path.
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return 0, b.blow("wall clock")
	}
	n, err := b.inner.Read(p)
	if n > 0 {
		b.member += int64(n)
		*b.archive += int64(n)
		if b.lim.MaxMemberBytes > 0 && b.member > b.lim.MaxMemberBytes {
			return n, b.blow("member size")
		}
		if b.lim.MaxUncompressedTotal > 0 && *b.archive > b.lim.MaxUncompressedTotal {
			return n, b.blow("uncompressed total")
		}
		// Ratio is meaningless for a member that compressed to nothing, so it
		// is only evaluated once there is a denominator.
		if b.lim.MaxRatio > 0 && b.member/b.compressed > int64(b.lim.MaxRatio) {
			return n, b.blow("compression ratio")
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%w", extract.ErrArchiveMalformed)
	}
	return n, err
}

func (b *boundedReader) blow(which string) error {
	// The reason names the BOUND, never the member: a member name is file
	// content and this error is logged.
	b.trip.err = fmt.Errorf("%w (%s)", extract.ErrArchiveBomb, which)
	return b.trip.err
}

func (b *boundedReader) Close() error {
	if b.inner == nil {
		return nil
	}
	return b.inner.Close()
}

// sniff reads a member's first bytes and types it from them.
//
// Opened separately from the member's own reader so a caller that never reads
// still gets a correct MIME, and so the sniff cannot consume bytes the caller
// expects to see. The read is capped at sniffBytes, so it is not a
// decompression bomb vector on its own.
func sniff(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()

	buf := make([]byte, sniffBytes)
	n, err := io.ReadFull(rc, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	detected := http.DetectContentType(buf[:n])

	// The extension breaks ties ONLY when sniffing declines to answer.
	// Anything stronger would hand a hostile filename the power to pick the
	// downstream parser, which is the thing sniffing exists to take away.
	if strings.HasPrefix(detected, "application/octet-stream") {
		if byExt := mime.TypeByExtension(path.Ext(f.Name)); byExt != "" {
			return byExt, nil
		}
	}
	return detected, nil
}

// safeName validates an archive-relative path and REJECTS anything that
// escapes. It never rewrites: a cleaned name disagrees with what the archive
// said it contained, and a reader comparing the two would be misled about
// which file they are looking at.
func safeName(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) || !utf8.ValidString(name) {
		return "", false
	}
	// Windows-authored archives use backslashes; normalizing the separator is
	// not the same as cleaning the path, and skipping it would let
	// `..\..\x` past a check that only understands "/".
	unified := strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(unified) {
		return "", false
	}
	// A drive letter is absolute on the platform that wrote it.
	if len(unified) >= 2 && unified[1] == ':' {
		return "", false
	}
	for _, seg := range strings.Split(unified, "/") {
		if seg == ".." {
			return "", false
		}
	}
	cleaned := path.Clean(unified)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	for _, r := range cleaned {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return cleaned, true
}

// declaredTotalExceeds reports whether the archive's DECLARED uncompressed
// sizes exceed the ceiling, accumulating with SATURATION.
//
// A plain `total += size` in uint64 is wrappable: an archive declaring sizes
// that sum past 2^64 lands on a small number and slips under the ceiling.
// The streaming bound still catches such an archive, so the consequence was a
// downgrade from a clean refusal to a partial response rather than a bypass —
// but a pre-filter an attacker can arrange to skip is not worth having.
func declaredTotalExceeds(declared []uint64, ceiling int64) bool {
	if ceiling <= 0 {
		return false
	}
	limit := uint64(ceiling)
	var total uint64
	for _, d := range declared {
		if d > limit-total {
			// Would exceed the ceiling, or would have wrapped past it.
			return true
		}
		total += d
	}
	return false
}

// tripState is the bound-blown flag, shared by every reader one walk hands
// out. Held apart from boundedReader because Open returns a fresh reader per
// call: a per-reader flag would be invisible to the loop when the caller blew
// a bound on its second read.
type tripState struct{ err error }

// usableCompressedSize converts a declared compressed size to the denominator
// of the ratio bound, FAILING CLOSED when the value is unusable and CLAMPING it
// to what the container can actually hold.
//
// Two separate problems, both of which let the archive's author decide how hard
// the ratio bound bites:
//
//   - A CompressedSize64 above MaxInt64 wraps negative once cast, and a
//     negative denominator would make the ratio check quietly skip that member.
//     A denominator of 1 is the strictest reading, so the bound trips at
//     MaxRatio bytes instead. archive/zip rejects such a header as malformed
//     before Explode sees it, so this arm is unreachable today and stays for a
//     future reader that is less strict; there is no test for it because a
//     fixture that could exercise it cannot be built — one that tried passed
//     for the wrong reason, failing as "not a readable archive".
//
//   - An OVER-declaration is reachable, because archive/zip does not check the
//     declared compressed size against the bytes actually present. Declaring a
//     gigabyte for a tiny member drives the denominator up and the ratio never
//     trips — the bound switched off by writing a number. A member's compressed
//     bytes cannot exceed the size of the ARCHIVE containing them, so the
//     declaration is clamped to that: a fact about the container rather than a
//     claim by its author.
//
// An UNDER-declaration is deliberately not corrected: it only tightens the
// ratio, which refuses sooner, and the absolute per-member and total bounds
// remain the real defense either way. archiveSize <= 0 means the caller could
// not report one and no clamp is applied — clamping to zero would make every
// denominator 1 and refuse every archive.
func usableCompressedSize(declared uint64, archiveSize int64) int64 {
	if declared == 0 || declared > uint64(math.MaxInt64) {
		return 1
	}
	n := int64(declared)
	if archiveSize > 0 && n > archiveSize {
		return archiveSize
	}
	return n
}
