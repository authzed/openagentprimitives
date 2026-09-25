package deplogs

import (
	"bytes"
	"testing"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeSchema is a throwaway two-definition schema. Its only job is to make
// SpiceDB's compiler walk definition nodes, which is where the trace line
// lives; it deliberately shares no names with pkg/authz/spicedb/schema's scaffold so a
// scaffold edit can never change what this test observes.
const probeSchema = `definition probe_user {}

definition probe_doc {
	relation viewer: probe_user
	permission view = viewer
}`

// compileProbeSchema runs the dependency whose logging is under test. Asserting
// against the real compiler — rather than a hand-written zlog.Trace() standing
// in for it — is what makes this test evidence that the fleet's logs are clean,
// not just that a level was set.
func compileProbeSchema(t *testing.T) {
	t.Helper()
	_, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("deplogs-probe"),
		SchemaString: probeSchema,
	}, compiler.AllowUnprefixedObjectType())
	require.NoError(t, err, "probe schema must compile")
}

// captureGlobalZerolog redirects zerolog's package-global logger — the one
// SpiceDB's compiler writes to — into a buffer at trace level, restoring the
// process's original state afterwards. Tests using it must not run in
// parallel: they mutate package-global state.
func captureGlobalZerolog(t *testing.T) *bytes.Buffer {
	t.Helper()
	origLevel := zerolog.GlobalLevel()
	origLogger := zlog.Logger
	t.Cleanup(func() {
		zerolog.SetGlobalLevel(origLevel)
		zlog.Logger = origLogger
	})

	var buf bytes.Buffer
	zlog.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	return &buf
}

// SpiceDB v1.54 uses its own disabled-by-default logger. Verify that schema
// compilation stays silent through zerolog's global logger before and after
// this package disables that global logger for other dependencies.
func TestSpiceDBSchemaCompilerDoesNotWriteGlobalTrace(t *testing.T) {
	buf := captureGlobalZerolog(t)

	compileProbeSchema(t)
	require.Empty(t, buf.String(), "SpiceDB must not write through zerolog's global logger")

	buf.Reset()
	Silence()
	compileProbeSchema(t)

	assert.Empty(t, buf.String(),
		"dependency zerolog output must not reach the terminal or container logs")
}

// TestSilenceIsIdempotent: calling Silence twice leaves the dependency muted.
// Every binary calls it from main() while the package init() has already run,
// so the second call is the normal case, not an edge one.
func TestSilenceIsIdempotent(t *testing.T) {
	buf := captureGlobalZerolog(t)

	Silence()
	Silence()
	compileProbeSchema(t)

	assert.Empty(t, buf.String(), "a repeated Silence() must not re-enable dependency logging")
}
