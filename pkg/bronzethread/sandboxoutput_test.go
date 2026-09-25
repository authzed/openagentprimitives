package bronzethread_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

func TestDecodeSandboxOutput(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    bt.SandboxOutput
		wantErr string
	}{
		{
			name: "stdout only: the shape the sandbox fixture already carried",
			raw:  `{"stdout": "no open pull requests"}`,
			want: bt.SandboxOutput{Stdout: "no open pull requests"},
		},
		{
			name: "all three fields: a captured FAILURE replays as a failure",
			raw:  `{"stdout": "partial\n", "stderr": "boom\n", "exitCode": 3}`,
			want: bt.SandboxOutput{Stdout: "partial\n", Stderr: "boom\n", ExitCode: 3},
		},
		{
			name: "empty object: a tool that printed nothing and succeeded",
			raw:  `{}`,
			want: bt.SandboxOutput{},
		},
		{
			name:    "a typo'd key is REFUSED, not silently zeroed",
			raw:     `{"stdout_": "hello"}`,
			wantErr: "stdout_",
		},
		{
			// encoding/json matches field names case-insensitively, so this is
			// accepted rather than refused. Pinned because a reader looking at
			// DisallowUnknownFields would reasonably expect the opposite, and
			// because it is the one "typo" that is safe: it lands on the field
			// it meant.
			name: "a differently-cased key still lands on the field it meant",
			raw:  `{"stdOut": "hello"}`,
			want: bt.SandboxOutput{Stdout: "hello"},
		},
		{
			name:    "an MCP-shaped result is refused rather than read as empty stdout",
			raw:     `{"companies": ["a", "b"]}`,
			wantErr: "companies",
		},
		{
			name:    "a JSON array is not a sandbox output",
			raw:     `["a"]`,
			wantErr: "cannot unmarshal",
		},
		{
			name: "streamResult alone: a toolkit stream, served as the toolkit's own output",
			raw:  `{"streamResult": "status: success (1s, $0.01)\nall done"}`,
			want: bt.SandboxOutput{StreamResult: "status: success (1s, $0.01)\nall done"},
		},
		{
			// A call is answered by a process or by a toolkit stream, and the
			// two are served by DIFFERENT halves of the fake exec binder, so an
			// entry carrying both leaves which one wins to the tool's mode
			// rather than to the bundle.
			name:    "streamResult beside stdout is refused, not silently preferred",
			raw:     `{"stdout": "out", "streamResult": "status: success (1s, $0.01)\nall done"}`,
			wantErr: "never by both",
		},
		{
			name:    "streamResult beside a non-zero exitCode is refused too",
			raw:     `{"exitCode": 3, "streamResult": "status: success (1s, $0.01)\nall done"}`,
			wantErr: "never by both",
		},
		{
			name:    "streamResult beside stderr is refused too",
			raw:     `{"stderr": "boom", "streamResult": "status: success (1s, $0.01)\nall done"}`,
			wantErr: "never by both",
		},
		{
			// The shape a capture actually emits: Stdout carries no omitempty,
			// so an empty one rides along beside every streamResult. It must
			// not read as the forbidden pairing.
			name: "an EMPTY stdout beside streamResult is the ordinary emitted shape",
			raw:  `{"stdout": "", "streamResult": "status: success (1s, $0.01)\nall done"}`,
			want: bt.SandboxOutput{StreamResult: "status: success (1s, $0.01)\nall done"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bt.DecodeSandboxOutput(json.RawMessage(tc.raw))
			if tc.wantErr != "" {
				require.Error(t, err, "decoding %s must fail", tc.raw)
				assert.Contains(t, err.Error(), tc.wantErr, "the error names what was wrong")
				return
			}
			require.NoError(t, err, "decoding %s", tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSplitSandboxResult(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantOut  string
		wantCode int32
		wantOK   bool
	}{
		{
			name:     "the ordinary success shape: stdout plus the artifacts trailer",
			content:  "cluster-a\ncluster-b\n[exit=0; artifacts: stdout=ns/sess/uid/stdout, stderr=ns/sess/uid/stderr]\n",
			wantOut:  "cluster-a\ncluster-b\n",
			wantCode: 0,
			wantOK:   true,
		},
		{
			name:     "stdout that did NOT end in a newline still round-trips exactly once",
			content:  "no open pull requests\n[exit=0; artifacts: stdout=a, stderr=b]\n",
			wantOut:  "no open pull requests\n",
			wantCode: 0,
			wantOK:   true,
		},
		{
			name:     "stdout containing its OWN line starting [exit= anchors on the LAST line",
			content:  "[exit=99; not the trailer]\nreal output\n[exit=0; artifacts: stdout=a, stderr=b]\n",
			wantOut:  "[exit=99; not the trailer]\nreal output\n",
			wantCode: 0,
			wantOK:   true,
		},
		{
			name:    "a FAILED call is not this shape and must be refused, not parsed",
			content: "ToolCall failed: NonZeroExit — exit code 3\nstderr (last 4096 bytes):\nboom\n",
			wantOK:  false,
		},
		{
			name:    "a secret-output diversion is not this shape",
			content: "wrote the kubeconfig\n<secret-output name=\"kubeconfig\" ref=\"h1\" bytes=42>",
			wantOK:  false,
		},
		{
			name:    "an MCP JSON result is not this shape",
			content: `{"companies":["a"]}`,
			wantOK:  false,
		},
		{
			name:    "a trailer with an unparseable exit code is refused rather than defaulted to 0",
			content: "out\n[exit=abc; artifacts: stdout=a, stderr=b]\n",
			wantOK:  false,
		},
		{
			// The "[exit=" prefix is what makes this a trailer. Without that
			// check, ordinary stdout whose last line happens to start with a
			// number and contain a semicolon parses as a trailer, and the line
			// is silently deleted from the recovered stdout.
			name:    "stdout ending in a numeric semicolon line is NOT a trailer",
			content: "out\n0; some tool's own last line]\n",
			wantOK:  false,
		},
		{
			// And the closing bracket is what makes it COMPLETE. A truncated
			// trailer means the result was cut off, which is not the same thing
			// as a clean run, and reading it as one loses that.
			name:    "a trailer missing its closing bracket is refused",
			content: "out\n[exit=0; artifacts: stdout=a, stderr=b\n",
			wantOK:  false,
		},
		{
			name:    "a single line with no preceding stdout is refused: there is nothing to recover",
			content: "[exit=0; artifacts: stdout=a, stderr=b]\n",
			wantOK:  false,
		},
		{
			name:    "empty content",
			content: "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code, ok := bt.SplitSandboxResult(tc.content)
			assert.Equal(t, tc.wantOK, ok, "recognized as a sandbox success result")
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantOut, out, "recovered stdout")
			assert.Equal(t, tc.wantCode, code, "recovered exit code")
		})
	}
}

// The recovered stdout must always end in a newline, because that is what makes
// the round trip exact: the composer appends one only when stdout lacks it, so
// a recovered value that ended without one would gain a newline the original
// result did not have.
func TestSplitSandboxResult_RecoveredStdoutAlwaysEndsInNewline(t *testing.T) {
	out, _, ok := bt.SplitSandboxResult("x\n[exit=0; artifacts: stdout=a, stderr=b]\n")
	require.True(t, ok)
	assert.Equal(t, "x\n", out, "the newline before the trailer belongs to the recovered stdout")
}

// TestSplitStreamResult covers what a STREAMING toolkit's composed result is
// recognized as, and — the half that matters — what it is refused for.
//
// Every refusal below is a case where serving the text back would replay as
// something other than what was recorded, which is the divergence this whole
// package exists to keep out of a bundle.
func TestSplitStreamResult(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantOK  bool
	}{
		{
			name:    "the parsed shape: a status header carrying a duration and a cost",
			content: "status: success (7.272s, $0.09)\nI reviewed the file and found no typos.",
			wantOK:  true,
		},
		{
			// The toolkit declared no stream parser, or its parser recognized
			// no terminal result. Still a streaming composition, and still the
			// only recording the call left.
			content: "status: success (exit 0)\nraw stdout tail",
			name:    "the unparsed shape: a status header carrying the process exit",
			wantOK:  true,
		},
		{
			// composeStreamResult writes the header alone when the toolkit
			// produced no text. Served back it becomes its own payload, so the
			// derived assertion still matches.
			name:    "a header with no payload at all is still servable",
			content: "status: success (exit 0)",
			wantOK:  true,
		},
		{
			// OUR watchdog ended the run, but the result is neither an error
			// nor terminal, so the replay reproduces both of those.
			name:    "a maxDuration exit is an ordinary non-terminal result",
			content: "status: maxDuration (exit 0)\npartial work",
			wantOK:  true,
		},
		{
			// Terminal=true parks the session and ends the turn. Served back as
			// an ordinary result the replay runs on past where the recording
			// stopped.
			name:    "an idle exit is REFUSED: it carries Terminal and would not end the turn",
			content: "status: idle (exit 0)\nwaiting",
			wantOK:  false,
		},
		{
			// Reachable on a cleanly-exited process whose parsed outcome said
			// the WORK failed. Served back as a success it would reverse what
			// the bundle is evidence of.
			name:    "a failed status is REFUSED even though the caller should never offer one",
			content: "status: failed (1.5s, $0.02)\nthe job did not finish",
			wantOK:  false,
		},
		{
			name:    "over the stdout-tail budget is REFUSED: the replay would truncate its head",
			content: "status: success (exit 0)\n" + strings.Repeat("x", bt.StreamResultBudgetBytes),
			wantOK:  false,
		},
		{
			name:    "exactly at the budget is accepted",
			content: "status: success (exit 0)\n" + strings.Repeat("x", bt.StreamResultBudgetBytes-25),
			wantOK:  true,
		},
		{
			name:    "an ordinary process composition is not a stream result",
			content: "no open pull requests\n[exit=0; artifacts: stdout=a, stderr=b]\n",
			wantOK:  false,
		},
		{
			name:    "a failure summary is not a stream result",
			content: "ToolCall failed: NonZeroExit — exit code 128\n",
			wantOK:  false,
		},
		{
			name:    "a header on any line but the first is not a header",
			content: "some prose\nstatus: success (exit 0)",
			wantOK:  false,
		},
		{
			name:    "an unclosed detail is not the header either format writes",
			content: "status: success (exit 0\nbody",
			wantOK:  false,
		},
		{
			name:    "an empty detail is neither format",
			content: "status: success ()\nbody",
			wantOK:  false,
		},
		{
			name:    "an unknown status word is refused rather than guessed at",
			content: "status: cancelled (exit 0)\nbody",
			wantOK:  false,
		},
		{
			name:    "the empty string is not a stream result",
			content: "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bt.SplitStreamResult(tc.content)
			assert.Equal(t, tc.wantOK, ok, "recognized as a servable streaming result")
			if !tc.wantOK {
				assert.Empty(t, got, "a refused result carries nothing forward")
				return
			}
			assert.Equal(t, tc.content, got,
				"the recorded text is carried WHOLE: the replay wraps it in a header of its own "+
					"rather than replacing this one, so every recorded byte stays assertable")
		})
	}
}
