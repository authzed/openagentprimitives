package bronzethread

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// SandboxOutput is what a Bundle.ToolOutputs entry holds for a SANDBOX tool.
//
// An MCP tool answers with a JSON-RPC result, so its entry is that result
// verbatim. A sandbox tool answers with process streams: the toolcall
// controller runs argv in the pod, captures stdout/stderr, and the sandbox tool
// COMPOSES the model-facing result from them. So the recorded thing is the
// process's own output, and the composition runs for real at replay — which is
// the black-box rule applied honestly, rather than canning a composed string
// the replay would have to be tricked into producing.
//
// The shape predates the wiring: the sandbox-subcommand-permissions fixture has
// carried `"toolOutputs": {"gh": {"stdout": …}}` since it was written, which is
// exactly this type. It was dead weight until the driver learned to route a
// sandbox name to the fake exec binder.
type SandboxOutput struct {
	// Stdout is the bytes the process wrote to stdout.
	//
	// It is the whole of what an ordinary successful call contributes: the
	// sandbox tool appends its own "[exit=…; artifacts: …]" trailer, and those
	// artifact refs are per-run values a bundle must not pin.
	Stdout string `json:"stdout"`

	// Stderr is the bytes the process wrote to stderr. Read back only on a
	// FAILING call — the sandbox tool's success path never shows stderr to the
	// model — so it is omitted for the ordinary case.
	Stderr string `json:"stderr,omitempty"`

	// ExitCode is the process exit status; zero is success and is the default.
	//
	// Non-zero makes the toolcall controller mark the ToolCall Failed, which is
	// a genuinely different model-facing result ("ToolCall failed: NonZeroExit
	// — exit code N"), not a cosmetic difference. It is spelled out so a
	// captured failure replays as a failure.
	ExitCode int32 `json:"exitCode,omitempty"`

	// StreamResult is the model-facing RESULT TEXT a STREAMING or INTERACTIVE
	// toolkit's call produced, carried verbatim — the one entry in this type
	// that is not process output.
	//
	// It exists because such a call has no process output to record. The
	// toolkit writes a wire stream (claude-stream-json, say), the sandbox tool
	// bridges it to the channel and then COMPOSES the model-facing result out
	// of the parsed terminal event: a "status: <word> (<duration>, $<cost>)"
	// header over the toolkit's own final text. Nothing durable holds that
	// stream — the tool_session Kind drops text deltas under its default
	// high-signal mode — so the composed result is the only recording there is,
	// and it is exactly what the model was handed.
	//
	// The bundle therefore treats the whole streaming call as the black box and
	// serves this text back as the toolkit's stdout. That is a REAL coverage
	// loss, and the capture says so in a finding rather than leaving a reader to
	// notice: the replayed toolkit emits nothing its stream parser recognizes,
	// so the parse, the terminal-event decode and the duration/cost header
	// derived from it are not exercised. What IS exercised is the rest of the
	// path — the ToolCall, the gateway, the bridge, and composeStreamResult's
	// no-terminal-result fallback, which wraps this text in a fresh
	// "status: <word> (exit <N>)" header of its own. The doubled header is the
	// point: a replayed result visibly carries both, so a bundle that replays a
	// recording cannot be mistaken for one that reproduced it.
	//
	// Mutually exclusive with Stdout/Stderr/ExitCode — a call is answered by a
	// process or by a toolkit stream, never by both, and DecodeSandboxOutput
	// refuses the pairing.
	StreamResult string `json:"streamResult,omitempty"`
}

// StreamResultBudgetBytes bounds the recorded stream result a bundle may carry.
//
// The replay serves it as the toolkit's own stdout, and the sandbox tool folds
// at most maxStdoutTailBytes of stdout into the result it composes
// (pkg/agent/tool/sandbox). A larger recording would arrive with its HEAD
// truncated — the tail buffer keeps the last bytes — so the assertion derived
// from it could never match. Refusing at capture names the tool; letting it
// through fails the replay several steps later on bytes nobody can trace back.
//
// TestStreamResultBudgetMatchesTheStdoutTail (pkg/agent/tool/sandbox) pins the
// two constants together, because they are one budget read from both sides.
const StreamResultBudgetBytes = 16 << 10

