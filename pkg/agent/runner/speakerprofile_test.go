package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/userprofile"
	"github.com/authzed/openagentprimitives/pkg/authz/untrusted"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// authorSubject builds the canonical subject the runner decodes back to email.
func authorSubject(t *testing.T, email string) identity.Subject {
	t.Helper()
	s, err := identity.VerifiedEmail(identity.Email(email), "").Subject()
	require.NoError(t, err)
	return s
}

func msgsWithUserTurn() []llm.Message {
	return []llm.Message{
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "earlier question"}}},
		{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "earlier answer"}}},
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "current question"}}},
	}
}

// renderText concatenates a message's text blocks for assertion.
func renderText(m llm.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// captureSlog installs a capturing slog handler as the process-wide default
// for the duration of the test and restores the previous default on
// cleanup. Tests using it must not run in parallel with each other or with
// any other test that reads/writes the package-global slog default — none
// of this package's tests call t.Parallel(), so sequential execution (Go's
// default) is what keeps this safe.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestHydrateSpeakerProfileInjectsOnMostRecentUserTurn(t *testing.T) {
	var gotEmail string
	l := &Loop{
		lastInboundAuthor:    authorSubject(t, "dana@example.com"),
		SpeakerProfileFields: []userprofile.Field{userprofile.FieldDisplayName, userprofile.FieldTitle},
	}
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		gotEmail = email
		return userprofile.Profile{DisplayName: "Dana Whitfield", Title: "Director of Support"}, nil
	}

	in := msgsWithUserTurn()
	got := l.hydrateSpeakerProfile(context.Background(), in)

	assert.Equal(t, "dana@example.com", gotEmail,
		"the canonical Author subject must decode back to the speaker's email")

	require.Len(t, got, 3)
	assert.Contains(t, renderText(got[2]), untrusted.ProfileTag)
	assert.Contains(t, renderText(got[2]), "Director of Support")
	assert.NotContains(t, renderText(got[0]), untrusted.ProfileTag,
		"only the most recent user turn may carry the block; earlier turns stay prompt-cacheable")
	assert.NotContains(t, renderText(got[1]), untrusted.ProfileTag)

	// Copy-before-mutate: the caller's own slice must be untouched by the
	// append, not merely equal to the (possibly aliased) return value.
	assert.Len(t, in[2].Content, 1, "the caller's slice must not be mutated in place")
}

// TestHydrateSpeakerProfileInertWithoutAFetcher is THE negative test for this
// plan. A Loop the gate declined to offer has a nil fetcher and must be
// completely inert.
func TestHydrateSpeakerProfileInertWithoutAFetcher(t *testing.T) {
	l := &Loop{lastInboundAuthor: authorSubject(t, "dana@example.com")}
	in := msgsWithUserTurn()

	got := l.hydrateSpeakerProfile(context.Background(), in)

	// Compared against a FRESH, never-passed-in slice — not against `in` —
	// because comparing to `in` would pass trivially if the function mutated
	// `in` in place and returned it: both variables would then show the same
	// (mutated) content.
	assert.Equal(t, msgsWithUserTurn(), got, "messages must be returned untouched")
	for _, m := range got {
		assert.NotContains(t, renderText(m), untrusted.ProfileTag)
	}
}

