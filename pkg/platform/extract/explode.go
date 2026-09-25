package extract

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// An exploder is NOT an Extractor. Result{Text, Pages} cannot express N
// members, and forcing a container format through it would either flatten the
// archive into one blob — losing every member's own handle and MIME — or push
// callers into branching on "is this MIME really an archive?", the
// `if kind == "x"` shape this package exists to avoid.
//
// So containers get their own interface and their own MIME registry, and a new
// container format (.tar.gz, .7z) is a registration rather than a special case
// in a caller.

// ErrArchiveBomb is a decompression bound trip: uncompressed total, per-member
// size, ratio, or wall clock.
//
// PERMANENT. The same archive trips it again every time, so a caller must
// never word it as a temporary failure — "try sending it again" is advice that
// cannot succeed, and telling a user that is worse than telling them nothing.
var ErrArchiveBomb = errors.New("extract: archive exceeds a decompression bound")

// ErrArchiveMalformed is input that is not a readable archive of its claimed
// type. Also permanent, and deliberately distinct from ErrArchiveBomb: one
// means "we refused to open this", the other "there was nothing to open".
var ErrArchiveMalformed = errors.New("extract: not a readable archive")

// SkipReason is why one entry inside an archive was refused.
//
// A CLOSED vocabulary, and skips are reported as COUNTS PER REASON rather than
// as a list of names. A member name is file content, and this package runs in
// a pod deliberately given no way to send file content anywhere — a diagnostic
// naming the entries it rejected would be exactly that channel, reopened for
// convenience.
type SkipReason string

const (
	// SkipNonRegular is a directory, symlink, hardlink, device, or anything
	// else that is not a regular file. Skipped rather than followed: a symlink
	// inside an archive is a traversal primitive, not a file.
	SkipNonRegular SkipReason = "non_regular"
	// SkipTraversalName is a name that escapes the archive root.
	SkipTraversalName SkipReason = "traversal_name"
	// SkipOverMemberBytes is one member past MaxMemberBytes. The member is
	// dropped; the archive continues, because one oversized file in a bundle
	// does not make the other 200 unreadable.
	SkipOverMemberBytes SkipReason = "over_member_bytes"
	// SkipNameCollision is a second member normalizing to a name already
	// taken. Dropped rather than overwritten, so a hostile archive cannot hide
	// a file behind a benign one.
	SkipNameCollision SkipReason = "name_collision"
)

// Limits is the exploder's refusal policy.
//
// Every field is enforced WHILE STREAMING members. A bound checked after
// expansion has already lost: the bytes exist by then, and the process it
// would have protected is already the one that has to survive them.
type Limits struct {
	// MaxUncompressedTotal caps the whole archive's expanded size.
	MaxUncompressedTotal int64
	// MaxMemberBytes caps a single member.
	MaxMemberBytes int64
	// MaxMembers caps how many members are yielded. Reaching it TRUNCATES —
	// Summary.Truncated — rather than failing the archive: a partial support
	// bundle is still useful, provided the caller is told it is partial.
	MaxMembers int
	// MaxRatio caps expanded÷compressed, evaluated per member AND across the
	// archive, so neither one enormous member nor a thousand modest ones gets
	// through on the other's budget.
	MaxRatio int
	// MaxWallClock bounds the whole explode. It covers the bomb that stays
	// under every byte bound by decompressing slowly, which no size check can
	// see.
	MaxWallClock time.Duration
}

// DefaultLimits is the shipped policy, and the single place it is written
// down. Every field is non-zero on purpose: a zero bound reads as "unlimited"
// to any check of the form `n > lim`, which is the one way this struct could
// fail open.
var DefaultLimits = Limits{
	MaxUncompressedTotal: 64 << 20, // a 25 MiB support bundle compresses ~10x
	MaxMemberBytes:       25 << 20, // the ceiling a direct upload already gets
	MaxMembers:           256,
	MaxRatio:             100,
	MaxWallClock:         30 * time.Second, // matches tabula.DefaultTimeout
}

// Member is one entry in an exploded archive.
type Member struct {
	// Name is the sanitized, archive-relative path. Never absolute, never
	// containing "..", and still UNTRUSTED text: it reaches an agent's context
	// and must be treated as such by whoever renders it.
	Name string
	// Size is the uncompressed size actually produced, not the size the
	// archive's header claimed.
	Size int64
	// MIME is SNIFFED from the member's first bytes, never derived from Name.
	//
	// This is where the archive path differs from a direct upload, where the
	// channel supplies a MIME we have no better source for: here WE assign it,
	// so trusting an attacker-chosen extension would let a filename decide
	// which parser runs downstream.
	MIME string
	// Open returns the member's bytes. The caller owns closing it. Reading
	// past a limit returns ErrArchiveBomb mid-stream.
	Open func() (io.ReadCloser, error)
}

