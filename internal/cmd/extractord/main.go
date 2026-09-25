// Command extractord is a small, deliberately powerless HTTP service that
// wraps the pkg/platform/extract registry. POST /extract takes raw file bytes with a
// Content-Type header naming the MIME and returns {"text","pages"}; GET
// /healthz reports liveness. It has no egress (config/networkpolicy/extractord.yaml),
// mounts no Secret, and mounts no Kubernetes ServiceAccount token either
// (config/extractord/serviceaccount.yaml sets automountServiceAccountToken:
// false — the kubelet projects one by default otherwise, and a projected SA
// token IS a mounted credential even with an empty RBAC binding) — bytes come
// in over HTTP and text goes out in the response, and it never calls back to
// anything else in the platform. That powerlessness is the point: it is the
// one component in the attachment path that runs directly against untrusted
// user-uploaded bytes, so a parser bug or a hostile file can crash or hang
// this pod without handing an attacker network access, credentials, or the
// Kubernetes API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/pkg/cli/clikit"
	"github.com/authzed/openagentprimitives/pkg/platform/deplogs"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"

	// Blank-imported so their init() registers with pkg/platform/extract at process
	// startup. Without both imports the registry is empty and every request
	// resolves to 415, regardless of MIME.
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/tabula"
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/text"
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/ziparchive"
)

// defaultMaxBytes bounds a single POST /extract body via http.MaxBytesReader.
// It matches pkg/platform/extract/tabula's and pkg/platform/extract/text's own (unexported)
// per-backend caps, 25 MiB, so this outer limit and each backend's inner
// extract.ErrTooLarge limit agree: a body extractord accepts is never one a
// backend would separately refuse, and one a backend would refuse is already
// rejected here first.
const defaultMaxBytes int64 = 25 << 20 // 25 MiB

// shutdownTimeout bounds the graceful-shutdown window on SIGINT/SIGTERM.
const shutdownTimeout = 5 * time.Second

// config holds extractord's configuration, populated from flags and (via
// clikit) the environment. There is no secret configuration: extractord
// holds no credentials at all.
type config struct {
	addr     string
	maxBytes int64
}

func main() {
	// extractord does not link SpiceDB's schema compiler today, so this is the
	// one call in the fleet that silences nothing yet. It is here so the rule
	// is "every server binary silences dependency chatter" rather than "the
	// ones we happened to check" — a new dependency logging to zerolog's
	// global is otherwise a silent regression in a binary nobody re-audits.
	// See pkg/platform/deplogs.
	deplogs.Silence()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:          "extractord",
		Short:        "Zero-egress attachment-extraction service: POST /extract wraps the pkg/platform/extract registry.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run(cmd.Context(), cfg)
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cfg.addr, "addr", ":8080", "HTTP listen address")
	fs.Int64Var(&cfg.maxBytes, "max-bytes", defaultMaxBytes, "maximum accepted /extract request body size, in bytes")
	cmd.PreRunE = clikit.EnvOverridePreRunE(map[string]string{
		"addr":      "EXTRACTORD_ADDR",
		"max-bytes": "EXTRACTORD_MAX_BYTES",
	})
	return cmd
}

func run(ctx context.Context, cfg *config) {
	srv := &http.Server{Addr: cfg.addr, Handler: routes(cfg.maxBytes)}
	// HardenServer sets ReadHeaderTimeout (the slow-loris defense) and
	// IdleTimeout, and deliberately leaves ReadTimeout/WriteTimeout at 0:
	// extractord legitimately accepts bodies up to maxBytes, so a whole-request
	// timeout would truncate a large-but-legitimate upload on a slow link. The
	// extraction phase itself is bounded independently — tabula.DefaultTimeout
	// inside pkg/platform/extract/tabula turns a hostile input's hang into a bounded
	// failure without needing a duplicate deadline here.
	safehttp.HardenServer(srv)

	go func() {
		// A bind failure leaves /healthz permanently unreachable; log + exit so
		// it surfaces as a crash-loop instead of a silently wedged pod.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("extractord: ListenAndServe failed", "addr", cfg.addr, "err", err.Error())
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("extractord: shutdown")
	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		slog.Error("extractord: graceful shutdown failed", "err", err.Error())
	}
}