// The streaming composition, spelled here for the same reason
// sandboxResultTrailer is: it is read in BOTH directions and the two readings
// must never disagree. pkg/agent/tool/sandbox.composeStreamResult writes
//
//	status: <word> (<duration>, $<cost>)\n<the toolkit's final text>
//
// when the toolkit's stream parser recognized a terminal result, and
//
//	status: <word> (exit <N>)\n<the toolkit's stdout tail>
//
// when it did not. The word comes from the bridge's exit reason.
const (
	streamStatusPrefix = "status: "
	streamDetailOpen   = " ("

	// The two words a bundle can serve back. Both mean the call returned an
	// ordinary, non-terminal, non-error result, which is the only shape a
	// canned stream result can honestly claim; see SplitStreamResult for why
	// the other two are refused.
	streamWordSuccess     = "success"
	streamWordMaxDuration = "maxDuration"
)

// SplitStreamResult recognizes the model-facing result a STREAMING or
// INTERACTIVE toolkit's SUCCESSFUL call produced, and returns it unchanged.
//
// Unchanged is the whole point, and it is what makes this different from every
// other Split in this file. Those invert a composition to recover what a
// PROCESS printed, because the replay re-composes the rest for real. There is
// nothing to invert here: the toolkit's wire stream is not recorded anywhere,
// so the composed text is the only artifact of the call that survives, and the
// bundle carries it whole and hands it back as the toolkit's own output. See
// SandboxOutput.StreamResult for what that costs.
//
// ok is false for everything a canned stream result cannot honestly stand in
// for:
//
//   - Anything whose first line is not the status header. A process
//     composition, a secret-output diversion and a failure summary are all
//     recognized by their own Split above, and a shape none of them matched is
//     not a streaming result either.
//   - The word "idle". An idle exit parks the session and carries
//     Terminal=true, which ends the turn; served back as an ordinary result the
//     replay would run on past the point the recording stopped.
//   - The word "failed", and any error result. The caller reaches this only for
//     a result the transcript recorded as a success, but the word is checked
//     anyway: composeStreamResult also writes "failed" for a clean process
//     whose parsed outcome said the WORK failed, and a bundle serving that back
//     as a success would reverse what it is evidence of.
//   - A recording over StreamResultBudgetBytes, which the replay's stdout tail
//     could not carry whole.
func SplitStreamResult(content string) (composed string, ok bool) {
	if len(content) > StreamResultBudgetBytes {
		return "", false
	}
	header := content
	if nl := strings.IndexByte(content, '\n'); nl >= 0 {
		header = content[:nl]
	}
	if !strings.HasPrefix(header, streamStatusPrefix) || !strings.HasSuffix(header, ")") {
		return "", false
	}
	rest := header[len(streamStatusPrefix):]
	open := strings.Index(rest, streamDetailOpen)
	if open <= 0 {
		return "", false
	}
	// A header with nothing between the parentheses is neither format; both
	// write a duration and a cost, or an exit code.
	if len(rest)-(open+len(streamDetailOpen)) < 2 {
		return "", false
	}
	switch rest[:open] {
	case streamWordSuccess, streamWordMaxDuration:
		return content, true
	default:
		return "", false
	}
}

// DecodeSandboxOutput reads a ToolOutputs entry as a sandbox result.
//
// Strict about unknown fields. A typo'd key would otherwise decode to the zero
// value and replay as a tool that ran fine and printed nothing — the silent
// divergence a bundle exists to prevent — and the driver has no other way to
// tell an empty recording from a misspelled one.
//
// Equally strict about the one pairing that has no meaning: a streamResult
// alongside any process field. The two are answered by DIFFERENT halves of the
// fake exec binder — a stream program keyed by pod, a request responder keyed
// by argv — so an entry carrying both says the same call was served twice, and
// which half won would depend on the tool's mode rather than on the bundle.
func DecodeSandboxOutput(raw json.RawMessage) (SandboxOutput, error) {
	var out SandboxOutput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return SandboxOutput{}, fmt.Errorf(
			"decoding a sandbox tool output (expected {\"stdout\": …, \"stderr\": …, \"exitCode\": …} "+
				"or {\"streamResult\": …}): %w", err)
	}
	if out.StreamResult != "" && (out.Stdout != "" || out.Stderr != "" || out.ExitCode != 0) {
		return SandboxOutput{}, fmt.Errorf(
			"a sandbox tool output carries streamResult together with process output " +
				"(stdout/stderr/exitCode); a call is answered by a process or by a toolkit stream, " +
				"never by both")
	}
	return out, nil
}

