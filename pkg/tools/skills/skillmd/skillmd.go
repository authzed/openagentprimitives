// Package skillmd parses a SKILL.md document: a YAML frontmatter block fenced
// by "---" lines, followed by the Markdown body. The frontmatter fields follow
// the agentskills.io standard.
package skillmd

import (
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Frontmatter holds the agentskills.io SKILL.md frontmatter fields.
type Frontmatter struct {
	// Name is the skill's own name, independent of its canonical name.
	Name string `json:"name"`
	// Description is the prose an agent reads to decide whether to load the
	// skill, so it is the field that determines when the skill fires.
	Description string `json:"description"`
	// License is the SPDX identifier the skill is published under.
	License string `json:"license,omitempty"`
	// Compatibility declares which agent runtimes the skill targets.
	Compatibility string `json:"compatibility,omitempty"`
	// Metadata is author-defined key/value data; AP stores it but does not
	// interpret any key.
	Metadata map[string]string `json:"metadata,omitempty"`
	// AllowedTools is the skill's declared tool surface, as authored. Empty
	// means the skill declares no restriction.
	AllowedTools string `json:"allowed-tools,omitempty"`
}

// Doc is a parsed SKILL.md.
type Doc struct {
	Frontmatter Frontmatter
	Body        string
}

const fence = "---"

// bom is the UTF-8 byte-order mark some editors write at the start of a file.
const bom = "\ufeff"

// ErrNoFrontmatter is Split's report that content does not open with a
// frontmatter fence.
var ErrNoFrontmatter = errors.New("skillmd: missing opening '---' frontmatter fence")

// Split separates a fenced YAML frontmatter block from the body that follows
// it. It is the one frontmatter splitter for every markdown document AP reads
// this way — a SKILL.md, and a plugin's agent definitions — so a document
// that one reader treats as fenced is fenced for all of them.
//
// CRLF line endings and a leading UTF-8 byte-order mark are normalized away,
// and a fence line may carry trailing whitespace: all three are things an
// editor adds without the author noticing, and a reader that missed them
// would see no frontmatter where another sees one. block may be empty (an
// opening fence immediately followed by the closing one). body is everything
// after the closing fence line, untrimmed. Content that does not open with a
// fence returns ErrNoFrontmatter; an opening fence with no closing one is a
// different error.
func Split(content []byte) (block, body string, err error) {
	text := strings.TrimPrefix(strings.ReplaceAll(string(content), "\r\n", "\n"), bom)
	first, rest, _ := strings.Cut(text, "\n")
	if !isFence(first) {
		return "", "", ErrNoFrontmatter
	}
	for off := 0; off <= len(rest); {
		line, next, more := strings.Cut(rest[off:], "\n")
		if isFence(line) {
			if more {
				return rest[:max(off-1, 0)], next, nil
			}
			return rest[:max(off-1, 0)], "", nil
		}
		if !more {
			break
		}
		off += len(line) + 1
	}
	return "", "", fmt.Errorf("skillmd: missing closing '---' frontmatter fence")
}

// isFence reports whether line is a frontmatter fence, ignoring trailing
// whitespace.
func isFence(line string) bool {
	return strings.TrimRight(line, " \t") == fence
}

// Parse splits the frontmatter from the body and unmarshals the frontmatter.
// The document MUST start with a "---" fence on its own line and contain a
// closing "---" fence (see Split); everything after the closing fence is the
// body (trimmed).
func Parse(content []byte) (Doc, error) {
	fmBlock, body, err := Split(content)
	if err != nil {
		return Doc{}, err
	}
	var fmStruct Frontmatter
	if err := yaml.Unmarshal([]byte(fmBlock), &fmStruct); err != nil {
		return Doc{}, fmt.Errorf("skillmd: frontmatter yaml: %w", err)
	}
	return Doc{Frontmatter: fmStruct, Body: strings.TrimSpace(body)}, nil
}
