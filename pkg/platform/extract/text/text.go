// Package text is the extract.Extractor for formats that are already text:
// no document-parsing library buys anything over reading to EOF and coercing
// to valid UTF-8. It claims text/plain, text/markdown, text/csv, and
// application/json ahead of pkg/platform/extract/tabula's init — Register panics on a
// duplicate claim, so if tabula ever tried to claim one of these too, that
// conflict would surface at process startup rather than depend on import
// order.
package text

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

// maxInputBytes matches pkg/platform/extract/tabula's cap. Each backend enforces its
// own limit rather than sharing a constant across packages, but the two
// values are kept equal so the cap behaves the same regardless of which
// backend handled a given upload.
const maxInputBytes = 25 * 1024 * 1024 // 25 MiB

// backend is the Extractor for MIME types that are already plain text.
type backend struct{ mimes []string }

func (b backend) MIMEs() []string { return b.mimes }

// Extract reads r to EOF and returns it as valid UTF-8, replacing any
// invalid byte sequences with U+FFFD. Plain text has no page concept, so
// Result.Pages is always 0.
func (b backend) Extract(r io.Reader) (extract.Result, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return extract.Result{}, fmt.Errorf("extract/text: reading input: %w", err)
	}
	if len(data) > maxInputBytes {
		return extract.Result{}, extract.ErrTooLarge
	}

	s := string(data)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return extract.Result{Text: s}, nil
}

func init() {
	extract.Register(backend{mimes: []string{
		"text/plain",
		"text/markdown",
		"text/csv",
		"application/json",
	}})
}
