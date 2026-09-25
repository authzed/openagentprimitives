package skillsource

import (
	"fmt"
	"unicode/utf8"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
)

// maxRepoInstructionsBytes caps the repo-instructions text stored on a Skill and
// injected into the agent prompt, bounding prompt growth and blunting a hostile
// or oversized AGENTS.md/CLAUDE.md.
//
// Aliased from the validate package rather than declared again: the admission
// webhook enforces the same number, and a second literal is how the door and
// the materializer end up disagreeing about what fits.
const maxRepoInstructionsBytes = validate.MaxRepoInstructionsBytes

// BuildRepoInstructions applies the size cap to discovered repo instructions and
// returns the storable value plus a non-empty warning when it had to truncate.
// Returns (nil, "") when di is empty (no file found). Shared by the SkillSource
// and ClusterSkillSource controllers.
func BuildRepoInstructions(di DiscoveredRepoInstructions) (*v1.SkillRepoInstructions, string) {
	if di.SourceFile == "" {
		return nil, ""
	}
	content := di.Content
	truncated := false
	var warning string
	if len(content) > maxRepoInstructionsBytes {
		content = truncateUTF8(content, maxRepoInstructionsBytes) +
			fmt.Sprintf("\n\n[…truncated: %s exceeded 64 KiB]", di.SourceFile)
		truncated = true
		warning = fmt.Sprintf("repo-instructions: %s exceeded the 64 KiB cap and was truncated in the agent prompt", di.SourceFile)
	}
	return &v1.SkillRepoInstructions{SourceFile: di.SourceFile, Content: content, Truncated: truncated}, warning
}

// truncateUTF8 returns s clipped to at most max bytes, backing off so the result
// never ends mid-rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}
