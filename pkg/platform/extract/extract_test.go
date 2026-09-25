package extract_test

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

type fakeExtractor struct{ mimes []string }

func (f fakeExtractor) MIMEs() []string { return f.mimes }
func (f fakeExtractor) Extract(r io.Reader) (extract.Result, error) {
	b, err := io.ReadAll(r)
	return extract.Result{Text: string(b)}, err
}

// mimeCounter backs uniqueMIME. extract.Register claims against a
// process-global registry and panics on a duplicate claim (by design — see
// registry.go), so a test that registers a fixed literal MIME collides
// with itself on its own second invocation under `go test -count=N`. A
// counter-suffixed MIME per call sidesteps that without adding a
// test-only unregister to the production API.
var mimeCounter atomic.Uint64

func uniqueMIME(label string) string {
	return fmt.Sprintf("application/x-%s-%d", label, mimeCounter.Add(1))
}

func TestFor_IgnoresMIMEParameters(t *testing.T) {
	mime := uniqueMIME("registry-test")
	extract.Register(fakeExtractor{mimes: []string{mime}})

	e, ok := extract.For(mime + "; charset=utf-8")
	require.True(t, ok, "a MIME with parameters must resolve to the base type's backend")

	got, err := e.Extract(strings.NewReader("hi"))
	require.NoError(t, err)
	assert.Equal(t, "hi", got.Text)
}

func TestFor_UnknownMIME_ReportsNotFound(t *testing.T) {
	_, ok := extract.For("application/x-not-registered")
	assert.False(t, ok, "an unclaimed MIME must report not-found so callers can emit the unsupported notice")
}

func TestRegister_DuplicateClaim_Panics(t *testing.T) {
	mime := uniqueMIME("dup-test")
	extract.Register(fakeExtractor{mimes: []string{mime}})
	assert.Panics(t, func() {
		extract.Register(fakeExtractor{mimes: []string{mime}})
	}, "a duplicate claim makes behavior depend on import order and must fail loudly at init")
}
