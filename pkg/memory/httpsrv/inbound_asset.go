package httpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"reflect"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/memory/assetlimits"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// MaxInboundAssetBytes hard-caps a single inbound attachment upload,
// independent of any Channel's spec.attachments.maxSizeBytes — the operator's
// own absolute ceiling, matching extractord's defaultMaxBytes so the two never
// disagree about what "too large" means on the extracted path.
//
// It ALIASES assetlimits.MaxInboundAssetBytes rather than defining a value:
// channelsd's pipeline needs the same ceiling to clamp its per-attachment
// limit, but pipeline is reachable from the browser-facing chat package while
// this one pulls in provenance (audit-signing), so pipeline must import the
// leaf assetlimits package directly and never this one. Without the clamp a
// file between the channel's limit and this cap would pass the pre-fetch check,
// download in full, and only then trip the ceiling — a "temporary failure"
// note after a wasted download instead of a cheap pre-fetch "oversize" one.
const MaxInboundAssetBytes = assetlimits.MaxInboundAssetBytes

// AttachmentExtractor turns raw attachment bytes into extracted text by calling
// extractord. Optional, injected by internal/cmd/operator. Nil means "not configured":
// serveInboundAsset still stores the raw bytes but reports extraction as
// unavailable, which the channelsd pipeline MUST treat as a transient failure —
// never a silently missing capability, and never "unsupported type".
type AttachmentExtractor interface {
	// Extract reads body to EOF and returns the extracted text and a page/slide
	// count (0 when the format has none). ErrAttachmentMIMEUnsupported means
	// extractord answered 415 — no backend claims mime, a PERMANENT condition.
	// Every other error (5xx, timeout, connection failure) is TRANSIENT, and
	// callers must not conflate the two.
	Extract(ctx context.Context, mime string, body io.Reader) (text string, pages int, err error)
}

// ErrAttachmentMIMEUnsupported is returned when extractord reports 415: no
// registered backend claims the MIME. PERMANENT, and deliberately distinct from
// every other extraction failure, which is transient.
var ErrAttachmentMIMEUnsupported = errors.New("httpsrv: no extractor claims this attachment MIME")

// WithAttachmentExtractor enables extraction on the /inbound-asset route.
// Without it the route still stores bytes — an upload never fails for want of
// an extractor — but every response reports Extracted=false, Unsupported=false,
// which the channelsd pipeline reads as a transient failure rather than a
// silent skip.
//
// TYPED-NIL SAFETY: the natural DI shape for "fail-closed if unset" is
// `var c *extractordClient; if endpoint != "" { c = New(...) }`, and passing
// that nil POINTER yields a non-nil interface ({type, nil}), so the
// `h.attachmentExtractor == nil` guard would be false and the first upload
// would panic calling Extract on a nil receiver. Rather than trust every future
// caller to remember AGENTS.md's typed-nil rule, this constructor defends the
// seam: a nil pointer in a non-nil interface is detected by reflection and
// normalized to a true nil interface.
func WithAttachmentExtractor(e AttachmentExtractor) HandlerOption {
	return func(h *handler) {
		if e != nil {
			if v := reflect.ValueOf(e); v.Kind() == reflect.Ptr && v.IsNil() {
				e = nil
			}
		}
		h.attachmentExtractor = e
	}
}

// inboundAssetResponse is the wire contract channelsd's UploadInboundAsset
// decodes. The two ways text can be missing are deliberately distinguishable:
// Unsupported=true is PERMANENT, while Unsupported=false with Extracted=false is
// TRANSIENT and must never be read as "this type can't be read".
type inboundAssetResponse struct {
	// Ref is the artifactstore handle for the raw bytes; always set on a 200,
	// since bytes are stored even when nothing extracts them.
	Ref string `json:"ref"`
	// Extracted is true only when text was actually produced.
	Extracted bool `json:"extracted"`
	// TextRef is the artifactstore handle for the extracted text; empty unless
	// Extracted is true.
	TextRef string `json:"textRef,omitempty"`
	// Pages is the extractor-reported page/slide count; 0 when the format has no
	// notion of pages or nothing was extracted.
	Pages int `json:"pages,omitempty"`
	// Members is one entry per stored archive member. Non-empty only for an
	// archive that was exploded; TextRef then points at the index.
	Members []memberResult `json:"members,omitempty"`
	// ArchiveTruncated reports that a bound stopped the walk early, so the
	// caller can say the bundle is partial rather than implying it is whole.
	ArchiveTruncated bool `json:"archiveTruncated,omitempty"`
	// ArchiveTruncatedReason names the bound that tripped.
	ArchiveTruncatedReason string `json:"archiveTruncatedReason,omitempty"`
	// ArchiveTruncatedMember names the member whose copy was cut, so a caller
	// can mark THAT attachment partial rather than treating every row in a
	// truncated archive as suspect -- or, worse, none of them.
	ArchiveTruncatedMember string `json:"archiveTruncatedMember,omitempty"`

	// Unsupported marks the PERMANENT case: no extractor backend claims the MIME.
	Unsupported bool `json:"unsupported,omitempty"`
}

