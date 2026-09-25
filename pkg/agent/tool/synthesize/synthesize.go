// Package synthesize provides the shared "iterate spec → emit tool.Tool"
// machinery used by every tool-kind runtime adapter (mcp, sandbox,
// future…). Each kind's package-level Synthesize function prepares
// per-entry data via this package's Source/Entry shape; the shared
// Build does name normalization, collision detection, and the factory
// call.
package synthesize

import (
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Source describes one batch of entries to be synthesized into tool.Tools.
// The kind-specific synthesizer owns drift-check and shape decisions;
// this package owns naming and collision rules.
type Source interface {
	// Prefix is the bundle/server prefix joined with each Entry.Name to
	// form the LLM-facing tool name. Empty allowed (Entry.Name used
	// alone).
	Prefix() string
	// Entries returns the list to be synthesized. Order is preserved in
	// the returned []tool.Tool.
	Entries() []Entry
}

// Entry describes one tool to synthesize. The kind's synthesizer has
// already resolved drift, rendered the description, and prepared the
// InputSchema in whatever envelope shape is appropriate. Build's only
// remaining job is name normalization + collision detection + invoking
// Factory with the final name.
type Entry struct {
	// Name is the kind-side raw name (e.g. MCP server's tool name, or
	// the sandbox class-tool name). Joined with Source.Prefix to form
	// the full LLM-facing name.
	Name string
	// Factory builds the actual tool.Tool given the final normalized
	// LLM-facing name. The Factory is responsible for stamping its own
	// Description and InputSchema; Build does NOT pass those through.
	Factory func(llmName string) tool.Tool
}

// Slice is a convenience Source backed by a literal prefix + entries.
// Most kind adapters don't need their own Source type — they prepare a
// prefix and a []Entry, then call Build(Slice{...}).
type Slice struct {
	P string
	E []Entry
}

func (s Slice) Prefix() string   { return s.P }
func (s Slice) Entries() []Entry { return s.E }

// Build applies the shared name normalization rules + collision
// detection and invokes each Entry's Factory. Returns an error if two
// entries normalize to the same final name.
func Build(src Source) ([]tool.Tool, error) {
	entries := src.Entries()
	out := make([]tool.Tool, 0, len(entries))
	used := map[string]string{} // final name → Entry.Name (for collision diagnostics)

	for _, e := range entries {
		full := FullName(src.Prefix(), e.Name)
		if prior, exists := used[full]; exists {
			return nil, fmt.Errorf("synthesize: tool name collision: %q and %q both normalize to %q",
				prior, e.Name, full)
		}
		used[full] = e.Name
		out = append(out, e.Factory(full))
	}
	return out, nil
}

// FullName returns the LLM-facing name Build gives an entry named name under
// prefix. An empty prefix leaves the entry name to stand alone.
//
// Exported so a caller that needs to know what a tool WILL be called — without
// building it — asks the same function Build does, rather than re-joining and
// re-normalizing by hand. A second copy of "prefix, underscore, normalize"
// drifts silently: the symptom is a name that exists nowhere in the tool set,
// which reads as the tool being missing rather than as the name being wrong.
func FullName(prefix, name string) string {
	if prefix != "" {
		name = prefix + "_" + name
	}
	return NormalizeName(name)
}

// NormalizeName maps s into Anthropic's tool-name regex
// `^[a-zA-Z0-9_-]{1,128}$`. Letters lowercased; anything outside the
// alphabet replaced by '-'; truncated at 128 runes.
func NormalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}
