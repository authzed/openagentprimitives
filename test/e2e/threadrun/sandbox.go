//go:build e2e

package threadrun

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
)

// A bundle replays black boxes: the recorded call goes in, the recorded result
// comes back, and the transport that produced it is not the bundle's business.
// This file is the sandbox half of that — the routing and the canned answers
// for a tool that runs as a process in a sandbox pod rather than as a JSON-RPC
// call to a server.
//
// Two facts shape all of it:
//
//   - The fake exec binder is keyed by POD ("<ns>/<pod>:<container>"), and every
//     tool a SpiceboxClass declares runs in that one pod. So a per-pod static
//     Response cannot answer two tools differently; fake.Binder.ProgramFunc is
//     what makes the answer depend on the argv, and this file owns the rule that
//     maps an argv onto a tool.
//   - The toolcall controller is what actually runs a sandbox call, so a bundle
//     with a recorded sandbox result needs e2e.Options.WithToolCallController.
//     That is decided BEFORE the harness starts, which is why the tool NAMES are
//     read from the fixture's YAML here rather than from the applied objects.

// sandboxToolNames returns the tool names the bundle's fixture declares in a
// SpiceboxClass tool catalog, read straight off the manifest files.
//
// Read pre-Start, from the files, because the two things it decides —
// whether to run the toolcall controller, and which ToolOutputs entries are
// sandbox rather than MCP — are both needed before e2e.Start has applied
// anything. Everything the RUN needs is read from the session's resolved status
// instead (see sandboxResponder), so this is a pair of booleans, not a second
// source of truth for dispatch.
//
// A fixture with no SpiceboxClass yields an empty set and every caller below
// becomes a no-op, which is every MCP-only bundle in the corpus.
func sandboxToolNames(t *testing.T, dir string, b bt.Bundle) map[string]bool {
	t.Helper()
	var classTools, bundles []string
	for _, raw := range fixtureManifests(t, dir, b) {
		for _, cls := range spiceboxClassesIn(t, raw) {
			for _, tl := range cls.Spec.Tools {
				if tl.Name != "" {
					classTools = append(classTools, tl.Name)
				}
			}
		}
		for _, ac := range agentClassesIn(t, raw) {
			for _, tb := range ac.Spec.ToolBundles {
				if tb.Name != "" {
					bundles = append(bundles, tb.Name)
				}
			}
		}
	}

	// BOTH spellings a ToolOutputs entry may use — see sandboxResponder's
	// bundleName parameter. The cross-product is deliberately not narrowed by
	// resolving which toolspec reaches which class tool: that resolution belongs
	// to the capture (pkg/steelthread.sandboxTools), duplicating it here would
	// be a second place to keep in agreement with sandbox.Synthesize, and the
	// only cost of over-accepting is that an MCP tool would have to be named
	// exactly "<a toolBundle>_<a class tool>" to be misrouted.
	out := make(map[string]bool, len(classTools)*(1+len(bundles)))
	for _, tool := range classTools {
		out[tool] = true
		for _, bundle := range bundles {
			out[bundle+"_"+tool] = true
		}
	}
	return out
}

// fixtureManifests returns the YAML blobs e2e.Start will apply: the AgentDir's
// *.yaml in the sorted order the 00-/01-/02- prefixes encode, then the bundle's
// extraManifests. Mirrors Harness.applyAgentDir plus loadManifests.
//
// The {{MCP_URL}} substitution applyAgentDir performs is deliberately NOT done:
// nothing read here looks at an MCPServer's url, and the harness has not been
// started, so there is no URL to substitute yet.
func fixtureManifests(t *testing.T, dir string, b bt.Bundle) []string {
	t.Helper()
	var out []string
	if b.AgentDir != "" {
		entries, err := os.ReadDir(b.AgentDir)
		require.NoError(t, err, "reading agentDir %q", b.AgentDir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(b.AgentDir, e.Name()))
			require.NoError(t, err, "reading %s", e.Name())
			out = append(out, string(raw))
		}
	}
	return append(out, loadManifests(t, dir, b)...)
}