// sandboxResultTrailer is the line the sandbox tool appends to stdout on a
// SUCCESSFUL call, before the model sees it:
//
//	[exit=0; artifacts: stdout=<ref>, stderr=<ref>]
//
// The refs are per-run artifact-store keys (they embed the session name and the
// ToolCall UID), so they are the one part of a recorded sandbox result that a
// bundle must NOT carry: pinned in an assertion they would fail every replay,
// and stored in a ToolOutputs entry they would be handed back as if the process
// had printed them.
//
// The format is spelled here rather than in pkg/steelthread because it is read
// in BOTH directions and the two readings must never disagree: capture strips
// the trailer to recover stdout, and replay's own sandbox tool re-appends a
// fresh one. Same rule as MintedIDFields.
const sandboxResultTrailer = "[exit="

// SplitSandboxResult recovers the process stdout from the model-facing result
// text a SUCCESSFUL sandbox call produced, and reports whether the text had
// that shape at all.
//
// The composition it inverts (pkg/agent/tool/sandbox.composeToolCallResult) is:
// stdout, a newline if stdout lacked one, then the trailer line. So the
// recovered stdout always ends in a newline, which is what makes the round trip
// exact: fed back through the composer it is left alone rather than having a
// second newline added.
//
// ok is false for anything else — a failed call ("ToolCall failed: …"), a
// secret-output diversion, an MCP result — and the caller is expected to refuse
// rather than to guess, because every one of those means something different
// happened than a process printing to stdout.
func SplitSandboxResult(content string) (stdout string, exitCode int32, ok bool) {
	// The trailer is the LAST line, and stdout may itself contain a line that
	// starts with "[exit=" — so anchor on the final newline-delimited segment
	// rather than on the first match.
	trimmed := strings.TrimSuffix(content, "\n")
	nl := strings.LastIndex(trimmed, "\n")
	if nl < 0 {
		return "", 0, false
	}
	last := trimmed[nl+1:]
	if !strings.HasPrefix(last, sandboxResultTrailer) || !strings.HasSuffix(last, "]") {
		return "", 0, false
	}
	rest := strings.TrimPrefix(last, sandboxResultTrailer)
	semi := strings.Index(rest, ";")
	if semi < 0 {
		return "", 0, false
	}
	code, err := strconv.ParseInt(rest[:semi], 10, 32)
	if err != nil {
		return "", 0, false
	}
	// Everything up to and including the newline that precedes the trailer.
	return content[:nl+1], int32(code), true
}

// The failure composition, spelled here for the same reason
// sandboxResultTrailer is: it is read in BOTH directions and the two readings
// must never disagree. pkg/agent/tool/sandbox.composeToolCallResult writes
//
//	ToolCall failed: <reason> — <message>\n
//	stderr (last <N> bytes):\n<stderr>\n     (only when stderr is non-empty)
//
// where reason and message come from the ToolCall's terminal condition.
const (
	sandboxFailurePrefix   = "ToolCall failed: "
	sandboxFailureSep      = " — "
	sandboxStderrPrefix    = "stderr (last "
	sandboxStderrInfix     = " bytes):\n"
	sandboxTruncatedMarker = "\n...(truncated)"

	// nonZeroExitReason and exitCodeMessagePrefix are the ONE terminal
	// condition a replay can reproduce: the toolcall controller marks a
	// process that exited non-zero Failed with exactly this reason and
	// fmt.Sprintf("exit code %d", code) as the message
	// (pkg/apis/v1alpha1.ReasonNonZeroExit, pkg/controllers/toolcall).
	nonZeroExitReason     = "NonZeroExit"
	exitCodeMessagePrefix = "exit code "
)