// Summary reports what a whole archive did.
type Summary struct {
	// Members is how many were yielded.
	Members int
	// Truncated is set when a bound stopped the walk early.
	Truncated bool
	// TruncatedReason names the bound, for a caller to state plainly.
	TruncatedReason string
	// TruncatedMember names the member whose COPY was cut, when the walk
	// stopped inside one rather than between them.
	//
	// Without it a member cut mid-copy is indistinguishable from a whole one:
	// its part is already on the wire, and the consumer stores it, runs text
	// extraction over it and lists it as complete. The archive-level Truncated
	// flag says SOMETHING was cut; this says WHICH, which is what a consumer
	// needs to mark that one row partial instead of guessing.
	//
	// Empty when the walk stopped BETWEEN members (the member-count bound), or
	// when nothing was truncated at all.
	TruncatedMember string
	// Skipped counts refused entries by reason. Counts, never names — see
	// SkipReason.
	Skipped map[SkipReason]int
}

// Exploder turns one container format into its members.
//
// Bound by every rule in this package's doc: no network, no filesystem beyond
// os.TempDir(), malformed input produces an error rather than a panic.
type Exploder interface {
	// MIMEs returns the exact MIME types this exploder claims. Registration
	// panics on a duplicate claim, so two backends cannot silently fight.
	MIMEs() []string

	// Explode calls yield once per surviving member, in archive order.
	//
	// Takes io.ReaderAt + size rather than io.Reader because a container
	// format needs random access to read its directory; a backend fed a stream
	// buffers to os.TempDir() and removes what it writes.
	//
	// Implementations MUST enforce lim while streaming, and MUST NOT recurse
	// into a member that is itself an archive — nesting depth is 0, so a
	// zip-of-zips cannot re-enter. An error returned by yield aborts the walk
	// and is returned unchanged.
	Explode(r io.ReaderAt, size int64, lim Limits, yield func(Member) error) (Summary, error)
}

var byExplodeMIME = map[string]Exploder{}

// RegisterExploder claims every MIME in e.MIMEs(). It panics on a duplicate
// claim, for the same reason Register does: two backends silently competing
// would make which one runs depend on package-import order.
//
// The registry is separate from the extractor registry rather than shared,
// because the two answer different questions about the same MIME — a container
// has an exploder and no extractor, and a caller must never get one where it
// asked for the other.
func RegisterExploder(e Exploder) {
	mu.Lock()
	defer mu.Unlock()
	for _, m := range e.MIMEs() {
		m = normalize(m)
		if prior, dup := byExplodeMIME[m]; dup {
			panic(fmt.Sprintf("extract: exploder %q claimed by both %T and %T", m, prior, e))
		}
		byExplodeMIME[m] = e
	}
}

// ExploderFor returns the exploder claiming mime. Parameters are ignored, so
// "application/zip; charset=binary" resolves to the "application/zip" backend.
func ExploderFor(mime string) (Exploder, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := byExplodeMIME[normalize(mime)]
	return e, ok
}

// ExplodableMIMEs lists every claimed container MIME, sorted. Used by
// extractord's readiness output and by tests asserting the registered set.
func ExplodableMIMEs() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(byExplodeMIME))
	for m := range byExplodeMIME {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// containerMIMEs is the vocabulary of CONTAINER types — formats whose bytes
// hold other files.
//
// It is declared here, in the package that owns the container concern, rather
// than in any consumer. A consumer naming these literals itself would be a
// second opinion this package could not override, and pkg/agent/runner's
// MIME-opinion guard rejects exactly that shape (its subject is native model
// support, but the reasoning is identical and the guard's own remedy is "put
// it behind a named constant in the package that owns that concern").
//
// Distinct from the exploder REGISTRY, and deliberately so. The registry
// answers "can this process open one?", which depends on which backends a
// binary linked; this answers "is this a container at all?", which does not.
// channelsd needs the second to decide a time budget without linking a zip
// parser it will never run. TestClaimedMIMEsAreContainerMIMEs keeps the two
// from drifting.
var containerMIMEs = map[string]bool{
	"application/zip":              true,
	"application/x-zip-compressed": true,
}

// IsContainerMIME reports whether mime names a container format. Parameters
// are ignored, matching For and ExploderFor.
func IsContainerMIME(mime string) bool { return containerMIMEs[normalize(mime)] }

// ContainerMIMEs lists the container vocabulary, sorted. For tests and
// readiness output.
func ContainerMIMEs() []string {
	out := make([]string, 0, len(containerMIMEs))
	for m := range containerMIMEs {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
