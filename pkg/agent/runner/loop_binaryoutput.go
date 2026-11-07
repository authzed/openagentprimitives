package runner

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
)

// binarySniffLen is how far into a tool's output a NUL byte marks the whole
// output as binary. It is git's own buffer_is_binary heuristic (the first 8000
// bytes), so a file git would diff as "Binary files differ" is treated as
// binary here too.
const binarySniffLen = 8000

// sanitizeToolOutput makes a tool's raw output safe to carry into a
// tool_result. It exists because NUL survives every other step: it is valid
// UTF-8, so JSON encoding keeps it (as \u0000) rather than replacing it, and
// Postgres jsonb then refuses the whole turn (SQLSTATE 22P05). The turn is the
// session's durable record, so one `git show <sha>:<binary>` failed the
// session outright.
//
// Output that looks binary is replaced with a notice naming its size — the
// model cannot use the bytes, and the notice tells it what to run instead. A
// stray NUL past the sniff window is replaced with U+FFFD, the same
// substitution JSON encoding already makes for invalid UTF-8, so the text
// around it survives.
func sanitizeToolOutput(content string) string {
	i := strings.IndexByte(content, 0)
	if i < 0 {
		return content
	}
	if i < binarySniffLen {
		return fmt.Sprintf("[binary output omitted: %s. The command printed raw binary data, which cannot be shown. "+
			"To inspect a binary file, use a command that prints text about it instead "+
			"(for example `git cat-file -s <rev>:<path>` for its size, or `git ls-tree <rev> -- <path>` for its mode).]",
			humanize.Bytes(uint64(len(content))))
	}
	return strings.ReplaceAll(content, "\x00", "�")
}
