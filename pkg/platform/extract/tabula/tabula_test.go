package tabula_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/tabula"
	_ "github.com/authzed/openagentprimitives/pkg/platform/extract/text" // registers the plain-text MIMEs so TestSupported sees the full intended set
)

const (
	pdfMIME  = "application/pdf"
	docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	pptxMIME = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	htmlMIME = "text/html"
	// xlsxMIME is deliberately not registered (see tabula.go's package doc);
	// it's kept here only for TestXLSX_NotRegistered.
	xlsxMIME = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

func TestExtract_HappyPath(t *testing.T) {
	cases := []struct {
		name      string
		mime      string
		data      []byte
		wantText  string
		wantPages int
	}{
		{
			name: "pdf: real page count", mime: pdfMIME, data: buildPDF("Hello PDF"),
			wantText: "Hello PDF", wantPages: 1,
		},
		{
			// DOCX has no real pagination without rendering; tabula's own
			// PageCount() hardcodes 1 for every DOCX regardless of length, so
			// this adapter reports 0 rather than that misleading stand-in.
			name: "docx: no real page count, reports 0", mime: docxMIME, data: buildDOCX("Hello DOCX"),
			wantText: "Hello DOCX", wantPages: 0,
		},
		{
			name: "pptx: real slide count", mime: pptxMIME, data: buildPPTX([]string{"Slide One", "Slide Two"}),
			wantText: "Slide One", wantPages: 2,
		},
		{
			name: "html: no page concept, reports 0", mime: htmlMIME,
			data:     []byte("<html><body><p>Hello HTML</p></body></html>"),
			wantText: "Hello HTML", wantPages: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := extract.For(tc.mime)
			require.True(t, ok, "MIME %q must be registered", tc.mime)

			got, err := e.Extract(bytes.NewReader(tc.data))
			require.NoError(t, err)
			assert.Contains(t, got.Text, tc.wantText)
			assert.Equal(t, tc.wantPages, got.Pages)
		})
	}
}

func TestExtract_MalformedInput_ReturnsErrorNotPanic(t *testing.T) {
	// Garbage bytes under each MIME's extension: none of these are valid ZIP
	// or PDF structure, so tabula's own format validation must reject them
	// with an error. HTML is deliberately absent — arbitrary bytes are
	// syntactically valid (if meaningless) HTML, so there is no analogous
	// "malformed" case for it.
	garbage := []byte("this is not a valid document of any kind, just garbage bytes 12345")

	cases := []struct {
		name string
		mime string
	}{
		{name: "docx", mime: docxMIME},
		{name: "pptx", mime: pptxMIME},
		{name: "pdf", mime: pdfMIME},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := extract.For(tc.mime)
			require.True(t, ok)

			_, err := e.Extract(bytes.NewReader(garbage))
			assert.Error(t, err, "malformed input must produce an error, never a panic that crashes this test")
		})
	}
}

func TestExtract_OversizedInput_ReturnsErrTooLarge(t *testing.T) {
	e, ok := extract.For(htmlMIME)
	require.True(t, ok)

	oversized := bytes.Repeat([]byte("a"), 25*1024*1024+1) // one byte past the documented 25 MiB cap
	_, err := e.Extract(bytes.NewReader(oversized))
	assert.ErrorIs(t, err, extract.ErrTooLarge)
}

// TestExtract_HighCompressionRatioInput_CompletesWithoutHanging feeds a
// "zip-bomb-shaped" input — a payload that compresses at a wildly
// disproportionate ratio — and asserts extraction finishes in bounded time.
//
// This is not proof against a true multi-GB bomb: neither tabula's DOCX reader
// nor this adapter guards against decompression RATIO, only wire size (the
// 25 MiB cap Extract enforces before handing bytes to tabula). What it verifies
// is that this adapter's own plumbing — buffering, temp-file write, delegation
// — does not compound the problem with quadratic re-processing, and stays
// inside DefaultTimeout.
//
// The deadline below is DERIVED from tabula.DefaultTimeout and deliberately
// LONGER than it, so the two arms test different things: Extract bounds itself
// by DefaultTimeout and returns an error rather than hanging, so blowing past
// the production bound comes back as err (caught by require.NoError), while
// time.After fires only if Extract's own timeout never did — a true hang.
//
// A deadline SHORTER than DefaultTimeout can express neither: it asserts
// wall-clock performance rather than the contract, failing a run that would
// have succeeded in production. Under the loaded parallel suite (-p=4 -race
// alongside envtest control planes) this ~0.4s extraction takes 40x longer.
// The defect class this test exists for — quadratic re-processing of 10 MB —
// takes minutes, not seconds, so the longer deadline still catches it.
func TestExtract_HighCompressionRatioInput_CompletesWithoutHanging(t *testing.T) {
	data := buildHighRatioDOCX("word", 2_000_000) // ~10 MB decompressed; compresses to a few KB

	e, ok := extract.For(docxMIME)
	require.True(t, ok)

	type outcome struct {
		res extract.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := e.Extract(bytes.NewReader(data))
		done <- outcome{res, err}
	}()

	// Margin over DefaultTimeout: enough that Extract's own timeout always
	// wins the race on a loaded machine, so a breach is reported as the
	// error it is rather than as this backstop.
	const hangBackstop = tabula.DefaultTimeout + 30*time.Second

	select {
	case o := <-done:
		require.NoError(t, o.err,
			"extraction must finish inside DefaultTimeout; an error here means it did not")
		assert.Contains(t, o.res.Text, "word")
	case <-time.After(hangBackstop):
		t.Fatalf("extraction hung: no result after %s, which is past Extract's own %s bound — "+
			"its timeout did not fire", hangBackstop, tabula.DefaultTimeout)
	}
}

// TestXLSX_NotRegistered is a regression guard for the finding that kept
// XLSX out of the registered set (see tabula.go's package doc): a cell
// reference of enough repeated "Z"s overflows
// int64 in tabula's xlsx.ColumnToIndex, and a shorter-but-still-enormous
// run drives an allocation that hangs or OOMs the process outright — a
// failure mode recover() cannot help with, since a Go out-of-memory
// condition is fatal regardless of any deferred recover in any goroutine.
//
// buildXLSXWithCellRef constructs that exact hostile shape so the fixture
// stays next to the finding it documents, but it is deliberately never
// passed to Extract — there is no registered backend to call. If XLSX is
// ever re-registered without first fixing or working around the upstream
// bug, this test is what trips instead of the DoS being silently
// reintroduced.
func TestXLSX_NotRegistered(t *testing.T) {
	_ = buildXLSXWithCellRef(strings.Repeat("Z", 13)) // the exact fixture that panics tabula's xlsx reader; never fed to Extract

	_, ok := extract.For(xlsxMIME)
	assert.False(t, ok, "XLSX must stay unregistered until the upstream allocation bug is fixed or worked around")
}

func TestSupported_ContainsExactlyTheIntendedMIMESet(t *testing.T) {
	want := []string{
		pdfMIME,
		docxMIME,
		pptxMIME,
		htmlMIME,
		"text/plain",
		"text/markdown",
		"text/csv",
		"application/json",
	}
	assert.ElementsMatch(t, want, extract.Supported(),
		"the registered MIME set is Task 5's supported-vs-unsupported gate; a silent gap here becomes a user-visible wrong notice")
}