func TestHydrateSpeakerProfileDegradesQuietly(t *testing.T) {
	// The "transport error" case below intentionally logs (see
	// TestHydrateSpeakerProfileLogsOnlyTransportErrors, which pins that log
	// line explicitly) — capture it here purely to keep this test's own
	// output pristine; nothing about the log line itself is asserted in this
	// test.
	captureSlog(t)

	// okFetch succeeds; the cases using it exercise paths that must bail out
	// BEFORE any fetch happens, so a successful fetcher proves the bail-out
	// rather than masking it.
	okFetch := func(context.Context, string) (userprofile.Profile, error) {
		return userprofile.Profile{Title: "Director of Support"}, nil
	}
	cases := []struct {
		name   string
		author identity.Subject
		fetch  func(context.Context, string) (userprofile.Profile, error)
	}{
		{
			name:   "no author at all",
			author: "",
			fetch:  okFetch,
		},
		{
			name:   "synthetic subject decodes to no email",
			author: identity.Subject("user:c2xhY2s6VDE6VTE"), // base64 of "slack:T1:U1"
			fetch:  okFetch,
		},
		{
			name:   "profile not found",
			author: authorSubject(t, "dana@example.com"),
			fetch: func(context.Context, string) (userprofile.Profile, error) {
				return userprofile.Profile{}, channelkinds.ErrProfileNotFound
			},
		},
		{
			name:   "transport error",
			author: authorSubject(t, "dana@example.com"),
			fetch: func(context.Context, string) (userprofile.Profile, error) {
				return userprofile.Profile{}, errors.New("slack: 503")
			},
		},
		{
			name:   "empty profile renders nothing",
			author: authorSubject(t, "dana@example.com"),
			fetch: func(context.Context, string) (userprofile.Profile, error) {
				return userprofile.Profile{}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{
				lastInboundAuthor:    tc.author,
				SpeakerProfileFields: userprofile.DefaultFields(),
				FetchSpeakerProfile:  tc.fetch,
			}
			got := l.hydrateSpeakerProfile(context.Background(), msgsWithUserTurn())
			// Compared against a FRESH msgsWithUserTurn(), not the `in` passed
			// to hydrateSpeakerProfile — see the identical note on
			// TestHydrateSpeakerProfileInertWithoutAFetcher.
			assert.Equal(t, msgsWithUserTurn(), got, "a miss degrades to no block, never to an error or a partial block")
		})
	}
}

func TestHydrateSpeakerProfileNoUserTurnToCarryIt(t *testing.T) {
	l := &Loop{
		lastInboundAuthor:    authorSubject(t, "dana@example.com"),
		SpeakerProfileFields: userprofile.DefaultFields(),
		FetchSpeakerProfile: func(context.Context, string) (userprofile.Profile, error) {
			return userprofile.Profile{Title: "Director of Support"}, nil
		},
	}
	in := []llm.Message{{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}}
	got := l.hydrateSpeakerProfile(context.Background(), in)
	// Compared against a freshly built literal, not `in` — see the note on
	// TestHydrateSpeakerProfileInertWithoutAFetcher.
	assert.Equal(t, []llm.Message{{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}}}, got)
}

// TestHydrateSpeakerProfileByteStableAcrossRequestsWithinOneTurn pins the
// critical fix: hydrateSpeakerProfile runs once per PROVIDER REQUEST, not
// once per human turn, so a multi-step tool-calling turn re-enters it many
// times while the head user-turn index stays fixed. Without memoization that
// would mean a fresh fetch and a freshly-nonced (therefore byte-DIFFERENT)
// block on every round-trip — destroying prompt caching for the rest of the
// session and, for a rate-limited kind lookup, turning one human turn into N
// upstream calls.
func TestHydrateSpeakerProfileByteStableAcrossRequestsWithinOneTurn(t *testing.T) {
	var calls int
	l := &Loop{
		lastInboundAuthor:    authorSubject(t, "dana@example.com"),
		SpeakerProfileFields: userprofile.DefaultFields(),
	}
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls++
		return userprofile.Profile{Title: "Director of Support"}, nil
	}

	turn1 := msgsWithUserTurn() // idx 2 is the head user turn
	got1 := l.hydrateSpeakerProfile(context.Background(), turn1)
	require.Len(t, got1[2].Content, 2, "the first request must append exactly one block")

	// Simulate a second provider round-trip WITHIN THE SAME human turn: the
	// assistant made a tool call and the runner appended its own tool_result
	// relay message. mostRecentUserTurn must still resolve to the same head
	// index (2) — extending the PRISTINE turn1, not got1, because in
	// production hydrateSpeakerProfile is always called on a fresh copy
	// derived from the Loop's persistent conversation, which never carries a
	// previously injected block.
	roundTrip2 := append(append([]llm.Message{}, turn1...),
		llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "using a tool"}}},
		llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "tool_result", Text: "tool output"}}},
	)
	got2 := l.hydrateSpeakerProfile(context.Background(), roundTrip2)
	require.Len(t, got2[2].Content, 2, "the second round-trip must reuse, not duplicate, the block")

	assert.Equal(t, 1, calls,
		"the fetcher must be called at most once per speaker-change, never once per provider round-trip")
	assert.Equal(t, got1[2].Content[1].Text, got2[2].Content[1].Text,
		"the emitted block must be byte-identical (same nonce) across requests within the same turn")
}

