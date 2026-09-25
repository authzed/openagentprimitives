package aptest

import (
	"io"
	"testing"
)

// RefusingReader fails the test if anything reads from it. Given to a
// --non-interactive run to prove it never prompts: huh's accessible renderer
// discards per-field read errors, so a run that DID prompt would otherwise
// look like a run that did not.
type RefusingReader struct{ T *testing.T }

// Read records the failure and reports EOF, so the run under test proceeds
// down the same path an exhausted stream would take.
func (r RefusingReader) Read([]byte) (int, error) {
	r.T.Helper()
	r.T.Error("the wizard read from the input stream during a --non-interactive run")
	return 0, io.EOF
}
