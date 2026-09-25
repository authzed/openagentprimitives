//go:build e2e

package threadrun

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// classTool is the two-field shape every case below varies.
func classTool(name string, command ...string) spiceboxv1alpha1.SpiceboxTool {
	return spiceboxv1alpha1.SpiceboxTool{Name: name, Command: command}
}

// constant wraps one recorded result per tool into the one-element sequences
// sandboxResponder serves, so a case that is not about per-call variation reads
// the way it did before sequences existed.
func constant(m map[string]bt.SandboxOutput) map[string][]bt.SandboxOutput {
	out := make(map[string][]bt.SandboxOutput, len(m))
	for k, v := range m {
		out[k] = []bt.SandboxOutput{v}
	}
	return out
}

func TestSandboxResponder_DispatchesOnArgv(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{
		classTool("gh", "/usr/bin/gh"),
		classTool("sre", "/usr/local/bin/sre-tool"),
	}
	outs := map[string]bt.SandboxOutput{
		"gh":  {Stdout: "no open pull requests\n"},
		"sre": {Stdout: "cluster-a\n", Stderr: "warn\n", ExitCode: 2},
	}
	respond := sandboxResponder("demo-bundle", tools, constant(outs))

	cases := []struct {
		name     string
		argv     []string
		wantOut  string
		wantErrS string // substring the responder's error must carry; "" means no error
		wantCode int32
		wantErrO string // substring of stderr
	}{
		{
			name:    "the first tool's argv reaches ITS recording, not the other's",
			argv:    []string{"/usr/bin/gh", "pr", "view", "123"},
			wantOut: "no open pull requests\n",
		},
		{
			name:     "a second tool at the same pod key gets its OWN recording, stderr and exit code",
			argv:     []string{"/usr/local/bin/sre-tool", "list-clusters"},
			wantOut:  "cluster-a\n",
			wantErrO: "warn\n",
			wantCode: 2,
		},
		{
			name:     "an argv matching no catalog entry fails, naming what WAS available",
			argv:     []string{"/bin/sh", "-c", "echo hi"},
			wantErrS: "matches no tool in the SpiceboxClass catalog",
		},
		{
			name:     "an empty argv fails rather than matching the first zero-length prefix",
			argv:     nil,
			wantErrS: "matches no tool in the SpiceboxClass catalog",
		},
		{
			name:     "an argv that is a PREFIX of a catalog command does not match it",
			argv:     []string{"/usr/bin"},
			wantErrS: "matches no tool in the SpiceboxClass catalog",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := respond(exec.Request{Command: tc.argv})
			if tc.wantErrS != "" {
				require.Error(t, got.Err, "the responder must refuse an argv it cannot answer")
				assert.Contains(t, got.Err.Error(), tc.wantErrS)
				assert.EqualValues(t, -1, got.ExitCode, "a refusal is a transport failure")
				return
			}
			require.NoError(t, got.Err, "argv %v", tc.argv)
			assert.Equal(t, tc.wantOut, string(got.Stdout), "stdout")
			assert.Equal(t, tc.wantErrO, string(got.Stderr), "stderr")
			assert.Equal(t, tc.wantCode, got.ExitCode, "exit code")
		})
	}
}

// A catalog tool the bundle recorded nothing for must be REFUSED by name.
// Returning an empty success instead is the silent divergence the whole
// mechanism exists to make impossible: the model would be handed a tool that
// "ran" and printed nothing, several steps before anything looked wrong.
func TestSandboxResponder_RecognizedToolWithNoRecordingIsRefusedByName(t *testing.T) {
	respond := sandboxResponder("demo-bundle",
		[]spiceboxv1alpha1.SpiceboxTool{classTool("gh", "/usr/bin/gh")},
		nil, // nothing recorded
	)

	got := respond(exec.Request{Command: []string{"/usr/bin/gh", "pr", "view"}})
	require.Error(t, got.Err)
	assert.Contains(t, got.Err.Error(), `sandbox tool "gh"`, "the error names the TOOL")
	assert.Contains(t, got.Err.Error(), "/usr/bin/gh pr view", "the error names the argv")
	assert.NotContains(t, got.Err.Error(), "matches no tool",
		"a KNOWN tool with no recording is a different failure from an unknown argv")
}