// spiceboxClassesIn and agentClassesIn decode one kind out of a multi-doc YAML
// blob, typed rather than field-picked so what they read is the same shape the
// operator reads.
func spiceboxClassesIn(t *testing.T, blob string) []spiceboxv1alpha1.SpiceboxClass {
	t.Helper()
	return docsOfKind[spiceboxv1alpha1.SpiceboxClass](t, blob, "SpiceboxClass")
}

func agentClassesIn(t *testing.T, blob string) []spiceboxv1alpha1.AgentClass {
	t.Helper()
	return docsOfKind[spiceboxv1alpha1.AgentClass](t, blob, "AgentClass")
}

func docsOfKind[T any](t *testing.T, blob, kind string) []T {
	t.Helper()
	var out []T
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(blob), 4096)
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			break // EOF, or a document this pass has no opinion about
		}
		if k, _ := raw["kind"].(string); k != kind {
			continue
		}
		asJSON, err := json.Marshal(raw)
		require.NoError(t, err, "re-marshalling a %s document", kind)
		var obj T
		require.NoError(t, json.Unmarshal(asJSON, &obj), "decoding a %s document", kind)
		out = append(out, obj)
	}
	return out
}

// sandboxOutputs splits the bundle's recorded results into the sandbox entries,
// decoded, keyed by class-tool name — a one-element slice for a constant entry
// and the whole recorded run for a sequence.
//
// Split by NAME against the fixture's own catalog, never by guessing at the
// value's shape: an MCP tool is free to return an object with a "stdout" key,
// and reading that as a sandbox result would silently stop serving it to the
// MCP stub.
//
// BOTH maps, because a sandbox tool that varied per call is an ordinary session
// shape rather than an exotic one — a capture of a `git` bundle whose first two
// calls succeeded and whose third exited non-zero produces exactly this — and
// the sequence is what makes the failure replay as a failure at the call it
// happened on. It used to be refused outright, on the grounds that the sequence
// handler was an MCP-stub handler with no sandbox equivalent; sandboxResponder
// is that equivalent.
func sandboxOutputs(t *testing.T, b bt.Bundle, names map[string]bool) map[string][]bt.SandboxOutput {
	t.Helper()
	if len(names) == 0 {
		return nil
	}
	out := map[string][]bt.SandboxOutput{}
	for name, raw := range b.ToolOutputs {
		if !names[name] {
			continue
		}
		so, err := bt.DecodeSandboxOutput(raw)
		require.NoError(t, err, "toolOutputs[%q] names a SpiceboxClass tool, so it must be a sandbox output", name)
		out[name] = []bt.SandboxOutput{so}
	}
	// Bundle.ValidateToolOutputs already refuses a tool present in both maps, so
	// these cannot collide with the entries above.
	for name, seq := range b.ToolOutputSequence {
		if !names[name] {
			continue
		}
		vals := make([]bt.SandboxOutput, 0, len(seq))
		for i, raw := range seq {
			so, err := bt.DecodeSandboxOutput(raw)
			require.NoErrorf(t, err,
				"toolOutputSequence[%q][%d] names a SpiceboxClass tool, so it must be a sandbox output", name, i)
			vals = append(vals, so)
		}
		out[name] = vals
	}
	return out
}

// recordsAStream reports whether any decoded sandbox output is a recorded
// toolkit STREAM rather than process output.
func recordsAStream(outs map[string][]bt.SandboxOutput) bool {
	for _, seq := range outs {
		for _, so := range seq {
			if so.StreamResult != "" {
				return true
			}
		}
	}
	return false
}

