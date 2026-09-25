package hooks

import (
	"context"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// A Destination is somewhere data can go. The info-leakage audience gate
// compares what a session has accumulated against who would receive it, and
// that comparison — computePermitted, subtractSubjects — never cared what
// kind of destination it was. Three things do: resolving the audience, the
// bypass rules for a destination that cannot enumerate its own recipients,
// and whether a leak here is something a human can be asked to allow. This
// interface is those three, so a channel reply and a memory pool are judged
// by one gate rather than two.
type Destination interface {
	// Audience returns the subjects that would receive data sent here.
	//
	// An error means the audience is UNKNOWN, which is not the same as
	// empty: an empty audience passes the gate vacuously, so a caller that
	// treats a failure as an empty set opens the gate. Every caller refuses
	// on error.
	Audience(ctx context.Context) ([]string, error)

	// Capability reports how completely this destination can enumerate its
	// recipients, which decides the bypass branches. It performs no I/O.
	Capability() channelkinds.Capability

	// Describe names the destination for refusal messages and audit records.
	Describe() string

	// MayAskAnApprover reports whether a leak to this destination can be put
	// to a human, or must be refused outright.
	//
	// This is a property OF THE DESTINATION, not of the gate, which is why it
	// lives here rather than as a branch in the consumer. A channel reply can
	// sensibly ask "may I send this to these people": an approval grants those
	// people access to the data, which is exactly the question the card puts.
	// A pool write cannot. The grant an approval writes names the SOURCE
	// resources, so approving a cross-pool write would widen who may read the
	// resource the session read FROM — a different resource entirely, and not
	// what a card reading "would share with…" tells the approver they are
	// agreeing to.
	//
	// The design spec rules on this directly: a cross-pool write is refused,
	// fail-closed, with no prompt, and a leakage_share card is the option that
	// was considered and declined.
	MayAskAnApprover() bool
}

// channelDestination adapts the deps' ResolveAudience closure. It resolves
// once and memoizes, because Capability() must answer without I/O while the
// underlying closure returns both halves together.
type channelDestination struct {
	resolve func(ctx context.Context) ([]string, channelkinds.Capability, error)

	done bool
	subs []string
	cap  channelkinds.Capability
	err  error
}

// errNoChannelAudienceResolver is what a channel destination reports when
// nothing resolves the channel's membership. Unknown, not empty — the caller
// that wants "no resolver means no channel gate" applies that policy before
// building a destination (InfoLeakAudience.enforceAudienceForChannel), because
// it is true of the channel only.
var errNoChannelAudienceResolver = errors.New("info-leakage: no channel audience resolver wired; the channel's audience is unknown")

func (d *channelDestination) load() {
	if d.done {
		return
	}
	d.done = true
	if d.resolve == nil {
		// A nil resolver reaches here, because the "no resolver means no
		// channel gate" bypass lives on the channel leg
		// (enforceAudienceForChannel) — it cannot live in the shared gate
		// without silently skipping a pool destination. So answer rather than
		// dereference a nil func: a panic in a security gate is worse than a
		// refusal.
		d.err = errNoChannelAudienceResolver
		return
	}
	d.subs, d.cap, d.err = d.resolve(context.Background())
}

func (d *channelDestination) Audience(context.Context) ([]string, error) {
	d.load()
	return d.subs, d.err
}

func (d *channelDestination) Capability() channelkinds.Capability {
	d.load()
	return d.cap
}

func (d *channelDestination) Describe() string { return "channel" }

// A channel leak is the case the leakage_share card was written for: the
// approver is told who would newly see the data, and their yes grants exactly
// those subjects exactly that access. Answering true here preserves the
// pre-destination behaviour of every channel leg unchanged.
func (d *channelDestination) MayAskAnApprover() bool { return true }
