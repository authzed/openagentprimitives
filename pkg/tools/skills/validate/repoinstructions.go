package validate

import (
	"fmt"
)

// MaxRepoInstructionsBytes caps spec.repoInstructions.Content.
//
// The same 64 KiB the SkillSource controller applies at materialization — where
// its own comment gives the reason, "a hostile or oversized AGENTS.md" — and
// the number is here rather than duplicated as a literal so the two cannot
// drift apart.
const MaxRepoInstructionsBytes = 64 * 1024

// RepoInstructions validates the always-on prompt text a Skill carries.
//
// This field lands VERBATIM in the system prompt of every AgentClass linking the
// skill, under "Treat them as authoritative for work that touches those repos",
// on every turn — the highest-trust text surface the product has. The controller
// capped it at materialization and the WEBHOOK never looked at it: validate.Skill
// takes no repoInstructions parameter, and the CRD declares content as a plain
// string with no maxLength. So a hand-authored Skill set it directly and it went
// straight into the prompt, uncapped.
//
// The guard-on-one-of-two-paths shape is sharp here: the DESCRIPTION beside it in
// the same prompt is validated — length, no XML/HTML tags, no reserved words —
// precisely because it reaches the prompt.
//
// A length cap is what is checked and not more. The content is a repo's own
// instructions file and is meant to read as prose; the meaningful bound is on how
// much of the prompt one skill may occupy, and the tag/reserved-word rules that
// suit a one-line description would reject legitimate Markdown here.
func RepoInstructions(content string) []error {
	if len(content) > MaxRepoInstructionsBytes {
		return []error{fmt.Errorf(
			"skill repoInstructions.content is %d bytes, over the %d-byte cap; "+
				"this text is injected into every system prompt for every agent linking this skill",
			len(content), MaxRepoInstructionsBytes)}
	}
	return nil
}
