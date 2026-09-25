package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestEveryRegisteredKindDeclaresTextFormatting is the guard that stops the
// next channel kind shipping without an answer.
//
// Picking the dialect by kind name where respond_to_user's `text` description
// is built would be the "if kind == x outside the kind's own package" AGENTS.md
// §Pluggability forbids, and would be correct only by luck of the current
// roster: every non-Slack kind today happens to be a CommonMark surface, so a
// default arm looks right. A kind added tomorrow with a different dialect would
// silently inherit CommonMark instructions and every reply would render wrong.
//
// Asserting the property registry-wide — rather than naming the kinds that
// have an answer today — is what makes the seam load-bearing.
func TestEveryRegisteredKindDeclaresTextFormatting(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by init")

	for _, k := range kinds {
		t.Run(k.Name()+": implements TextFormatter with non-empty instructions", func(t *testing.T) {
			tf, ok := k.(channelkinds.TextFormatter)
			require.True(t, ok,
				"kind %q must implement channelkinds.TextFormatter — otherwise "+
					"respond_to_user falls back to generic CommonMark instructions "+
					"that may be wrong for this transport", k.Name())

			instr := tf.TextFormattingInstructions()
			assert.NotEmpty(t, strings.TrimSpace(instr),
				"kind %q returned empty formatting instructions; the string is model-facing "+
					"prompt text and an empty answer teaches the model nothing", k.Name())
			assert.Equal(t, strings.TrimSpace(instr), instr,
				"kind %q must not pad its instructions — the caller owns the joining "+
					"whitespace, so leading/trailing space doubles up", k.Name())
		})
	}
}

// TestTextFormattingInstructionsForFallsBackWhenTheKindCannotAnswer pins the
// documented degrade path: the fallback lives in pkg/channels/channelkinds, never in a
// consumer, so no caller ever needs to know a kind name to pick prompt text.
func TestTextFormattingInstructionsForFallsBackWhenTheKindCannotAnswer(t *testing.T) {
	assert.Equal(t, channelkinds.DefaultTextFormattingInstructions,
		channelkinds.TextFormattingInstructionsFor(nil),
		"a nil Kind (unregistered channel kind name) must yield the generic instructions")
}

// TestSlackTextFormattingInstructionsAreMrkdwnNotCommonMark keeps the
// user-visible half honest: moving the rules onto the kind must not quietly
// drop Slack's mrkdwn dialect back to CommonMark, which would make every
// bolded reply render with literal asterisks.
func TestSlackTextFormattingInstructionsAreMrkdwnNotCommonMark(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "slack kind must be registered")
	tf, ok := k.(channelkinds.TextFormatter)
	require.True(t, ok, "slack must implement TextFormatter")

	instr := tf.TextFormattingInstructions()
	assert.Contains(t, instr, "mrkdwn", "slack instructions must name the mrkdwn dialect")
	assert.Contains(t, instr, "*bold*", "slack instructions must teach single-asterisk bold")
	assert.Contains(t, instr, "<https://url|label>", "slack instructions must teach the link form")
	assert.NotEqual(t, channelkinds.DefaultTextFormattingInstructions, instr,
		"slack must not degrade to the generic CommonMark instructions")
}
