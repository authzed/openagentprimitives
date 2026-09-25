package channelevents

// ThreadTitlePayload is the body of a KindThreadTitle envelope. Title is the
// agent-chosen conversation label. Emoji, when non-empty, overrides the
// leading glyph of the rendered title (default 🤖); empty means "keep the
// default glyph". Both are rendered by the channel kind — any per-surface
// size limits are the channel's concern, not this channel-agnostic layer's.
type ThreadTitlePayload struct {
	Title string `json:"title"`
	Emoji string `json:"emoji,omitempty"`
}
