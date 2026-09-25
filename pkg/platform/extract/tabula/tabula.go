// Package tabula adapts github.com/tsawler/tabula (MIT; pure Go on the
// build path used here — OCR support lives behind the library's own "ocr"
// build tag, which pulls in a CGO Tesseract binding we never enable) to
// extract.Extractor.
//
// Reader vs. path: tabula's top-level fluent Extractor can stream only HTML
// from an arbitrary io.Reader (FromHTMLReader). PDF, DOCX, and PPTX are
// reachable only through tabula.Open(filename), which dispatches on the
// file's extension (format.Detect); the lower-level per-format readers
// (docx.Open, pptx.Open) are filename-only too, and PDF's reader.NewReader
// wants an *os.File, not a generic io.Reader. So this adapter writes its
// input to a file under os.TempDir() for every format, including HTML,
// rather than splitting into a stream path for one format and a temp-file
// path for the rest.
//
// The registered MIME set (see init) is deliberately narrower than what tabula
// supports: XLSX, ODT, and EPUB are left out. An unregistered MIME produces an
// honest "this file type cannot be read" notice downstream; a registered one
// that crashes the service produces an outage. XLSX is the concrete hazard — a
// hostile cell reference (a column of 7+ repeated "Z"s) makes tabula's xlsx
// reader compute a column index in the billions and allocate a Go slice that
// wide per row (xlsx/reader.go:298,300), which hangs or OOM-kills the process.
// That is not a panic, so DefaultTimeout contains the hang but no recover()
// survives the out-of-memory case. ODT and EPUB stay out on the same
// "don't register what isn't verified" principle.
package tabula

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	tsawler "github.com/tsawler/tabula"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// maxInputBytes caps a single extraction. Untrusted uploads have no other
// size gate at this layer, so the adapter owns the guard rather than
// trusting the library.
const maxInputBytes = 25 * 1024 * 1024 // 25 MiB

// DefaultTimeout bounds how long a single extraction may run before this
// adapter gives up and returns an error instead of blocking the caller
// forever. It exists because a hostile input can make tabula hang (see the
// package doc); a timeout can't reclaim memory a runaway allocation is
// mid-requesting, so it doesn't substitute for extractord's own container
// memory limit, but it does turn a hang into a bounded failure for the one
// upload that triggered it. Exported so a caller's own deadline (a later
// task, at the extractord request level) can be set relative to it.
const DefaultTimeout = 30 * time.Second

// backend is the one Extractor implementation this package registers, once
// per MIME type it claims (see init). ext is the filename extension
// tabula.Open's format.Detect needs to route to the right parser.
// reportPages says whether tabula's PageCount() reflects a real count for
// this format: it does for PDF (pages) and PPTX (slides), but DOCX and
// HTML always return a hardcoded 1 regardless of actual content —
// reporting that as Pages would tell the agent a 40-page report is one
// page, which is worse than reporting nothing.
type backend struct {
	mime        string
	ext         string
	reportPages bool
}

func (b backend) MIMEs() []string { return []string{b.mime} }

// Extract reads r to EOF, spills it to a temp file with the extension
// tabula needs, and parses it under DefaultTimeout. It performs no network
// I/O.
func (b backend) Extract(r io.Reader) (extract.Result, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return extract.Result{}, fmt.Errorf("extract/tabula: reading input: %w", err)
	}
	if len(data) > maxInputBytes {
		return extract.Result{}, extract.ErrTooLarge
	}

	f, err := os.CreateTemp("", "extract-tabula-*"+b.ext)
	if err != nil {
		return extract.Result{}, fmt.Errorf("extract/tabula: creating temp file: %w", err)
	}
	path := f.Name()
	// This path is ours alone: created above, named with a random component,
	// never handed out. Removing it is the only cleanup tabula's file-based
	// API leaves us responsible for. Safe to remove even while a timed-out
	// extraction goroutine (below) still holds it open: unlinking a path
	// doesn't invalidate an already-open file descriptor on the OSes this
	// runs on.
	defer func() { _ = os.Remove(path) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return extract.Result{}, fmt.Errorf("extract/tabula: writing temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return extract.Result{}, fmt.Errorf("extract/tabula: closing temp file: %w", err)
	}

	return b.extractWithTimeout(path)
}

// extractWithTimeout runs b.parse in a goroutine, wrapped by recoverToError,
// and bounds it by DefaultTimeout. A hang inside tabula (see the package
// doc) cannot be interrupted from here — Go has no way to force another
// goroutine to stop — so on timeout this returns an error and abandons the
// goroutine rather than waiting on it; the goroutine's memory is reclaimed
// only when it eventually returns or the process is restarted. The recover
// runs inside the spawned goroutine, not in extractWithTimeout itself: a
// recover only catches a panic on its own goroutine's stack, so putting it
// anywhere else would silently do nothing while the panic still took the
// process down.
func (b backend) extractWithTimeout(path string) (extract.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	type outcome struct {
		result extract.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := recoverToError(b.mime, func() (extract.Result, error) {
			return b.parse(path)
		})
		done <- outcome{result, err}
	}()

	select {
	case o := <-done:
		return o.result, o.err
	case <-ctx.Done():
		return extract.Result{}, fmt.Errorf("extract/tabula: extracting %s: %w (exceeded %s)", b.mime, ctx.Err(), DefaultTimeout)
	}
}

// recoverToError runs fn and converts any panic into an error instead of
// letting it propagate. tabula parses attacker-controlled bytes on the
// other side of fn, so this is a boundary for untrusted input, not a
// substitute for normal error handling elsewhere in this package. Factored
// out of extractWithTimeout's goroutine so the recover mechanism itself is
// directly testable (see recover_test.go) independent of any particular
// hostile fixture.
func recoverToError(mime string, fn func() (extract.Result, error)) (result extract.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			result = extract.Result{}
			err = fmt.Errorf("extract/tabula: panic extracting %s: %v", mime, p)
		}
	}()
	return fn()
}

// parse drives tabula against path. Called only from inside
// extractWithTimeout's recoverToError-wrapped goroutine, never directly.
func (b backend) parse(path string) (extract.Result, error) {
	ext := tsawler.Open(path)

	pages := 0
	if b.reportPages {
		// A failed count doesn't abort extraction — Text() below makes its
		// own independent judgment on whether the document is readable at
		// all. Losing the count just leaves Pages at 0.
		if n, pcErr := ext.PageCount(); pcErr == nil {
			pages = n
		}
	}

	// Text is a terminal operation: it closes tabula's underlying reader on
	// both the success and error paths, so no explicit Close here.
	text, _, err := ext.Text()
	if err != nil {
		return extract.Result{}, fmt.Errorf("extract/tabula: extracting %s: %w", b.mime, err)
	}

	return extract.Result{Text: text, Pages: pages}, nil
}

func init() {
	for _, b := range []backend{
		{mime: "application/pdf", ext: ".pdf", reportPages: true},
		{mime: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ext: ".docx", reportPages: false},
		{mime: "application/vnd.openxmlformats-officedocument.presentationml.presentation", ext: ".pptx", reportPages: true},
		{mime: "text/html", ext: ".html", reportPages: false},
	} {
		extract.Register(b)
	}
}
