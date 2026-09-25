package fake

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The fake kind's DIRECTORY stand-in.
//
// # What it is for
//
// A captured session is replayed against a fixture whose every non-trigger
// Channel has been rewritten to kind=fake. That rewrite destroys the
// collaborator the channel-sourced meta tools were assembled against: the real
// kind advertised a user directory, so its session was offered
// lookup_user_for_mention, and the fake kind advertising none means the replay
// is offered nothing — a tool the recorded catalog names and the replay cannot
// produce, which is a hard capture refusal.
//
// So the directory is SEEDED. The bundle carries which lookup kinds the
// recorded channel advertised and, where a run consulted it, which users it
// resolved; the fake serves exactly that and nothing more. Our code — the
// capability that offers the tool, the tool's own argument parsing, its
// unsupported-kind refusal, its not-found refusal — all runs for real. Only the
// directory's CONTENTS, which were never ours, are pinned.
//
// # Why this is not the fixture proving itself
//
// delivery_surfaces.go draws the line and this sits on the same side of it. A
// trigger-status surface is refused for fake because it needs a third-party
// provider to REPORT TO, and simulating a counterparty would test the
// simulation. A directory READ has no counterparty: the kind is asked whether
// it knows a user, and answering from a table the recorded run's own answers
// were copied into is the same move every other fake collaborator already makes.
//
// What it deliberately does NOT claim is that a recorded CALL replays
// identically. The reply a caller reads is Kind.RenderMention(externalID), and
// fake renders the bare id where slack renders "<@ID>" — so a transcript that
// called the tool cannot be reproduced here, and the capture refuses that case
// separately rather than this file pretending otherwise.
//
// # Off by default, restored by the caller
//
// Process-wide, like the delivery surfaces, and for the same reason: a kind's
// advertised lookups decide which meta tools an AgentClass is offered, so a
// seed left in place would widen the tool list of every scenario that ran
// after it.

// MentionUser is one directory entry: the (kind, value) a run looked up, and
// what the real channel answered.
//
// DisplayName is carried even though the tool's reply does not include it —
// LookupUser's contract returns both, and a stand-in that dropped one would be
// answering a narrower question than the interface asks.
type MentionUser struct {
	Kind        channelkinds.MentionLookupKind
	Value       string
	ExternalID  string
	DisplayName string
}

// mentionStandIn is the seeded directory. Guarded rather than atomic because it
// holds two related values that must be swapped together, and it is read on
// whichever goroutine dispatched a tool call.
var mentionStandIn struct {
	mu      sync.RWMutex
	lookups []channelkinds.MentionLookupKind
	users   []MentionUser
}

// EnableMentionLookups makes the fake kind advertise lookups and answer them
// from users, and returns the func that restores the default (advertising
// none).
//
// Call the restore in a t.Cleanup, in the same discipline EnableDeliverySurfaces
// documents. Advertising with an EMPTY user list is legitimate and is the
// ordinary case: a recorded run that was OFFERED the tool and never called it
// needs the offer reproduced and has no answer to reproduce.
//
// Tests and the capture's own fixture prediction only. Nothing in production
// may call it: the seed is global, and a serving channelsd shares it with every
// Channel it hosts.
func EnableMentionLookups(lookups []channelkinds.MentionLookupKind, users []MentionUser) (restore func()) {
	mentionStandIn.mu.Lock()
	prevLookups, prevUsers := mentionStandIn.lookups, mentionStandIn.users
	mentionStandIn.lookups = slices.Clone(lookups)
	mentionStandIn.users = slices.Clone(users)
	mentionStandIn.mu.Unlock()
	return func() {
		mentionStandIn.mu.Lock()
		mentionStandIn.lookups, mentionStandIn.users = prevLookups, prevUsers
		mentionStandIn.mu.Unlock()
	}
}

// MentionLookupsEnabled reports what the kind currently advertises. Exists so a
// test can assert the default rather than trusting it.
func MentionLookupsEnabled() []channelkinds.MentionLookupKind {
	mentionStandIn.mu.RLock()
	defer mentionStandIn.mu.RUnlock()
	return slices.Clone(mentionStandIn.lookups)
}

// SupportedMentionLookups reports the seeded set, and nil by default.
//
// nil is what keeps lookup_user_for_mention out of every fake-bound session
// that did not ask for it — the default the whole bronze suite's tool lists
// stand on.
func (Kind) SupportedMentionLookups() []channelkinds.MentionLookupKind {
	return MentionLookupsEnabled()
}

// LookupUser answers from the seeded directory.
//
// The three refusals are the KIND's own, returned as the seam's sentinels so
// the tool's real mapping onto its three distinct messages runs: a lookup kind
// this stand-in was not seeded to advertise is Unsupported, a value it does not
// hold is NotFound, and a name matching two entries is Ambiguous. None of that
// is stood in for — only which users exist is.
func (Kind) LookupUser(
	_ context.Context, _ channelkinds.LookupDeps,
	kind channelkinds.MentionLookupKind, value string,
) (string, string, error) {
	mentionStandIn.mu.RLock()
	lookups := mentionStandIn.lookups
	users := mentionStandIn.users
	mentionStandIn.mu.RUnlock()

	if len(lookups) == 0 {
		return "", "", channelkinds.ErrMentionUnsupported
	}
	if !slices.Contains(lookups, kind) {
		return "", "", channelkinds.ErrMentionUnsupported
	}

	var matched []MentionUser
	for _, u := range users {
		// MentionLookupAny is the caller saying "apply your own heuristic", so
		// an entry recorded under any kind is a candidate for it; every other
		// kind matches only entries recorded under that same kind.
		if kind != channelkinds.MentionLookupAny && u.Kind != kind {
			continue
		}
		if strings.EqualFold(u.Value, value) {
			matched = append(matched, u)
		}
	}
	switch len(matched) {
	case 0:
		return "", "", channelkinds.ErrMentionNotFound
	case 1:
		return matched[0].ExternalID, matched[0].DisplayName, nil
	default:
		return "", "", channelkinds.ErrMentionAmbiguous
	}
}

// MentionToolDescription is empty: the generic template in
// meta.NewLookupUserForMention is what a stand-in should carry, since any
// bespoke wording here would put text in front of the model that the recorded
// channel never wrote.
func (Kind) MentionToolDescription() string { return "" }