// SplitSandboxFailure recovers the process exit code and stderr from the
// model-facing result text a FAILED sandbox call produced, and reports whether
// the failure was one a replay can reproduce.
//
// A failed call has no stdout to recover — composeToolCallResult drops it and
// writes a summary of the ToolCall's terminal CONDITION instead — so what a
// bundle has to express is the condition, and the only lever it has is the
// process exit code the fake exec binder returns. That reaches exactly one
// condition: NonZeroExit with "exit code N". Fed back as
// SandboxOutput{ExitCode: N, Stderr: …}, the replayed call re-composes these
// same bytes, so nothing is invented — every part of the result is recovered
// from the record or regenerated by the code under test.
//
// ok is false for everything else, and each exclusion is a case where serving
// an exit code would produce DIFFERENT bytes than were recorded:
//
//   - A Timeout or a Cancellation. Both are terminal conditions our own
//     watchdog writes, with messages an exit code cannot reproduce.
//   - A NonZeroExit whose message is an exec error rather than "exit code N"
//     (the controller uses execErr.Error() when the process could not be run
//     at all).
//   - Exit code 0. Serving it would replay as a SUCCESS — the composition
//     inverts to something that is not a failure at all, which is the silent
//     reversal this whole package exists to refuse.
//   - A stderr tail the sandbox tool TRUNCATED. The recorded text ends in a
//     truncation marker and the original bytes are gone; feeding the truncated
//     text back yields a shorter stderr that the replay reads in full, so the
//     re-composed result would differ from what was recorded.
//   - A gate refusal, which never reaches this function's shape at all — its
//     body is the authz denial or the approval host's framing. See
//     steelthread.gateRefused for why such a call must can NOTHING.
func SplitSandboxFailure(content string) (stderr string, exitCode int32, ok bool) {
	if !strings.HasPrefix(content, sandboxFailurePrefix) {
		return "", 0, false
	}
	rest := content[len(sandboxFailurePrefix):]
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return "", 0, false
	}
	summary, rest := rest[:nl], rest[nl+1:]

	sep := strings.Index(summary, sandboxFailureSep)
	if sep < 0 {
		return "", 0, false
	}
	reason, message := summary[:sep], summary[sep+len(sandboxFailureSep):]
	if reason != nonZeroExitReason || !strings.HasPrefix(message, exitCodeMessagePrefix) {
		return "", 0, false
	}
	code, err := strconv.ParseInt(strings.TrimSpace(message[len(exitCodeMessagePrefix):]), 10, 32)
	if err != nil || code == 0 {
		return "", 0, false
	}

	if rest == "" {
		return "", int32(code), true
	}
	if !strings.HasPrefix(rest, sandboxStderrPrefix) {
		return "", 0, false
	}
	i := strings.Index(rest, sandboxStderrInfix)
	if i < 0 {
		return "", 0, false
	}
	body := rest[i+len(sandboxStderrInfix):]
	// The composer wrote exactly one newline after the tail.
	if !strings.HasSuffix(body, "\n") {
		return "", 0, false
	}
	body = body[:len(body)-1]
	if strings.HasSuffix(body, sandboxTruncatedMarker) {
		return "", 0, false
	}
	return body, int32(code), true
}

// secretOutputMarker opens the line the runner replaces a diverted secret
// output's result with (pkg/agent/runner/loop_secretout.go):
//
//	<description>\n<secret-output name="…" ref="…" bytes=N>
//
// The raw value never reaches the transcript — the diversion happens BEFORE the
// tool_result block is built, so neither the model nor session memory ever sees
// it. What is recorded is the description line and an opaque handle.
const secretOutputMarker = "\n<secret-output name="

// SplitSecretOutputResult recovers the description line from the model-facing
// result of a sandbox call that PRODUCED a secret output, and reports whether
// the text had that shape.
//
// This is a different shape from SplitSandboxResult's, because a producer's
// result is not composed from stdout at all: the runner substitutes the whole
// content. For a `file:`-sourced output the description IS the producer's real
// stdout (the sandbox tool puts stdout there and the file bytes in the value),
// so recovering it recovers genuine process output. For a `stdout:`-sourced one
// the description comes from the toolspec, and the process's actual stdout was
// the secret — correctly diverted, and not recoverable from any record. The
// caller is expected to say which it got rather than treating them alike.
//
// The handle and byte count are deliberately NOT returned: both are per-run
// values, and a bundle that pinned either would fail every replay.
func SplitSecretOutputResult(content string) (description string, ok bool) {
	i := strings.LastIndex(content, secretOutputMarker)
	if i < 0 || !strings.HasSuffix(strings.TrimSuffix(content, "\n"), ">") {
		return "", false
	}
	return content[:i], true
}
