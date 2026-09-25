package mcp

import (
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// RedactSensitive returns a deep-copied JSON-shaped map where each path
// in `paths` (dotted: "config.apiKey", "args.tags[2]") has its leaf
// value replaced by a stable token from pkg/tools/redact.Redactor.
//
// Duplicate sensitive values across paths share one token, which is what makes
// the audit trail readable. Only the addressed leaf is tokenized, keyed on its
// Stringify form; the deep copy, path parsing, and leaf replacement live in
// pkg/tools/redact, shared with the MCP spec validator.
//
// The Redactor is constructed per call because audit does not yet emit the
// ID→descriptor map. Threading it through OperationCall would let the descriptor
// map be captured alongside the redacted args.
func RedactSensitive(payload map[string]any, paths []string) map[string]any {
	if len(paths) == 0 {
		return payload
	}
	out := redact.DeepCopyJSONMap(payload)
	r := redact.New()
	for _, p := range paths {
		if err := redact.RedactLeafAtPath(out, p, r, p); err != nil {
			// A path that does not parse is an authoring bug in the declaring
			// spec. The redactor has already failed closed onto the enclosing
			// value, so nothing leaks — but the caller asked for a leaf and got
			// an object, and this log line is the only thing that says why.
			slog.Default().Info("mcp arg redaction: declared sensitive path did not parse",
				"path", p, "err", err.Error())
		}
	}
	return out
}
