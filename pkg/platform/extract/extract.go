// Package extract turns an uploaded file into agent-readable text. It is the
// inbound mirror of pkg/channels/channelassets: a MIME type selects a registered
// backend, so a new format is a registration rather than a branch in a caller.
//
// Extractors run in the extractord pod against untrusted user uploads. They
// must never perform network I/O — that pod has no egress and no credentials by
// design. A backend whose underlying library requires a filesystem path may use
// os.TempDir() and must remove what it writes; nothing else on disk is theirs to
// touch. Malformed input must produce an error, never a panic.
package extract

import (
	"errors"
	"io"
)

// ErrTooLarge is returned when input exceeds an extractor's own limit.
var ErrTooLarge = errors.New("extract: input too large")

// Result is what an extractor produces.
type Result struct {
	// Text is the extracted plain text, suitable for an agent to read.
	Text string
	// Pages is a page/slide count when the format has one, else 0. Reported to
	// the agent in the attachment manifest so it can judge scale before reading.
	Pages int
}

// Extractor converts one family of MIME types into text.
type Extractor interface {
	// MIMEs returns the exact MIME types this extractor claims. Registration
	// panics on a duplicate claim, so two backends cannot silently fight.
	MIMEs() []string
	// Extract reads r to EOF and returns the text. Implementations must not
	// perform network or filesystem I/O.
	Extract(r io.Reader) (Result, error)
}
