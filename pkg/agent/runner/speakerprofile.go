// pkg/agent/runner/speakerprofile.go
//
// Per-turn injection of the current speaker's profile.
//
// This runs in the runner rather than in channelsd for a structural reason:
// the runner receives inbound work as memory turns, so listener-side
// enrichment could only reach it by persisting profile data into the
// append-only transcript. Fetching here keeps PII out of durable memory.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// hydrateSpeakerProfile appends the current speaker's profile block to the
// most recent user message. Returns msgs untouched whenever anything is
// missing — which includes the common, intended case of the capability not
// being granted at all (FetchSpeakerProfile nil).
//
// Called once per PROVIDER REQUEST, not once per human turn: a turn that
// drives several tool calls re-enters once per round-trip, all resolving to
// the SAME head user-turn index. Without memoization each round-trip would
// refetch and re-render with a fresh nonce; a changed block invalidates every
// cache breakpoint anchored past it, so that destroys prompt caching for the
// rest of the session — and turns one human turn into N calls to a
// rate-limited kind lookup (e.g. Slack's user-by-email API).
//
// So speakerProfileBlocks remembers the byte-identical block emitted at each
// message index and re-applies it verbatim, and a fetch is ATTEMPTED only when
// the head index changes to one not yet decided (see speakerProfileDecided)
// AND the speaker there differs from speakerProfileAuthor, the speaker of the
// last block emitted. A second turn from the same speaker costs nothing —
// their earlier block already told the agent who they are.
func (l *Loop) hydrateSpeakerProfile(ctx context.Context, msgs []llm.Message) []llm.Message {
	if l.FetchSpeakerProfile == nil || len(l.SpeakerProfileFields) == 0 {
		return msgs
	}
	idx := mostRecentUserTurn(msgs)
	if idx < 0 {
		return msgs
	}

	// Claim the decision for idx under the lock (fast, local) before doing
	// any I/O — see decideSpeakerProfile's doc for why the fetch itself must
	// never run with speakerProfileMu held.
	l.speakerProfileMu.Lock()
	newIdx := !l.speakerProfileDecided || idx != l.speakerProfileDecidedIdx
	if newIdx {
		l.speakerProfileDecided = true
		l.speakerProfileDecidedIdx = idx
	}
	l.speakerProfileMu.Unlock()

	if newIdx {
		l.decideSpeakerProfile(ctx, idx)
	}

	l.speakerProfileMu.Lock()
	defer l.speakerProfileMu.Unlock()
	if len(l.speakerProfileBlocks) == 0 {
		return msgs
	}
	return applySpeakerProfileBlocks(msgs, l.speakerProfileBlocks)
}