// Longest match wins. Two catalog entries can share a leading element, and the
// more specific one is the entry that meant it — first-match-wins would route
// every `sh -c` call to the bare `sh` tool.
func TestSandboxResponder_LongestCommandPrefixWins(t *testing.T) {
	respond := sandboxResponder("demo-bundle",
		[]spiceboxv1alpha1.SpiceboxTool{
			classTool("shell", "/bin/sh"),
			classTool("script", "/bin/sh", "-c"),
		},
		constant(map[string]bt.SandboxOutput{
			"shell":  {Stdout: "from shell"},
			"script": {Stdout: "from script"},
		}),
	)

	assert.Equal(t, "from script",
		string(respond(exec.Request{Command: []string{"/bin/sh", "-c", "echo"}}).Stdout),
		"the two-element command is the more specific match")
	assert.Equal(t, "from shell",
		string(respond(exec.Request{Command: []string{"/bin/sh", "script.sh"}}).Stdout),
		"an argv the longer command does not prefix falls to the shorter one")
}

// A catalog entry with no command can never match an argv, and must not swallow
// every call by matching the empty prefix.
func TestSandboxResponder_CommandlessCatalogEntryMatchesNothing(t *testing.T) {
	respond := sandboxResponder("demo-bundle",
		[]spiceboxv1alpha1.SpiceboxTool{classTool("broken")},
		constant(map[string]bt.SandboxOutput{"broken": {Stdout: "should never be served"}}),
	)

	got := respond(exec.Request{Command: []string{"/usr/bin/gh"}})
	require.Error(t, got.Err, "a commandless catalog entry must not match")
	assert.Contains(t, got.Err.Error(), "matches no tool in the SpiceboxClass catalog")
}

// ---- reading the fixture's tool catalog ----------------------------------

// writeFixture materializes a one-file agentDir plus the bundle that names it.
func writeFixture(t *testing.T, yaml string) (dir string, b bt.Bundle) {
	t.Helper()
	root := t.TempDir()
	agentDir := filepath.Join(root, "manifests")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "00-agent.yaml"), []byte(yaml), 0o644))
	return root, bt.Bundle{AgentDir: agentDir}
}

// A class tool reached by TWO toolBundles is the shape the sre session has and
// the reason both spellings exist: one SpiceboxClass is one container image, and
// several toolBundles narrow the same binary differently.
func TestSandboxResponder_PrefersTheBundleQualifiedKey(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{classTool("sre", "/usr/local/bin/sre-tool")}
	outs := map[string]bt.SandboxOutput{
		"sre-discovery_sre": {Stdout: "the clusters\n"},
		"sre-fetch_sre":     {Stdout: "the kubeconfig\n"},
		"sre":               {Stdout: "the bare fallback\n"},
	}
	argv := exec.Request{Command: []string{"/usr/local/bin/sre-tool", "go"}}

	assert.Equal(t, "the clusters\n",
		string(sandboxResponder("sre-discovery", tools, constant(outs))(argv).Stdout),
		"each bundle's pod must get its OWN recording, not whichever one a bare key held")
	assert.Equal(t, "the kubeconfig\n",
		string(sandboxResponder("sre-fetch", tools, constant(outs))(argv).Stdout))
}

// The bare spelling is what a hand-authored fixture says when only one bundle
// reaches the tool, and it has to keep working.
func TestSandboxResponder_FallsBackToTheBareClassToolKey(t *testing.T) {
	respond := sandboxResponder("gitlike",
		[]spiceboxv1alpha1.SpiceboxTool{classTool("gh", "/usr/bin/gh")},
		constant(map[string]bt.SandboxOutput{"gh": {Stdout: "no open pull requests\n"}}))

	assert.Equal(t, "no open pull requests\n",
		string(respond(exec.Request{Command: []string{"/usr/bin/gh", "pr", "view"}}).Stdout))
}

// A pod serving no named bundle (nothing stamped the label) still resolves the
// bare key rather than refusing everything.
func TestSandboxResponder_AnUnlabelledPodStillResolvesTheBareKey(t *testing.T) {
	respond := sandboxResponder("",
		[]spiceboxv1alpha1.SpiceboxTool{classTool("gh", "/usr/bin/gh")},
		constant(map[string]bt.SandboxOutput{"gh": {Stdout: "out\n"}}))

	assert.Equal(t, "out\n", string(respond(exec.Request{Command: []string{"/usr/bin/gh"}}).Stdout))
}