// streamResultsFor returns the recorded STREAMING results ONE sandbox pod must
// serve, in call order, together with the class tool they belong to.
//
// Empty tool name when this pod's toolBundle recorded none, which is every
// bundle whose sandbox tools are ordinary processes.
//
// A pod serving two of them is refused here rather than replayed: the fake exec
// binder's streaming half is keyed by pod and hands its driver no argv (the
// request-inspecting responder that routes an ordinary sandbox call by command
// line has no streaming counterpart), so one registration would answer both
// tools and the run would go green with one served the other's output. The
// capture refuses the same shape up front (steelthread.CodeStreamResultCollision);
// this is the replay-side half of one rule, because a hand-authored bundle
// reaches here without passing through a capture at all.
//
// The refusal is returned rather than raised: the only caller runs on the
// SpiceboxSession stamping goroutine, where a t.Fatal's Goexit would abandon
// that loop instead of the test.
func streamResultsFor(
	bundleName string,
	classTools []spiceboxv1alpha1.SpiceboxTool,
	outs map[string][]bt.SandboxOutput,
) (string, []string, error) {
	var (
		tool    string
		results []string
	)
	// Catalog order is the class's own; sorted so the refusal below names the
	// same pair whichever way the class happened to list them.
	names := make([]string, 0, len(classTools))
	for _, tl := range classTools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	for _, name := range names {
		seq, ok := lookupSandboxOutput(outs, bundleName, name)
		if !ok {
			continue
		}
		var texts []string
		for _, so := range seq {
			if so.StreamResult == "" {
				continue
			}
			texts = append(texts, so.StreamResult)
		}
		if len(texts) == 0 {
			continue
		}
		if tool != "" {
			return "", nil, fmt.Errorf(
				"threadrun: toolBundle %q recorded a streamResult for both %q and %q, and one toolBundle is "+
					"one sandbox pod: the fake exec binder's streaming half is keyed by pod and receives no "+
					"argv, so the two cannot be told apart at replay", bundleName, tool, name)
		}
		tool, results = name, texts
	}
	return tool, results, nil
}

// streamResponder builds the toolkit-side program for ONE sandbox pod whose
// recorded results came from a STREAMING or INTERACTIVE toolkit.
//
// It writes the recorded RESULT TEXT to the toolkit's stdout and exits 0. That
// is deliberate and is the whole of the black-box claim: nothing durable holds
// the toolkit's wire stream, so the composed result is the only artifact of the
// call that survives, and re-deriving a stream from it would be inventing
// traffic in the toolkit's own dialect. The text is not valid stream-format
// output, so the replayed toolkit's parser recognizes no terminal result and
// the sandbox tool's own fallback serves the stdout tail under a freshly
// composed "status: <word> (exit 0)" header — which is why the replayed result
// carries two headers, and why nobody can mistake it for a reproduction. See
// bt.SandboxOutput.StreamResult.
//
// Exit 0 unconditionally: bt.SplitStreamResult refuses to carry anything but a
// success or a maxDuration, so a recorded stream result never stands for a
// process that failed.
//
// Successive calls get the successive recordings, clamped at the last, for the
// reason sandboxResponder's cursor gives. Guarded because the runner dispatches
// from its own goroutines.
func streamResponder(results []string) func(io.Reader, io.Writer, io.Writer) int32 {
	var (
		mu    sync.Mutex
		calls int
	)
	return func(_ io.Reader, stdout, _ io.Writer) int32 {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		_, _ = stdout.Write([]byte(results[min(i, len(results)-1)]))
		return 0
	}
}