// extractResponse is the POST /extract success body.
type extractResponse struct {
	// Text is the extracted plain text; empty means the document held none.
	Text string `json:"text"`
	// Pages is the source page count; 0 for formats that are not paginated.
	Pages int `json:"pages"`
}

// routes builds extractord's handler: POST /extract and GET /healthz. Go's
// method-qualified mux patterns answer every other method on a registered
// path with an automatic 405 + Allow header, so GET /extract needs no
// handwritten method check.
func routes(maxBytes int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extract", handleExtract(maxBytes))
	mux.HandleFunc("POST /explode", handleExplode(maxBytes))
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

// handleExtract serves POST /extract. The Content-Type header names the
// MIME; the raw body is the file bytes; the response is {"text","pages"}.
//
// Status codes are chosen so the caller can tell a permanent failure from a
// transient one and word its notice accordingly:
//   - 415 Unsupported Media Type: no backend claims this MIME. Permanent —
//     retrying can't help, so the caller shows a "this file type cannot be
//     read" notice rather than a transient-failure one. Load-bearing: this
//     is the ONLY signal that distinguishes the two notice types.
//   - 413 Request Entity Too Large: the body exceeded maxBytes (caught by
//     http.MaxBytesReader) or a backend's own internal cap
//     (extract.ErrTooLarge). Also permanent.
//   - 422 Unprocessable Entity: the backend recognized the MIME but the
//     bytes didn't parse as that format (corrupt/malformed input) — distinct
//     from a 5xx, which would imply extractord itself is unhealthy.
func handleExtract(maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mime := r.Header.Get("Content-Type")
		extractor, ok := extract.For(mime)
		if !ok {
			slog.Info("extractord: unsupported MIME", "mime", mime)
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}

		// body counts bytes actually read as they pass through it, so every
		// outcome below logs a real received-byte count — r.ContentLength is -1
		// for a chunked-encoded request and can't be used for this.
		body := &countingReader{r: http.MaxBytesReader(w, r.Body, maxBytes)}
		result, err := extractor.Extract(body)
		if err != nil {
			respondExtractError(w, mime, body.n, err)
			return
		}

		slog.Info("extractord: extracted", "mime", mime, "bytes", body.n, "extractedBytes", len(result.Text), "pages", result.Pages)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(extractResponse{Text: result.Text, Pages: result.Pages}); err != nil {
			// The 200 status is already written by the time Encode can fail
			// (partial write), so there is nothing left to do but log. This error
			// is about the response writer, not the uploaded file, so logging it
			// verbatim carries no file-content risk.
			slog.Error("extractord: encode response", "mime", mime, "err", err.Error())
		}
	}
}

// countingReader wraps an io.Reader and tracks the total bytes read through
// it, so a caller can log how many bytes an upload actually was without
// relying on r.ContentLength (which is -1 for a chunked-encoded body).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// classifyExtractError maps an extraction failure to an HTTP status, a
// content-free classification for logging (errKind), and a content-free
// response message. It deliberately never returns err.Error() itself:
// pkg/platform/extract/tabula wraps upstream parser errors from
// github.com/tsawler/tabula that embed raw bytes read from the uploaded
// file directly in the error string — e.g. its reader package formats
// "invalid PDF header: %s" with the literal header bytes it read, and its
// xref package does the same with raw PDF-stream bytes. Forwarding
// err.Error() to a log or an HTTP response would leak file content through
// exactly the channel this deliberately powerless pod must never use for
// that.
func classifyExtractError(err error) (status int, errKind, msg string) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr), errors.Is(err, extract.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large", "request body too large"
	case errors.Is(err, context.DeadlineExceeded):
		// pkg/platform/extract/tabula's DefaultTimeout wraps context.DeadlineExceeded
		// when a hostile input makes parsing hang.
		return http.StatusUnprocessableEntity, "timeout", "could not extract text from this file"
	default:
		return http.StatusUnprocessableEntity, "parse_failed", "could not extract text from this file"
	}
}

// respondExtractError logs a content-free classification of an extraction
// failure — MIME, the actual received byte count, and errKind, never
// err.Error() (see classifyExtractError) — and writes the matching status
// and content-free response body.
func respondExtractError(w http.ResponseWriter, mime string, n int64, err error) {
	status, errKind, msg := classifyExtractError(err)
	slog.Info("extractord: extraction failed", "mime", mime, "bytes", n, "errKind", errKind)
	http.Error(w, msg, status)
}
