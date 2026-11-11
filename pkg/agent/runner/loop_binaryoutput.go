package runner

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dustin/go-humanize"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// binarySniffLen is how much of a tool's output is examined to decide whether
// it is binary. It is the window git's own buffer_is_binary uses (the first
// 8000 bytes).
const binarySniffLen = 8000

// nulReplacement stands in for a NUL that is removed from text. It is the
// substitution JSON encoding already makes for invalid UTF-8.
const nulReplacement = "�"

// outputCleanup says what sanitizeToolOutput did to a tool's output.
type outputCleanup string

const (
	outputUnchanged     outputCleanup = ""
	outputNULReplaced   outputCleanup = "nul-replaced"
	outputBinaryOmitted outputCleanup = "binary-omitted"
)

// sanitizeToolOutput makes a tool's raw output safe to carry into a
// tool_result. It exists because NUL survives every other step: it is valid
// UTF-8, so JSON encoding keeps it (as \u0000) rather than replacing it, and
// Postgres jsonb then refuses the whole turn (SQLSTATE 22P05).
//
// Output with no NUL is returned untouched. Otherwise the rule is:
//
//   - Binary — a NUL in the first binarySniffLen bytes AND those bytes do not
//     read as text (see looksBinary) — is replaced whole with a notice naming
//     its size. The model cannot use the bytes.
//   - Anything else keeps its text, with each NUL replaced by U+FFFD. That
//     covers a stray NUL anywhere, and output that uses NUL as a separator
//     (`git ls-files -z`, `find -print0`), where the NULs are the least
//     interesting part of a listing the model asked for.
//
// A NUL alone is deliberately not enough to call output binary. git treats it
// as enough because it is deciding whether to DIFF a file; here the cost of a
// wrong "binary" is the model losing output it could have read.
func sanitizeToolOutput(content string) (string, outputCleanup) {
	if strings.IndexByte(content, 0) < 0 {
		return content, outputUnchanged
	}
	if looksBinary(content) {
		return fmt.Sprintf("[binary output omitted: %s. The tool returned raw binary data, which cannot be shown. "+
			"To inspect binary content, ask for something that describes it instead of its bytes — "+
			"its size, its type, a checksum.]",
			humanize.Bytes(uint64(len(content)))), outputBinaryOmitted
	}
	return replaceNUL(content), outputNULReplaced
}

// replaceNUL swaps each NUL in content for U+FFFD and keeps everything else.
func replaceNUL(content string) string {
	return strings.ReplaceAll(content, "\x00", nulReplacement)
}

// looksBinary reports whether the sniff window holds a NUL and does not read
// as text. "Not text" is either of: bytes that are not valid UTF-8, or a
// control character other than NUL itself, whitespace, backspace and ESC (the
// last two so terminal-styled output still counts as text). An executable or
// an image trips one of those within a few bytes of its header; a NUL-separated
// listing trips neither.
func looksBinary(content string) bool {
	window := content
	if len(window) > binarySniffLen {
		// Cut on a rune boundary, so a multi-byte character straddling the
		// window's edge is not mistaken for invalid UTF-8.
		end := binarySniffLen
		for end > binarySniffLen-utf8.UTFMax && !utf8.RuneStart(window[end]) {
			end--
		}
		window = window[:end]
	}
	if strings.IndexByte(window, 0) < 0 {
		return false
	}
	if !utf8.ValidString(window) {
		return true
	}
	for _, r := range window {
		switch {
		case r == 0, r == '\b', r == '\x1b', unicode.IsSpace(r):
		case unicode.IsControl(r):
			return true
		}
	}
	return false
}

// cleanToolOutput applies sanitizeToolOutput to a tool's result, with the two
// exceptions that make it safe to run on every result:
//
//   - A secret-output result is left alone. Its Content is the credential
//     itself, which applySecretOutput diverts into the session's Secret before
//     the LLM or memory sees it; replacing it would store the notice, or a
//     corrupted value, as the credential and report success.
//   - An error result never becomes the binary notice. Its text is what tells
//     the model what went wrong, so only the NULs are replaced.
//
// Altered output is logged: the model and the transcript no longer hold what
// the tool returned, and nothing else would say so.
func (l *Loop) cleanToolOutput(name string, res tool.Result) tool.Result {
	if res.SecretOutput != nil {
		return res
	}
	raw := res.Content
	cleanup := outputUnchanged
	if res.IsError {
		if strings.IndexByte(raw, 0) >= 0 {
			res.Content, cleanup = replaceNUL(raw), outputNULReplaced
		}
	} else {
		res.Content, cleanup = sanitizeToolOutput(raw)
	}
	if cleanup != outputUnchanged {
		slog.Default().Info("tool output carried NUL, which a tool_result cannot hold; altered before use",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"tool", name, "action", string(cleanup),
			"bytes", len(raw), "nulCount", strings.Count(raw, "\x00"), "isError", res.IsError)
	}
	return res
}
