package credupdate

import "time"

// The credential-update ask spans THREE binaries with no shared runtime
// dependency, and their clocks must stay in a fixed order:
//
//	tool wait  <=  park TTL  <=  link lifetime
//
// The runner's blocked tool must give up no LATER than the operator expires the
// request, or it blocks on a request that can never be answered; the card's
// signed deep-link must outlive the Open window, or a human who clicks near the
// end lands on a dead link.
//
// This file is the ONE place those three values are defined. Each consumer
// declares its own exported knob as an ALIAS of the matching constant below,
// with a compile-time assertion pinning the ordering right next to it:
//
//	pkg/controllers/credentialupdaterequest.DefaultCredentialUpdateIdleTTL
//	    (operator)  = DefaultAskWindow      -- how long a request may sit Open
//	pkg/channels/channelsd/pipeline.DefaultCredentialUpdateLinkTimeout
//	    (channelsd) = DefaultLinkLifetime   -- how long the card's link is valid
//	pkg/agent/tool/meta/capability's credentialUpdateMinWait
//	    (runner)    = DefaultToolWait       -- the floor on the tool's own wait
//
// The assertions are the point: aliasing alone still lets a site RESTATE the
// value and break the ordering silently. Converting a negative constant to an
// unsigned type is a compile error, so `uint64(A - B)` statically asserts A >= B.
//
// This package is the right home because it is the one dependency-free package
// (no k8s client, no network, no clock) all three already import.
const (
	// DefaultAskWindow is how long a CredentialUpdateRequest may sit Open
	// waiting for a human before the operator expires it. It is the middle
	// term of the ordering; changing it re-derives the other two.
	DefaultAskWindow = 30 * time.Minute

	// DefaultLinkLifetime is how long the card button's signed deep-link stays
	// valid. Must be >= DefaultAskWindow so a click in the last minute of the
	// window still lands on a live link.
	DefaultLinkLifetime = DefaultAskWindow

	// DefaultToolWait is the floor on how long request_credential_update blocks
	// before giving up on its own clock. Must be <= DefaultAskWindow, or the
	// tool waits on a decision that can never arrive. A third of the window
	// leaves the agent time to report the problem while the card is still live.
	DefaultToolWait = DefaultAskWindow / 3
)

// Compile-time ordering assertions. Each converts a difference that MUST be
// non-negative to an unsigned type, so an ordering-breaking edit fails the build
// rather than shipping a skew nobody notices until a park times out wrong.
const (
	_ = uint64(DefaultAskWindow - DefaultToolWait)     // tool wait <= park TTL
	_ = uint64(DefaultLinkLifetime - DefaultAskWindow) // park TTL <= link lifetime
)