// sandboxResponder builds the per-command answer for ONE sandbox pod.
//
// The dispatch rule: the toolcall controller runs
// `tool.Command + tool.DefaultArgs + call args`, so a request belongs to the
// class tool whose Command is a prefix of its argv. Longest match wins, because
// two tools may share a leading element ("/bin/sh -c" and "/bin/sh") and the
// more specific catalog entry is the one that meant it.
//
// classTools comes from the session's RESOLVED class rather than from the
// fixture files: it is the same snapshot the toolcall controller resolves the
// command from, so the two cannot disagree about what a tool's argv is.
//
// An argv that matches no catalog entry, or one whose tool has no recorded
// output, gets an explicit failure naming the command. It is the whole reason
// this is a func and not a static Response: a zero Response would replay as a
// tool that ran fine and printed nothing, and the divergence would surface
// several steps later as the model saying something odd.
// bundleName is the toolBundle this pod serves, and it is what disambiguates a
// class tool reached by more than one bundle. One SpiceboxClass is one container
// image, and several toolBundles routinely narrow the SAME binary differently —
// so a bundle's ToolOutputs entry may be keyed either by the LLM-facing
// "<bundle>_<tool>" (which is unique, and what a capture emits) or by the bare
// class-tool name (which is shorter, and what a hand-authored fixture says when
// only one bundle reaches the tool). The qualified spelling is preferred, so
// adding a second bundle over an existing class never silently re-points the
// first one's recorded output.
//
// Successive calls to ONE tool are served the successive recorded results, so a
// tool whose third call exited non-zero fails on the third call and not before.
// Guarded, because the runner dispatches from its own goroutines while the test
// goroutine built this closure; clamped at the last entry rather than wrapping
// or panicking, for the reason bt.SequencedHandler gives — an overrun means the
// replay already diverged, and the step's own Expect reports that with an index.
func sandboxResponder(
	bundleName string,
	classTools []spiceboxv1alpha1.SpiceboxTool,
	outs map[string][]bt.SandboxOutput,
) func(exec.Request) fake.Response {
	// Longest Command first, so the first prefix match is the most specific.
	tools := slices.Clone(classTools)
	slices.SortStableFunc(tools, func(a, b spiceboxv1alpha1.SpiceboxTool) int {
		return len(b.Command) - len(a.Command)
	})

	var (
		mu    sync.Mutex
		calls = map[string]int{} // class tool -> calls served so far
	)
	return func(req exec.Request) fake.Response {
		for _, tl := range tools {
			if len(tl.Command) == 0 || !hasArgvPrefix(req.Command, tl.Command) {
				continue
			}
			seq, ok := lookupSandboxOutput(outs, bundleName, tl.Name)
			if !ok || len(seq) == 0 {
				want := tl.Name
				if bundleName != "" {
					want = bundleName + "_" + tl.Name
				}
				return fake.Response{ExitCode: -1, Err: fmt.Errorf(
					"threadrun: the bundle recorded no toolOutputs entry for sandbox tool %q of toolBundle %q "+
						"(argv %v); add {%q: {\"stdout\": …}} or the replay diverges here",
					tl.Name, bundleName, req.Command, want)}
			}
			mu.Lock()
			i := calls[tl.Name]
			calls[tl.Name] = i + 1
			mu.Unlock()
			out := seq[min(i, len(seq)-1)]
			if out.StreamResult != "" {
				// The recording says a toolkit STREAM answered this call, and
				// the call arrived as a one-shot process exec instead. So the
				// tool's mode diverged from the recording — a toolspec no
				// longer narrowed to the streaming subcommand, or a toolkit
				// revision that dropped it. Serving the text as stdout would
				// hide that behind a green step whose assertion the composed
				// result still satisfies.
				return fake.Response{ExitCode: -1, Err: fmt.Errorf(
					"threadrun: sandbox tool %q of toolBundle %q recorded a STREAMING result, but the "+
						"replayed call arrived as a one-shot process exec (argv %v) — the tool's mode "+
						"diverged from the recording, so nothing here can answer it",
					tl.Name, bundleName, req.Command)}
			}
			return fake.Response{
				Stdout:   []byte(out.Stdout),
				Stderr:   []byte(out.Stderr),
				ExitCode: out.ExitCode,
			}
		}
		return fake.Response{ExitCode: -1, Err: fmt.Errorf(
			"threadrun: argv %v matches no tool in the SpiceboxClass catalog (%s), so the bundle "+
				"cannot say what it should return", req.Command, catalogNames(tools))}
	}
}

// lookupSandboxOutput resolves a class tool's recorded results, preferring the
// bundle-qualified key over the bare one. See sandboxResponder's bundleName
// parameter for why both spellings exist.
func lookupSandboxOutput(outs map[string][]bt.SandboxOutput, bundleName, classTool string) ([]bt.SandboxOutput, bool) {
	if bundleName != "" {
		if out, ok := outs[bundleName+"_"+classTool]; ok {
			return out, true
		}
	}
	out, ok := outs[classTool]
	return out, ok
}

// hasArgvPrefix reports whether argv begins with the whole of prefix.
func hasArgvPrefix(argv, prefix []string) bool {
	if len(argv) < len(prefix) {
		return false
	}
	return slices.Equal(argv[:len(prefix)], prefix)
}

// catalogNames renders the catalog for the error above, so a failing replay
// names what WAS available rather than only what was asked for.
func catalogNames(tools []spiceboxv1alpha1.SpiceboxTool) string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, fmt.Sprintf("%s=%v", tl.Name, tl.Command))
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "empty"
	}
	return strings.Join(names, ", ")
}