// TestHydrateSpeakerProfileSameSpeakerNextTurnAddsNoBlock pins the
// cross-turn half of the fix: a genuinely new human turn from the SAME
// speaker must not re-fetch or add a second block. Their existing block,
// still present earlier in the conversation, already told the agent who
// they are.
func TestHydrateSpeakerProfileSameSpeakerNextTurnAddsNoBlock(t *testing.T) {
	var calls int
	dana := authorSubject(t, "dana@example.com")
	l := &Loop{
		lastInboundAuthor:    dana,
		SpeakerProfileFields: userprofile.DefaultFields(),
	}
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls++
		return userprofile.Profile{Title: "Director of Support"}, nil
	}

	turn1 := msgsWithUserTurn() // idx 2 = dana
	got1 := l.hydrateSpeakerProfile(context.Background(), turn1)
	require.Len(t, got1[2].Content, 2)
	danaBlock := got1[2].Content[1].Text

	// Turn 2: dana speaks again. Built by extending the PRISTINE turn1 (never
	// got1) — see the identical note in the byte-stability test above.
	turn2 := append(append([]llm.Message{}, turn1...),
		llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "earlier answer 2"}}},
		llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "second question"}}},
	)
	l.lastInboundAuthor = dana // drainInbox would set this again to the same value
	got2 := l.hydrateSpeakerProfile(context.Background(), turn2)

	assert.Equal(t, 1, calls, "the same speaker on a new turn must not trigger another fetch")
	require.Len(t, got2, 5)
	assert.Len(t, got2[4].Content, 1,
		"the same-speaker turn gets NO new block — their existing block earlier in the conversation is enough")
	require.Len(t, got2[2].Content, 2, "turn 1's block must still be present")
	assert.Equal(t, danaBlock, got2[2].Content[1].Text, "turn 1's block must stay byte-identical to what was first emitted")
}

// TestHydrateSpeakerProfileSpeakerChangeAddsBlockKeepsEarlierOne pins the
// other half: a genuine speaker change adds a NEW block at the new head
// index while the earlier speaker's block remains present, at its original
// index, byte-identical.
func TestHydrateSpeakerProfileSpeakerChangeAddsBlockKeepsEarlierOne(t *testing.T) {
	dana := authorSubject(t, "dana@example.com")
	ravi := authorSubject(t, "ravi@example.com")
	calls := map[string]int{}
	l := &Loop{
		lastInboundAuthor:    dana,
		SpeakerProfileFields: userprofile.DefaultFields(),
	}
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls[email]++
		return userprofile.Profile{Title: "profile-for-" + email}, nil
	}

	turn1 := msgsWithUserTurn() // idx 2 = dana
	got1 := l.hydrateSpeakerProfile(context.Background(), turn1)
	require.Len(t, got1[2].Content, 2)
	danaBlock := got1[2].Content[1].Text

	// Turn 2: ravi speaks — a genuinely different author. Extends the
	// PRISTINE turn1, same reasoning as the two tests above.
	turn2 := append(append([]llm.Message{}, turn1...),
		llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "earlier answer 2"}}},
		llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "ravi's question"}}},
	)
	l.lastInboundAuthor = ravi
	got2 := l.hydrateSpeakerProfile(context.Background(), turn2)

	assert.Equal(t, 1, calls["dana@example.com"])
	assert.Equal(t, 1, calls["ravi@example.com"])

	require.Len(t, got2[2].Content, 2, "dana's earlier block must still be present")
	assert.Equal(t, danaBlock, got2[2].Content[1].Text, "dana's block must stay byte-identical to what was first emitted")

	require.Len(t, got2, 5)
	require.Len(t, got2[4].Content, 2, "ravi's new turn must carry a new block")
	assert.Contains(t, got2[4].Content[1].Text, "profile-for-ravi@example.com")
}