const demoSandboxFixture = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata: {name: cls-demo, namespace: default}
spec:
  image: registry.example.com/demo:latest
  tools:
    - {name: demo-cli, command: ["/usr/bin/demo"]}
    - {name: other-cli, command: ["/usr/bin/other"]}
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata: {name: demo-agent, namespace: default}
spec:
  displayName: Demo
  toolBundles:
    - {name: demo-a, class: cls-demo, toolspecs: [ts-demo]}
    - {name: demo-b, class: cls-demo, toolspecs: [ts-other]}
`

func TestSandboxToolNames_ReadsBothSpellingsFromTheFixture(t *testing.T) {
	dir, b := writeFixture(t, demoSandboxFixture)

	assert.Equal(t, map[string]bool{
		"demo-cli":         true,
		"other-cli":        true,
		"demo-a_demo-cli":  true,
		"demo-a_other-cli": true,
		"demo-b_demo-cli":  true,
		"demo-b_other-cli": true,
	}, sandboxToolNames(t, dir, b),
		"a captured bundle keys sandbox outputs by the LLM-facing <bundle>_<tool>, so a splitter "+
			"that knew only the bare names would hand every captured sandbox entry to the MCP stub")
}

func TestSandboxToolNames_EmptyForAnMCPOnlyFixture(t *testing.T) {
	dir, b := writeFixture(t, `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata: {name: demo-agent, namespace: default}
spec:
  displayName: Demo
