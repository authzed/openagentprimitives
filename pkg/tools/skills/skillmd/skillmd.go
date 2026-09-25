// Package skillmd parses a SKILL.md document: a YAML frontmatter block fenced
// by "---" lines, followed by the Markdown body. The frontmatter fields follow
// the agentskills.io standard.
package skillmd

import (
	"bytes"
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

// Parse splits the frontmatter from the body and unmarshals the frontmatter.
// The document MUST start with a "---" fence on its own line and contain a
// closing "---" fence; everything after the closing fence is the body (trimmed).
func Parse(content []byte) (Doc, error) {
	text := string(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")))
	if !strings.HasPrefix(text, fence+"\n") {
		return Doc{}, fmt.Errorf("skillmd: missing opening '---' frontmatter fence")
	}
	rest := text[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return Doc{}, fmt.Errorf("skillmd: missing closing '---' frontmatter fence")
	}
	fmBlock := rest[:end]
	body := rest[end+len("\n"+fence):]
	// Drop the remainder of the closing fence line.
	if nl := strings.Index(body, "\n"); nl >= 0 {
		body = body[nl+1:]
	} else {
		body = ""
	}

	var fmStruct Frontmatter
	if err := yaml.Unmarshal([]byte(fmBlock), &fmStruct); err != nil {
		return Doc{}, fmt.Errorf("skillmd: frontmatter yaml: %w", err)
	}
	return Doc{Frontmatter: fmStruct, Body: strings.TrimSpace(body)}, nil
}
