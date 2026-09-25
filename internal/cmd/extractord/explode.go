package main

import (
	"encoding/json"
	"errors"

	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// Member part headers. The operator reads these to reconstruct each member
// without parsing any of the bytes itself.
const (
	hdrMemberName = "X-Member-Name"
	hdrMemberSize = "X-Member-Size"
	hdrMemberKind = "X-Member-Kind"
	// hdrMemberArchive marks a member that is itself a container. extractord
	// holds the exploder registry, so it is the component that KNOWS; the
	// operator would otherwise have to keep a parallel list of container MIMEs
	// and drift from it.
	hdrMemberArchive = "X-Member-Archive"
)

// Limit headers. A caller MAY ask for tighter bounds than this pod's own;
// it can never ask for looser ones, because handleExplode re-clamps to
// extract.DefaultLimits. That re-clamp is the load-bearing half: extractord
// is the only place an archive is opened, so it must not inherit a ceiling
// from whoever called it.
const (
	hdrLimitUncompressedTotal = "X-Limit-Uncompressed-Total"
	hdrLimitMemberBytes       = "X-Limit-Member-Bytes"
	hdrLimitMembers           = "X-Limit-Members"
	hdrLimitRatio             = "X-Limit-Ratio"
	// memberKindSummary marks the trailing part carrying explodeSummary, so
	// the operator learns a bound was hit rather than inferring it from a part
	// count it has no expected value for.
	memberKindSummary = "summary"
)

// explodeSummary is the wire shape of the trailing summary part.
//
// Skips are counts per reason, never names: a member name is file content and
// this pod must not emit file content, which is the same rule
// classifyExtractError enforces for parser errors.
type explodeSummary struct {
	Members         int             `json:"members"`
	Truncated       bool            `json:"truncated,omitempty"`
	TruncatedReason string          `json:"truncatedReason,omitempty"`
	TruncatedMember string          `json:"truncatedMember,omitempty"`
	Skipped         []skippedReason `json:"skipped,omitempty"`
}

type skippedReason struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// handleExplode serves POST /explode. The Content-Type names the container
// MIME; the raw body is the archive; the response is multipart/mixed with one
// part per member and a trailing summary part.
//
// Status codes mirror /extract's permanent-vs-transient split, which the
// caller's notice wording depends on:
//
//   - 415: no exploder claims this MIME. Permanent.
//   - 413: the body exceeded maxBytes, or a decompression bound tripped
//     before any member was written. Permanent.
//   - 422: the bytes are not a readable archive of that type. Permanent.
//   - 5xx: extractord itself is unhealthy. Transient.
//
// STREAMING CAVEAT, and it is load-bearing: members are written as they are
// produced, so a bound tripped partway through arrives AFTER 200 and the
// headers are already gone. That case cannot become a 413 — instead the walk
// stops and the summary part reports truncated with its reason, so a partial
// response always says it is partial. Silently short multipart would be read
// as a complete archive, which is the one outcome worse than an error.
func handleExplode(maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mimeType := r.Header.Get("Content-Type")
		exploder, ok := extract.ExploderFor(mimeType)
		if !ok {
			slog.Info("extractord: unsupported archive MIME", "mime", mimeType)
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}

		// zip.NewReader needs io.ReaderAt + size, which a request body is not.
		// The package doc permits os.TempDir() for exactly this; what is
		// written here is removed on every path out.
		tmp, err := os.CreateTemp("", "extractord-archive-*")
		if err != nil {
			slog.Error("extractord: create temp file", "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer func() {
			name := tmp.Name()
			_ = tmp.Close()
			if rerr := os.Remove(name); rerr != nil && !os.IsNotExist(rerr) {
				slog.Error("extractord: remove temp archive", "err", rerr.Error())
			}
		}()

		body := &countingReader{r: http.MaxBytesReader(w, r.Body, maxBytes)}
		size, err := io.Copy(tmp, body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				slog.Info("extractord: archive too large", "mime", mimeType, "bytes", body.n)
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			// Never err.Error(): a read failure on untrusted bytes can quote
			// them, and this pod does not emit file content.
			slog.Info("extractord: reading archive body failed", "mime", mimeType, "bytes", body.n)
			http.Error(w, "could not read the uploaded archive", http.StatusInternalServerError)
			return
		}

		mw := multipart.NewWriter(w)
		wroteHeader := false
		// The member currently being streamed. A bound blown mid-copy stops the
		// walk inside THIS one, so it is the member whose part is already on the
		// wire but incomplete -- and a consumer that cannot tell stores it, runs
		// text extraction over it, and lists it as whole.
		var inFlight string
		sum, xerr := exploder.Explode(tmp, size, limitsFromHeaders(r.Header), func(m extract.Member) error {
			inFlight = m.Name
			if !wroteHeader {
				// Set before the first byte: after this the status is 200 and
				// no error can change it.
				w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
				w.WriteHeader(http.StatusOK)
				wroteHeader = true
			}
			return writeMemberPart(mw, m)
		})

		if !wroteHeader {
			// Nothing was written yet, so a real status is still available.
			respondExplodeError(w, mimeType, body.n, xerr)
			return
		}

		// Past this point only the summary can carry bad news.
		if xerr != nil {
			sum.Truncated = true
			if sum.TruncatedReason == "" {
				sum.TruncatedReason = explodeFailureReason(xerr)
			}
			// The walk stopped INSIDE a member, so name it: the archive-level
			// flag says something was cut, and this says which.
			sum.TruncatedMember = inFlight
			slog.Info("extractord: explode stopped after streaming began; reporting truncated",
				"mime", mimeType, "members", sum.Members, "reason", sum.TruncatedReason,
				"truncatedMember", sum.TruncatedMember)
		}
		if err := writeSummaryPart(mw, sum); err != nil {
			slog.Error("extractord: write summary part", "err", err.Error())
		}
		if err := mw.Close(); err != nil {
			slog.Error("extractord: close multipart writer", "err", err.Error())
		}
		slog.Info("extractord: exploded", "mime", mimeType, "bytes", body.n,
			"members", sum.Members, "truncated", sum.Truncated)
	}
}

func writeMemberPart(mw *multipart.Writer, m extract.Member) error {
	h := make(map[string][]string, 3)
	h["Content-Type"] = []string{m.MIME}
	h[hdrMemberName] = []string{m.Name}
	h[hdrMemberSize] = []string{strconv.FormatInt(m.Size, 10)}

	// Nesting depth is 0, so this member was stored and never opened. The
	// caller needs to SAY that in its index, and only this side can tell.
	if _, isArchive := extract.ExploderFor(m.MIME); isArchive {
		h[hdrMemberArchive] = []string{"1"}
	}

	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	rc, err := m.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(pw, rc)
	return err
}

func writeSummaryPart(mw *multipart.Writer, sum extract.Summary) error {
	out := explodeSummary{
		Members:         sum.Members,
		Truncated:       sum.Truncated,
		TruncatedReason: sum.TruncatedReason,
		TruncatedMember: sum.TruncatedMember,
	}
	for reason, count := range sum.Skipped {
		out.Skipped = append(out.Skipped, skippedReason{Reason: string(reason), Count: count})
	}
	// Sorted so the part is byte-stable for a given result; map order is not.
	sortSkipped(out.Skipped)

	pw, err := mw.CreatePart(map[string][]string{
		"Content-Type": {"application/json"},
		hdrMemberKind:  {memberKindSummary},
		hdrMemberName:  {""},
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(pw).Encode(out)
}

func sortSkipped(s []skippedReason) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Reason < s[j-1].Reason; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// respondExplodeError maps a pre-streaming failure onto the status contract.
// Content-free by the same rule as classifyExtractError.
func respondExplodeError(w http.ResponseWriter, mimeType string, read int64, err error) {
	if err == nil {
		// An archive with no members at all: a valid, empty container. 200
		// with only a summary part is the honest answer.
		http.Error(w, "archive contained no readable members", http.StatusUnprocessableEntity)
		slog.Info("extractord: archive had no readable members", "mime", mimeType, "bytes", read)
		return
	}
	status, kind, msg := classifyExplodeError(err)
	slog.Info("extractord: explode failed", "mime", mimeType, "bytes", read, "errKind", kind)
	http.Error(w, msg, status)
}

func classifyExplodeError(err error) (status int, errKind, msg string) {
	switch {
	case errors.Is(err, extract.ErrArchiveBomb):
		return http.StatusRequestEntityTooLarge, "bomb", "archive expands beyond what will be opened"
	case errors.Is(err, extract.ErrArchiveMalformed):
		return http.StatusUnprocessableEntity, "malformed", "not a readable archive"
	default:
		return http.StatusUnprocessableEntity, "explode_failed", "could not open this archive"
	}
}

func explodeFailureReason(err error) string {
	switch {
	case errors.Is(err, extract.ErrArchiveBomb):
		return "decompression bound"
	case errors.Is(err, extract.ErrArchiveMalformed):
		return "archive became unreadable"
	default:
		return "archive could not be fully opened"
	}
}

// limitsFromHeaders resolves the bounds for one request.
//
// Absent headers mean DefaultLimits. A present header TIGHTENS, and is clamped
// to DefaultLimits so a caller cannot raise this pod's ceiling — extractord is
// the only place an archive is opened, and a limit it accepted on trust would
// be no limit at all.
//
// A malformed value is IGNORED rather than fatal, and the pod's own default
// applies. The alternative — refusing the upload — would turn a config typo
// into a user-visible failure for a file that is perfectly readable under the
// default, and the value has already been validated once at apply time.
func limitsFromHeaders(h http.Header) extract.Limits {
	out := extract.DefaultLimits

	if v, ok := headerInt64(h, hdrLimitUncompressedTotal); ok && v < out.MaxUncompressedTotal {
		out.MaxUncompressedTotal = v
	}
	if v, ok := headerInt64(h, hdrLimitMemberBytes); ok && v < out.MaxMemberBytes {
		out.MaxMemberBytes = v
	}
	if v, ok := headerInt64(h, hdrLimitMembers); ok && int(v) < out.MaxMembers {
		out.MaxMembers = int(v)
	}
	if v, ok := headerInt64(h, hdrLimitRatio); ok && int(v) < out.MaxRatio {
		out.MaxRatio = int(v)
	}
	return out
}

// headerInt64 reads a positive integer header. A non-positive value is
// reported as absent: zero would otherwise mean "unlimited" to the bound
// checks, which is the exact inversion these headers exist to prevent.
func headerInt64(h http.Header, name string) (int64, bool) {
	raw := h.Get(name)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		slog.Info("extractord: ignoring unusable limit header; the pod default applies",
			"header", name)
		return 0, false
	}
	return v, true
}