// TestHydrateSpeakerProfileFailedFetchRetriesOncePerTurn pins the retry
// cadence that makes the "same speaker as last EMITTED" interpretation
// (decideSpeakerProfile's doc) defensible: a failing fetch must be memoized
// the same way a successful one is — NOT retried on a later round-trip of
// the SAME turn (the amplification Finding 1 fixed applies to failures too:
// a Slack outage must log/retry once per turn, not once per round-trip) —
// but IS retried on a genuinely NEW turn, even from the same speaker whose
// previous attempt failed, since nothing was ever successfully emitted for
// them to reuse.
func TestHydrateSpeakerProfileFailedFetchRetriesOncePerTurn(t *testing.T) {
	captureSlog(t) // the transport-error path logs; keep this test's output pristine

	var calls int
	dana := authorSubject(t, "dana@example.com")
	l := &Loop{
		lastInboundAuthor:    dana,
		SpeakerProfileFields: userprofile.DefaultFields(),
	}
	l.FetchSpeakerProfile = func(_ context.Context, email string) (userprofile.Profile, error) {
		calls++
		return userprofile.Profile{}, errors.New("slack: 503")
	}

	turn1 := msgsWithUserTurn() // idx 2 = dana
	got1 := l.hydrateSpeakerProfile(context.Background(), turn1)
	assert.Equal(t, 1, calls, "the first request in turn 1 must attempt the fetch")
	assert.Len(t, got1[2].Content, 1, "a failed fetch must not add a block")

	// Second provider round-trip, SAME turn (same head index) — extends the
	// PRISTINE turn1, not got1; see the identical note on the byte-stability
	// test above.
	roundTrip2 := append(append([]llm.Message{}, turn1...),
		llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "using a tool"}}},
		llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "tool_result", Text: "tool output"}}},
	)
	got2 := l.hydrateSpeakerProfile(context.Background(), roundTrip2)
	assert.Equal(t, 1, calls, "a round-trip within the SAME turn must not retry a failed fetch")
	assert.Len(t, got2[2].Content, 1)

	// A genuinely new turn from the SAME speaker — the fetch decision resets
	// for the new head index and retries, since nothing was ever
	// successfully emitted for dana.
	turn2 := append(append([]llm.Message{}, turn1...),
		llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: "text", Text: "earlier answer 2"}}},
		llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "second question"}}},
	)
	l.lastInboundAuthor = dana // drainInbox would set this again to the same value
	got3 := l.hydrateSpeakerProfile(context.Background(), turn2)
	assert.Equal(t, 2, calls, "a genuinely new turn must retry the fetch, even for the speaker whose last attempt failed")
	require.Len(t, got3, 5)
	assert.Len(t, got3[4].Content, 1, "the retry also failed (same erroring fetcher), so still no block")
}

// TestHydrateSpeakerProfileLogsOnlyTransportErrors pins the AGENTS.md
// "never silently drop an error" split: an ordinary miss (a guest, a
// foreign-workspace user) must NOT log — logging it would spam a line on
// every request from such a user — while a transport error, the one failure
// here an operator can actually act on, MUST log.
func TestHydrateSpeakerProfileLogsOnlyTransportErrors(t *testing.T) {
	t.Run("not found: no log line", func(t *testing.T) {
		buf := captureSlog(t)
		l := &Loop{
			lastInboundAuthor:    authorSubject(t, "dana@example.com"),
			SpeakerProfileFields: userprofile.DefaultFields(),
			FetchSpeakerProfile: func(context.Context, string) (userprofile.Profile, error) {
				return userprofile.Profile{}, channelkinds.ErrProfileNotFound
			},
		}
		l.hydrateSpeakerProfile(context.Background(), msgsWithUserTurn())
		assert.Empty(t, buf.String(), "an ordinary miss must not be logged")
	})

	t.Run("transport error: logs", func(t *testing.T) {
		buf := captureSlog(t)
		l := &Loop{
			lastInboundAuthor:    authorSubject(t, "dana@example.com"),
			SpeakerProfileFields: userprofile.DefaultFields(),
			FetchSpeakerProfile: func(context.Context, string) (userprofile.Profile, error) {
				return userprofile.Profile{}, errors.New("slack: 503")
			},
		}
		l.hydrateSpeakerProfile(context.Background(), msgsWithUserTurn())
		assert.Contains(t, buf.String(), "speaker profile fetch failed", "a transport error must be logged")
	})
}