// serveInboundAsset handles POST /inbound-asset/{ns}/{sess}.
//
// Auth: the same system-bearer check serveArtifact uses, declared writeAccess —
// this is the ONE route in this server that writes, so only channelsd's token is
// accepted. webd's read-only browser-facing token is refused (403): it must
// never append into an arbitrary session's key space or drive extractord. No
// per-session memory token is accepted either.
//
// The request body is the raw attachment bytes, streamed straight into
// artifactstore.Put and never buffered whole here. Metadata rides in headers:
// Content-Type (required) and X-Attachment-Filename (optional).
//
// Statuses: 401 missing/unknown bearer; 403 webd's read-only token; 405
// non-POST; 400 malformed path, an {ns}/{sess} that is not a valid Kubernetes
// name, or missing Content-Type; 413 over MaxInboundAssetBytes; 500 store write
// failure. On success, 200 with an inboundAssetResponse.
//
// Extraction runs synchronously against the stored bytes via a fresh
// artifactstore.Get — a second read of the same object, NOT a second copy of the
// request body, which is read exactly once.
func (h *handler) serveInboundAsset(w http.ResponseWriter, r *http.Request) {
	if !h.checkSystemBearer(w, r, writeAccess) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/inbound-asset/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "expected /inbound-asset/{ns}/{sess}", http.StatusBadRequest)
		return
	}
	ns, sess := parts[0], parts[1]

	// r.URL.Path is the DECODED path — a %2f in the raw request decodes to a
	// literal "/" here, so a segment that LOOKED like a single {sess} to
	// http.ServeMux's routing can smuggle extra "/"-delimited components
	// through the SplitN above (e.g. "sess1%2f..%2f..%2fartifact%2fvictim-ns%2fvictim-sess").
	// Kubernetes namespace/object names can never legitimately contain "/",
	// so rejecting anything that isn't a valid name closes this off
	// categorically rather than trying to enumerate traversal patterns.
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		http.Error(w, "invalid {ns}: "+strings.Join(errs, "; "), http.StatusBadRequest)
		return
	}
	if errs := validation.IsDNS1123Subdomain(sess); len(errs) > 0 {
		http.Error(w, "invalid {sess}: "+strings.Join(errs, "; "), http.StatusBadRequest)
		return
	}

	mime := r.Header.Get("Content-Type")
	if mime == "" {
		http.Error(w, "Content-Type required", http.StatusBadRequest)
		return
	}
	filename := sanitizeUploadFilename(r.Header.Get("X-Attachment-Filename"))

	body := http.MaxBytesReader(w, r.Body, MaxInboundAssetBytes)
	defer r.Body.Close()

	ctx := r.Context()
	assetID := uuid.New().String()
	// Keys MUST be laid out "<ns>/<session>/…", the convention the toolcall
	// controller shares: that is what lets a per-session memory token's
	// fetch_artifact call authorize against the key. Prefixing anything before
	// <ns> parses as ns=<prefix>, sess=<ns> and can never authorize.
	key := path.Join(ns, sess, "inbound-asset", assetID, storageFilename(filename))
	ref, err := h.artifactStore.Put(ctx, key, body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "attachment too large", http.StatusRequestEntityTooLarge)
			return
		}
		logInboundAssetError(ctx, "store attachment failed", ns, sess, filename, err)
		http.Error(w, "store attachment: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := inboundAssetResponse{Ref: string(ref)}
	// An archive fans OUT: its members are stored individually and its
	// "extracted text" is an index naming them. Anything else takes the
	// single-file extraction path unchanged.
	if h.explodeInboundArchive(ctx, ns, sess, assetID, mime, filename, ref, &resp) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	h.extractInboundAsset(ctx, ns, sess, assetID, mime, filename, ref, &resp)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// extractInboundAsset runs extraction against the just-stored bytes and fills
// resp. With no AttachmentExtractor configured it is a no-op, leaving resp in
// the TRANSIENT shape (Extracted=false, Unsupported=false). Every failure is
// logged rather than silently dropped.
func (h *handler) extractInboundAsset(ctx context.Context, ns, sess, assetID, mime, filename string, ref artifactstore.Ref, resp *inboundAssetResponse) {
	if h.attachmentExtractor == nil {
		return
	}
	rc, err := h.artifactStore.Get(ctx, ref)
	if err != nil {
		logInboundAssetError(ctx, "re-read stored attachment for extraction failed", ns, sess, filename, err)
		return
	}
	defer rc.Close()

	text, pages, eerr := h.attachmentExtractor.Extract(ctx, mime, rc)
	if eerr != nil {
		if errors.Is(eerr, ErrAttachmentMIMEUnsupported) {
			resp.Unsupported = true
			return
		}
		logInboundAssetError(ctx, "attachment extraction failed", ns, sess, filename, eerr)
		return
	}

	textKey := path.Join(ns, sess, "inbound-asset", assetID, "text.txt")
	textRef, terr := h.artifactStore.Put(ctx, textKey, strings.NewReader(text))
	if terr != nil {
		logInboundAssetError(ctx, "store extracted text failed", ns, sess, filename, terr)
		return
	}
	resp.Extracted = true
	resp.TextRef = string(textRef)
	resp.Pages = pages
}

func logInboundAssetError(ctx context.Context, msg, ns, sess, filename string, err error) {
	log.FromContext(ctx).Info(msg, "namespace", ns, "session", sess, "filename", filename, "err", err.Error())
}

// uploadFilenameMaxRunes caps the filename this handler stores and echoes.
// Deliberately independent of channelsd's note-display truncation: this length
// affects only the artifactstore key and the JSON response, not agent-facing
// prose.
const uploadFilenameMaxRunes = 200

// sanitizeUploadFilename strips control characters — CR/LF included, so a
// filename can never inject header-like content downstream — and truncates to
// uploadFilenameMaxRunes. Defence in depth: the listener already sanitizes the
// filename upstream, but this route accepts any valid system bearer, so it
// re-sanitizes rather than trusting the caller. This is the value echoed in the
// JSON response and logged; storageFilename applies further path-specific
// stripping before it becomes an artifactstore key.
func sanitizeUploadFilename(name string) string {
	var b strings.Builder
	count := 0
	for _, r := range name {
		if count >= uploadFilenameMaxRunes {
			break
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// storageFilename returns a safe, non-empty path segment for the artifactstore
// key. The caller's path.Join cleans ".." against the WHOLE joined path, so an
// attacker-controlled filename like "../../../other-ns/other-sess/evil" would
// otherwise escape the {ns}/{sess}/{assetID}/ prefix and let a forged
// X-Attachment-Filename header write into a foreign session's key space.
// Stripping every path separator and any leading dot makes that
// unrepresentable: the result can introduce no directory boundary and can never
// resolve to "." or "..".
func storageFilename(name string) string {
	replaced := strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	// Drop control chars and the bracket characters that would otherwise ride
	// into the runner's out-of-view attachment note — a bracketed `[…]` line the
	// model reads as trusted text. That note embeds this key UNQUOTED (the
	// handle must be copied verbatim into show_attachment, so it cannot be
	// quoted), so a `]` in the key closes the note early. The display filename
	// is stored separately and keeps the real name; only the artifactstore KEY
	// is narrowed, and only for new uploads (reads use the stored key).
	replaced = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '[' || r == ']' {
			return -1
		}
		return r
	}, replaced)
	trimmed := strings.TrimLeft(strings.TrimSpace(replaced), ".")
	if trimmed == "" {
		return "attachment"
	}
	return trimmed
}
