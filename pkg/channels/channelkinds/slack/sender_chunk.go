package slack

// Slack Block Kit imposes two hard limits that a naive "one section block holds
// the whole reply" renderer violates for long agent replies:
//
//   - a section block's mrkdwn text may be at most 3000 characters, and
//   - a single message may carry at most 50 blocks.
//
// Exceeding either makes chat.postMessage reject the whole message with an
// opaque "invalid_blocks" error — which, before this splitter existed, silently
// dropped the agent's reply and left the user with a generic "couldn't deliver"
// notice. See chunkForSlackSection and buildUserMessageBlocks.
const (
	// slackSectionMaxRunes caps each section block's mrkdwn payload below
	// Slack's 3000-char limit, leaving headroom (mrkdwn escaping can expand a
	// string, e.g. `<`→`&lt;`).
	slackSectionMaxRunes = 2900

	// slackMaxBlocksPerMessage is Slack's hard limit on blocks per message.
	slackMaxBlocksPerMessage = 50
)

// chunkForSlackSection splits text into pieces each at most maxRunes runes,
// preferring to break just after the last whitespace within the window so words
// and lines are not split mid-token. A single token longer than maxRunes (no
// whitespace to break on) is hard-cut.
//
// It NEVER drops content: strings.Join(chunkForSlackSection(t, n), "") == t for
// all t. Trailing whitespace stays at the end of the preceding chunk, which is
// harmless in a Slack section block. Text at or under the limit (including the
// empty string) is returned as a single verbatim chunk, matching the prior
// single-block behavior exactly.
func chunkForSlackSection(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = slackSectionMaxRunes
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}

	var chunks []string
	for len(runes) > maxRunes {
		cut := maxRunes
		// Search back from the window end for a whitespace boundary. i > 0 so a
		// leading-whitespace-only window can't produce a zero-length chunk.
		for i := maxRunes - 1; i > 0; i-- {
			if r := runes[i]; r == '\n' || r == ' ' || r == '\t' {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
