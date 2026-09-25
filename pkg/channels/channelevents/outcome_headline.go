package channelevents

// OutcomeHeadline is the human word for an Outcome* constant.
//
// The constants are wire enum values — what one program tells another — and
// several categories resolve carrying nothing else: both tool-approval
// decision handlers return an Outcome with no OutcomeText, so a surface
// rendering a resolved tool call has only the constant to work from. Without
// this, "approved" is what reaches the user.
//
// It lives beside the constants rather than in any one renderer because all
// three surfaces (Slack, the oap chat TUI, the web chat) need the same word for
// the same outcome. Category-aware labelling composes on top — see
// channelinteractions/categories.OutcomeLabel.
//
// An unrecognized outcome passes through unchanged: a renderer must never be
// the reason a resolution goes silent, and a future constant this build has
// not learned yet is still better shown than swallowed.
func OutcomeHeadline(outcome string) string {
	switch outcome {
	case OutcomeApproved:
		return "Approved"
	case OutcomeDenied:
		return "Denied"
	case OutcomeExpired:
		return "Expired"
	case OutcomeResolved:
		return "Resolved"
	default:
		return outcome
	}
}
