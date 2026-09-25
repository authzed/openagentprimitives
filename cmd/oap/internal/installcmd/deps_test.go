package installcmd

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func deployWithArgs(args ...string) *unstructured.Unstructured {
	a := make([]any, len(args))
	for i, s := range args {
		a[i] = s
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind": "Deployment",
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "c", "args": a}},
		}}},
	}}
}

func containerArgs(t *testing.T, d *unstructured.Unstructured) []string {
	t.Helper()
	cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	return anyToStrings(cs[0].(map[string]any)["args"].([]any))
}

func TestDisableLeaderElection(t *testing.T) {
	// a controller using --leader-election-namespace gets --leader-elect=false
	d := deployWithArgs("--v=2", "--leader-election-namespace=kube-system")
	require.NoError(t, disableLeaderElection(d))
	assert.Contains(t, containerArgs(t, d), "--leader-elect=false")

	// idempotent — re-running doesn't duplicate
	require.NoError(t, disableLeaderElection(d))
	n := 0
	for _, a := range containerArgs(t, d) {
		if a == "--leader-elect=false" {
			n++
		}
	}
	assert.Equal(t, 1, n, "no duplicate --leader-elect=false")

	// a container without leader-election is untouched
	d2 := deployWithArgs("--v=2")
	require.NoError(t, disableLeaderElection(d2))
	assert.NotContains(t, containerArgs(t, d2), "--leader-elect=false")

	// non-Deployment is a safe no-op
	require.NoError(t, disableLeaderElection(&unstructured.Unstructured{Object: map[string]any{"kind": "ConfigMap"}}))
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		assumeYes bool
		isTTY     bool
		want      bool
	}{
		{"assume-yes short-circuits", "", true, false, true},
		{"non-tty defaults no", "", false, false, false},
		{"tty yes", "y\n", false, true, true},
		{"tty Y", "Y\n", false, true, true},
		{"tty no (empty)", "\n", false, true, false},
		{"tty n", "n\n", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := apcmd.Confirm(strings.NewReader(tc.input), &out, "Install foo?", tc.assumeYes, tc.isTTY)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTerminalFile covers the gate that decides whether echo can be suppressed
// at all. The *os.File-but-not-a-terminal row is the one that matters: stdin
// redirected from a file is still an *os.File, and handing that descriptor to
// term.ReadPassword fails with ENOTTY instead of reading the line a scripted
// run expects — so the type assertion alone is not enough.
func TestTerminalFile(t *testing.T) {
	t.Run("non-*os.File reader: no terminal", func(t *testing.T) {
		assert.Nil(t, terminalFile(strings.NewReader("x\n")))
	})

	t.Run("*os.File that is a regular file: no terminal", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "stdin")
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		assert.Nil(t, terminalFile(f))
	})

	t.Run("*os.File that is a pipe: no terminal", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		assert.Nil(t, terminalFile(r))
	})
}

// TestReadSecretLine_WithoutATerminal pins the degraded path: with no terminal
// to suppress echo on, the value must still be read (a scripted or redirected
// run has to keep working), it must be reported as NOT suppressed so the
// caller can warn, and it must come off the scanner the caller was already
// using rather than a second reader that would strand buffered input.
func TestReadSecretLine_WithoutATerminal(t *testing.T) {
	in := strings.NewReader("endpoint.example:443\n  spicedb-psk  \nleftover\n")
	sc := bufio.NewScanner(in)

	require.True(t, sc.Scan(), "the caller reads the first line itself")
	require.Equal(t, "endpoint.example:443", sc.Text())

	var out bytes.Buffer
	got, suppressed := readSecretLine(in, sc, &out, "SpiceDB bearer token: ")

	assert.Equal(t, "spicedb-psk", got, "value is read and trimmed")
	assert.False(t, suppressed, "there was no terminal, so echo was not suppressed")
	assert.Contains(t, out.String(), "SpiceDB bearer token: ", "the prompt is still shown")
	assert.NotContains(t, out.String(), "spicedb-psk", "the value is never written to out")

	require.True(t, sc.Scan(), "the shared scanner is still positioned for the caller")
	assert.Equal(t, "leftover", sc.Text())
}

// TestReadSecretLine_WithoutATerminalAtEOF covers the exhausted-input case:
// no value, and still reported as unsuppressed rather than pretending the
// empty string was safely read.
func TestReadSecretLine_WithoutATerminalAtEOF(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader(""))
	var out bytes.Buffer
	got, suppressed := readSecretLine(strings.NewReader(""), sc, &out, "token: ")
	assert.Empty(t, got)
	assert.False(t, suppressed)
}
