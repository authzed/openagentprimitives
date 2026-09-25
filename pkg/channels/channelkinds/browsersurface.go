package channelkinds

// BrowserSurface is implemented by channel kinds whose viewer already reads
// the session through a platform-served page on webd's own trusted origin.
// Optional, discovered by type assertion.
//
// It lets a caller ask "would a link to a webd page tell this viewer anything
// they do not already have?" without comparing kind names. Not implementing it
// means "not a browser surface" — the safe default, since a redundant link
// offer is merely cosmetic while withholding one from a Slack or terminal
// viewer leaves them unable to reach the page at all.
type BrowserSurface interface {
	IsBrowserSurface() bool
}
