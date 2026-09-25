package httpsrv

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// ExplodedMember is one entry recovered from an inbound archive.
//
// Body is a LIVE reader positioned at the member's bytes, valid only for the
// duration of the yield callback that received it. It is a reader and not a
// []byte on purpose: an archive may expand to tens of megabytes across
// hundreds of members, and buffering them would put the whole expansion in the
// operator's heap — the cost the streaming contract exists to avoid.
type ExplodedMember struct {
	// Name is the sanitized, archive-relative path. Still untrusted text.
	Name string
	// MIME was SNIFFED from the member's bytes by the exploder, never derived
	// from Name.
	MIME string
	// Size is the uncompressed byte count the exploder produced.
	Size int64
	// IsArchive marks a member that is itself a container. It was stored and
	// NEVER opened — nesting depth is 0 — and the index must say so rather
	// than leaving a handle that looks readable.
	IsArchive bool

	// Body is the member's bytes. Valid only inside the yield callback.
	Body io.Reader
}

// ExplodeSummary reports what a whole archive did.
type ExplodeSummary struct {
	// Members is how many were yielded.
	Members int
	// Truncated is set when a bound stopped the walk early. A caller MUST
	// surface this: a partial archive presented as whole is the outcome the
	// streaming contract is most concerned with.
	Truncated bool
	// TruncatedReason names the bound, for a caller to state plainly.
	TruncatedReason string
	// TruncatedMember names the member whose copy was CUT, when the walk
	// stopped inside one rather than between them. Empty otherwise.
	//
	// Without it a half-file is indistinguishable from a whole one: it is
	// already on the wire, and the fan-out stores it, runs text extraction over
	// it and lists it as complete. See extract.Summary.TruncatedMember.
	TruncatedMember string
	// Skipped counts refused entries by reason. Counts, never names — a member
	// name is file content.
	Skipped map[string]int
}

// AttachmentExploder turns an inbound archive into its members by calling
// extractord. Optional, injected by internal/cmd/operator. Nil means "not
// configured": the inbound-asset route still stores the archive's raw bytes
// but reports no members, which callers MUST treat as a transient failure —
// never as "this archive type is unsupported".
type AttachmentExploder interface {
	// Explode POSTs body to extractord and calls yield once per member, in
	// archive order, with a live reader. An error returned by yield aborts and
	// is returned unchanged.
	//
	// ErrArchiveMIMEUnsupported (415) and ErrArchiveRefused (413/422) are
	// PERMANENT. Every other error — 5xx, timeout, connection failure — is
	// TRANSIENT, and callers must not conflate the two: the notice wording for
	// each is the opposite of the other's.
	Explode(ctx context.Context, mime string, body io.Reader, lim extract.Limits, yield func(ExplodedMember) error) (ExplodeSummary, error)
}

// ErrArchiveMIMEUnsupported is returned when extractord reports 415: no
// registered exploder claims the MIME. PERMANENT.
var ErrArchiveMIMEUnsupported = errors.New("httpsrv: no exploder claims this archive MIME")

// ErrArchiveRefused is returned when extractord reports 413 or 422: the
// archive tripped a decompression bound, or is not a readable archive of its
// claimed type. PERMANENT, and deliberately distinct from
// ErrArchiveMIMEUnsupported — "we will not open this" and "nothing here claims
// to open this" lead a user to different remedies.
var ErrArchiveRefused = errors.New("httpsrv: archive refused")

// WithAttachmentExploder enables archive fan-out on the /inbound-asset route.
// Without it an archive is stored whole and reports no members.
//
// TYPED-NIL SAFETY: the natural DI shape for "fail-closed if unset" is
// `var c *explodeClient; if endpoint != "" { c = New(...) }`, and passing that
// nil POINTER yields a non-nil interface ({type, nil}), so a
// `h.attachmentExploder == nil` guard would be false and the first upload
// would panic. This constructor defends the seam by reflection rather than
// trusting every future caller to remember AGENTS.md's typed-nil rule —
// exactly as WithAttachmentExtractor does.
func WithAttachmentExploder(e AttachmentExploder) HandlerOption {
	return func(h *handler) {
		if e != nil {
			if v := reflect.ValueOf(e); v.Kind() == reflect.Ptr && v.IsNil() {
				e = nil
			}
		}
		h.attachmentExploder = e
	}
}