// decideSpeakerProfile makes the ONE decision for head user-turn index idx:
// whether to fetch and render a new profile block. hydrateSpeakerProfile calls
// this at most once per distinct idx, so every early return here — failure
// paths included — STICKS for the rest of that human turn, however many
// provider round-trips it takes. That is what keeps a Slack outage logging
// (and retrying) once per turn instead of once per round-trip.
//
// Every failure degrades to "no block for this idx"; speakerProfileBlocks and
// speakerProfileAuthor are left exactly as they were. A missing profile is
// ORDINARY — guests, foreign-workspace users, and anyone without a verified
// email have none by design — so it is not logged. A transport error IS
// logged: it is the only failure here an operator can act on.
//
// Locking shape: claim (read) under speakerProfileMu, fetch+render with NO
// lock held, commit under speakerProfileMu. FetchSpeakerProfile is arbitrary
// caller-supplied I/O (a live Slack API call in production), and holding the
// mutex across it would block every other speakerProfileMu critical section
// for a network round-trip.
func (l *Loop) decideSpeakerProfile(ctx context.Context, idx int) {
	l.speakerProfileMu.Lock()
	// Same speaker as the one we last actually emitted a block for: nothing
	// new to say, so no fetch at all. This is what keeps a whole conversation
	// with one speaker down to a single upstream lookup, no matter how many
	// turns they send.
	sameSpeaker := !l.lastInboundAuthor.Empty() && l.lastInboundAuthor == l.speakerProfileAuthor
	l.speakerProfileMu.Unlock()
	if sameSpeaker {
		return
	}

	email := speakerEmail(l.lastInboundAuthor)
	if email == "" {
		return
	}
	profile, err := l.FetchSpeakerProfile(ctx, email)
	switch {
	case errors.Is(err, channelkinds.ErrProfileNotFound):
		return // ordinary miss; nothing an operator would act on
	case err != nil:
		slog.Default().Info("speaker profile fetch failed; no profile injected",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return
	}
	nonce, ok := freshProfileNonce()
	if !ok {
		// No trustworthy nonce means no defensible data boundary. Emit nothing
		// rather than a block whose closing marker an attacker could predict.
		slog.Default().Info("could not generate a profile marker nonce; no profile injected",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
		return
	}
	block := userprofile.Render(profile, l.SpeakerProfileFields, nonce)
	if block == "" {
		return
	}

	l.speakerProfileMu.Lock()
	if l.speakerProfileBlocks == nil {
		l.speakerProfileBlocks = make(map[int]string)
	}
	l.speakerProfileBlocks[idx] = block
	l.speakerProfileAuthor = l.lastInboundAuthor
	l.speakerProfileMu.Unlock()
}

// applySpeakerProfileBlocks returns a copy of msgs with every remembered
// block appended, byte-for-byte, at its original index. The block is never
// written back into the Loop's persistent conversation (mirroring
// hydrateAttachments — see loop.go's call site comment), so every request's
// freshly hydrated slice starts without it and this must re-apply the full
// remembered set, not just the most recently decided index.
//
// Copy-before-mutate: msgs is the caller's live view. A message with no stored
// block keeps sharing the caller's blocks (nothing to change about it), but
// nothing here is ever written into a slice the caller still owns.
func applySpeakerProfileBlocks(msgs []llm.Message, blocks map[int]string) []llm.Message {
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for idx, block := range blocks {
		if idx < 0 || idx >= len(out) {
			// Defensive only: every stored idx came from mostRecentUserTurn(msgs)
			// at the moment it was decided, and the conversation only ever grows
			// by append, so a stored index going out of range should be
			// unreachable.
			continue
		}
		content := make([]llm.ContentBlock, 0, len(out[idx].Content)+1)
		content = append(content, out[idx].Content...)
		content = append(content, llm.ContentBlock{Type: "text", Text: block})
		out[idx].Content = content
	}
	return out
}

// speakerEmail decodes a canonical Author subject back to the email it
// encodes. Returns "" for an empty subject, a subject DecodeForDisplay could
// not decode, or a decoded payload with no "@" — all meaning "no profile to
// fetch", which is the intended fail-closed path for guests, foreign
// -workspace users, and raw passthrough subjects, none of whose self-set
// profile text is worth trusting.
func speakerEmail(author identity.Subject) string {
	if author.Empty() {
		return ""
	}
	decoded := identity.DecodeForDisplay(author.String())
	if decoded == author.String() {
		// DecodeForDisplay returns its argument UNCHANGED (prefix and all) when
		// the payload was not a decodable canonical id — e.g. a bento
		// RawSubject passthrough (the spec.authzSubject bypass). The "@" test
		// below cannot catch that: such a subject can legitimately BE
		// "user:dana@example.com" ("@" and "." are outside the base64 alphabet,
		// so decoding fails and it falls through unchanged) — "@"-bearing, yet
		// exactly the opaque, self-asserted text this function rejects.
		return ""
	}
	// A genuinely decoded email always contains "@"; a decoded synthetic
	// "kind:teamScope:externalID" payload never does.
	if !strings.Contains(decoded, "@") {
		return ""
	}
	return decoded
}

// freshProfileNonce returns a per-turn random marker nonce, or ok=false if
// one could not be generated. It MUST NOT derive from any profile content: a
// nonce a profile could predict would let crafted profile text close its own
// untrusted region and escape the data boundary.
//
// Unlike loop.go's newUntrustedOutputNonce, a crypto/rand failure here does
// not panic. That one guards EVERY tool result crossing the untrusted
// boundary, so losing it silently would defeat the boundary for the rest of
// the session. A profile block is optional decoration on an already-complete
// turn: ok=false just means this capability contributes nothing this turn, and
// the caller drops the block rather than crashing the session.
func freshProfileNonce() (string, bool) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(b[:]), true
}