`)

	assert.Empty(t, sandboxToolNames(t, dir, b),
		"a fixture with no SpiceboxClass declares no sandbox tool, and the whole sandbox path stays off")
}

func TestSandboxOutputs_SplitsByNameAgainstTheCatalog(t *testing.T) {
	names := map[string]bool{"demo-cli": true}
	b := bt.Bundle{ToolOutputs: map[string]json.RawMessage{
		"demo-cli":       json.RawMessage(`{"stdout": "hello\n"}`),
		"list_companies": json.RawMessage(`{"companies": ["a"]}`),
	}}

	got := sandboxOutputs(t, b, names)
	assert.Equal(t, map[string][]bt.SandboxOutput{"demo-cli": {{Stdout: "hello\n"}}}, got,
		"only the entry naming a class tool is a sandbox output; the MCP entry stays for the stub")
}

// A sandbox tool whose calls did not all return the same bytes is an ordinary
// captured session — a `git` bundle whose first two calls succeeded and whose
// third exited non-zero — and the sequence is what makes the failure replay at
// the call it happened on rather than at the first. Refusing it outright is what
// this used to do.
func TestSandboxOutputs_ReadsAPerCallSequence(t *testing.T) {
	b := bt.Bundle{ToolOutputSequence: map[string][]json.RawMessage{
		"gitlike_git": {
			json.RawMessage(`{"stdout": "on branch main\n"}`),
			json.RawMessage(`{"stdout": "", "stderr": "fatal: no upstream\n", "exitCode": 128}`),
		},
		"list_companies": {json.RawMessage(`{"companies": ["a"]}`)},
	}}

	got := sandboxOutputs(t, b, map[string]bool{"gitlike_git": true})
	assert.Equal(t, map[string][]bt.SandboxOutput{"gitlike_git": {
		{Stdout: "on branch main\n"},
		{Stderr: "fatal: no upstream\n", ExitCode: 128},
	}}, got, "the MCP tool's sequence stays for the stub, in the order it was recorded")
}

// The sequence is served in CALL order, one entry per call, and clamps at the
// last rather than wrapping or panicking — an overrun means the replay already
// diverged, and the step's own Expect reports that with an index.
func TestSandboxResponder_ServesSuccessiveCallsTheSuccessiveRecordings(t *testing.T) {
	respond := sandboxResponder("gitlike",
		[]spiceboxv1alpha1.SpiceboxTool{classTool("git", "/usr/bin/git")},
		map[string][]bt.SandboxOutput{"gitlike_git": {
			{Stdout: "on branch main\n"},
			{Stdout: "nothing to commit\n"},
			{Stderr: "fatal: no upstream\n", ExitCode: 128},
		}})
	argv := exec.Request{Command: []string{"/usr/bin/git", "status"}}

	first := respond(argv)
	require.NoError(t, first.Err)
	assert.Equal(t, "on branch main\n", string(first.Stdout))
	assert.EqualValues(t, 0, first.ExitCode)

	assert.Equal(t, "nothing to commit\n", string(respond(argv).Stdout))

	third := respond(argv)
	assert.EqualValues(t, 128, third.ExitCode,
		"a captured failure must replay as a failure at the call it happened on")
	assert.Equal(t, "fatal: no upstream\n", string(third.Stderr))

	assert.EqualValues(t, 128, respond(argv).ExitCode,
		"an overrun clamps at the last recording; the step's Expect is what reports the divergence")
}

// Two tools sharing one pod each advance their OWN cursor. A single counter
// would hand the second tool's first call the first tool's second recording.
func TestSandboxResponder_EachToolAdvancesItsOwnCursor(t *testing.T) {
	respond := sandboxResponder("demo",
		[]spiceboxv1alpha1.SpiceboxTool{
			classTool("git", "/usr/bin/git"),
			classTool("gh", "/usr/bin/gh"),
		},
		map[string][]bt.SandboxOutput{
			"demo_git": {{Stdout: "git-1"}, {Stdout: "git-2"}},
			"demo_gh":  {{Stdout: "gh-1"}, {Stdout: "gh-2"}},
		})

	git := exec.Request{Command: []string{"/usr/bin/git", "status"}}
	gh := exec.Request{Command: []string{"/usr/bin/gh", "pr", "list"}}

	assert.Equal(t, "git-1", string(respond(git).Stdout))
	assert.Equal(t, "gh-1", string(respond(gh).Stdout))
	assert.Equal(t, "git-2", string(respond(git).Stdout))
	assert.Equal(t, "gh-2", string(respond(gh).Stdout))
}

// The split is by NAME, never by the value's shape. An MCP tool is free to
// return an object with a "stdout" key, and reading that as a sandbox result
// would silently stop serving it to the MCP stub.
func TestSandboxOutputs_AnMCPToolReturningStdoutIsNotASandboxOutput(t *testing.T) {
	b := bt.Bundle{ToolOutputs: map[string]json.RawMessage{
		"run_query": json.RawMessage(`{"stdout": "rows"}`),
	}}

	assert.Empty(t, sandboxOutputs(t, b, map[string]bool{"demo-cli": true}),
		"a name the class catalog does not declare is an MCP result whatever it looks like")
}

func TestSandboxOutputs_NilWhenTheFixtureDeclaresNoSandboxTool(t *testing.T) {
	b := bt.Bundle{ToolOutputs: map[string]json.RawMessage{"x": json.RawMessage(`{}`)}}
	assert.Nil(t, sandboxOutputs(t, b, nil))
}

// A recorded STREAMING result is answered by the binder's OTHER half: the
// toolkit runs as a stream, so the call never reaches Exec at all. The
// responder writes the recorded text to the toolkit's stdout and exits 0 —
// nothing about the toolkit's own dialect is synthesized, and the sandbox
// tool's no-terminal-result fallback is what turns it back into a result.
func TestStreamResponder_WritesTheRecordedResultAndExitsZero(t *testing.T) {
	const recorded = "status: success (7.272s, $0.09)\nI reviewed the file and found no typos."
	var stdout, stderr bytes.Buffer

	code := streamResponder([]string{recorded})(strings.NewReader(""), &stdout, &stderr)

	assert.Equal(t, int32(0), code,
		"a recorded stream result never stands for a process that failed; SplitStreamResult refuses those")
	assert.Equal(t, recorded, stdout.String(), "the recorded text is served verbatim")
	assert.Empty(t, stderr.String())
}

// Successive calls get the successive recordings, clamped at the last — the
// same cursor rule sandboxResponder follows, and for the same reason: an
// overrun means the replay already diverged, and the step's own Expect reports
// that with an index.
func TestStreamResponder_ServesSuccessiveCallsTheSuccessiveRecordings(t *testing.T) {
	respond := streamResponder([]string{"status: success (exit 0)\nfirst", "status: success (exit 0)\nsecond"})

	var got []string
	for range 3 {
		var stdout bytes.Buffer
		respond(strings.NewReader(""), &stdout, io.Discard)
		got = append(got, stdout.String())
	}
	assert.Equal(t, []string{
		"status: success (exit 0)\nfirst",
		"status: success (exit 0)\nsecond",
		"status: success (exit 0)\nsecond",
	}, got, "clamped at the last recording rather than wrapping or panicking")
}

// One toolBundle is one sandbox pod, and the binder's streaming half is keyed
// by pod with no argv to dispatch on. Two recorded streams on one pod would
// answer both tools with whichever was registered, and the bundle would replay
// green with one tool served the other's output.
func TestStreamResultsFor_RefusesTwoStreamsInOnePod(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{classTool("claude"), classTool("codex")}
	outs := constant(map[string]bt.SandboxOutput{
		"codelike_claude": {StreamResult: "status: success (exit 0)\na"},
		"codelike_codex":  {StreamResult: "status: success (exit 0)\nb"},
	})

	_, _, err := streamResultsFor("codelike", tools, outs)
	require.Error(t, err, "the pod cannot tell the two apart, so the replay must not start")
	assert.Contains(t, err.Error(), "claude")
	assert.Contains(t, err.Error(), "codex")
}

// The negative control the refusal above needs: ONE recorded stream resolves,
// and the ordinary process recordings beside it are left to sandboxResponder.
func TestStreamResultsFor_ResolvesTheOneRecordedStream(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{classTool("claude"), classTool("gh")}
	outs := constant(map[string]bt.SandboxOutput{
		"codelike_claude": {StreamResult: "status: success (exit 0)\na"},
		"codelike_gh":     {Stdout: "no open pull requests\n"},
	})

	tool, results, err := streamResultsFor("codelike", tools, outs)
	require.NoError(t, err)
	assert.Equal(t, "claude", tool)
	assert.Equal(t, []string{"status: success (exit 0)\na"}, results)
}

// A fixture whose sandbox tools are all ordinary processes registers no stream
// program at all, which is every bundle in the corpus but the captured one.
func TestStreamResultsFor_EmptyWhenNothingRecordedAStream(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{classTool("gh")}
	outs := constant(map[string]bt.SandboxOutput{"codelike_gh": {Stdout: "out\n"}})

	tool, results, err := streamResultsFor("codelike", tools, outs)
	require.NoError(t, err)
	assert.Empty(t, tool)
	assert.Empty(t, results)
}

// The mode divergence the process path must not paper over. A tool whose
// recording came from a STREAM reaching Exec means the replayed tool is no
// longer streaming — a toolspec that stopped narrowing to the streaming
// subcommand, or a toolkit revision that dropped it. Serving the text as stdout
// would hide that behind a green step, because the composed result still
// contains the bytes the step's assertion was derived from.
func TestSandboxResponder_AStreamRecordingReachingTheProcessPathIsRefused(t *testing.T) {
	tools := []spiceboxv1alpha1.SpiceboxTool{classTool("claude", "/usr/bin/claude")}
	outs := constant(map[string]bt.SandboxOutput{
		"codelike_claude": {StreamResult: "status: success (exit 0)\nall done"},
	})

	resp := sandboxResponder("codelike", tools, outs)(exec.Request{Command: []string{"/usr/bin/claude", "-p"}})

	require.Error(t, resp.Err)
	assert.Contains(t, resp.Err.Error(), "STREAMING")
	assert.Contains(t, resp.Err.Error(), "claude")
	assert.Empty(t, resp.Stdout, "serving the recorded text here would satisfy the very assertion meant to catch this")
}

// recordsAStream is what gates the one precondition Run checks on the test
// goroutine: a bundle cannot both drive a scripted streaming toolkit (wired by
// name) and record a stream of its own, because both register on the same pod
// key and one would silently overwrite the other.
func TestRecordsAStream(t *testing.T) {
	assert.False(t, recordsAStream(constant(map[string]bt.SandboxOutput{"gh": {Stdout: "out\n"}})))
	assert.True(t, recordsAStream(constant(map[string]bt.SandboxOutput{
		"gh":              {Stdout: "out\n"},
		"codelike_claude": {StreamResult: "status: success (exit 0)\na"},
	})))
	assert.False(t, recordsAStream(nil))
}
