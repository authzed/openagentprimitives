// How a channel kind's surface renders a plain-text reply — the markup
// dialect the model must emit for respond_to_user.text to look right.
package channelkinds

// DefaultTextFormattingInstructions is what a caller gets for a kind that does
// not implement TextFormatter, or a kind name absent from the registry.
//
// Deliberately the weakest true statement: over-promising a dialect the
// transport does not have is how a reply ends up full of literal asterisks. A
// kind whose surface differs MUST say so —
// TestEveryRegisteredKindDeclaresTextFormatting fails if a registered kind
// leaves this to chance.
const DefaultTextFormattingInstructions = "Markdown is supported."

// TextFormatter is the OPTIONAL interface a Kind implements to declare the
// markup dialect of its rendering surface.
//
// The returned string is MODEL-FACING PROMPT TEXT: it is appended to the
// `text` property description of respond_to_user's input schema, and the
// runner's prompt tells the model those rules are channel-specific and
// authoritative. Write it as instructions to the model, name the dialect, and
// say what is NOT supported — a model that emits `**bold**` into a mrkdwn
// surface produces literal asterisks, and nothing downstream can repair it.
//
// Return the instructions with no leading or trailing whitespace; the caller
// owns the joining space.
//
// Optional so out-of-package test doubles need not implement it. Every
// registered kind is required to, and a registry-wide sweep test enforces it.
type TextFormatter interface {
	TextFormattingInstructions() string
}

// TextFormattingInstructionsFor returns k's formatting instructions, or
// DefaultTextFormattingInstructions when k is nil (an unregistered channel
// kind name) or does not implement TextFormatter.
//
// The fallback lives HERE, not at the call site, so no consumer ever needs to
// know a kind name to pick prompt text.
func TextFormattingInstructionsFor(k Kind) string {
	tf, ok := k.(TextFormatter)
	if !ok || tf == nil {
		return DefaultTextFormattingInstructions
	}
	if instr := tf.TextFormattingInstructions(); instr != "" {
		return instr
	}
	return DefaultTextFormattingInstructions
}
