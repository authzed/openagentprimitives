// pkg/channels/channelkinds/slack/lookup.go
//
// Slack implementation of Kind.LookupUser — resolves email/name/any to
// a Slack user_id + display name for use by lookup_user_for_mention.
// Caches:
//   - identityCache (LRU): email → user_id, hit on second lookup of same
//     user in this process. Shared instance per *Kind.
//   - memberCache (TTL): full users.list snapshot used by the name path.
//     Rate-limit driven (users.list is Tier 2; 20/min).
package slack

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// slackMentionToolDescription is the text the lookup_user_for_mention
// tool surfaces to the LLM. Speaks Slack-specifics (mrkdwn token,
// notification semantics) so the runner's generic ComposeSystem stays
// channel-agnostic per the repo's memory.
const slackMentionToolDescription = `Resolve a user identifier to a Slack mention token (` + "`<@USERID>`" + `) for use inside respond_to_user.text. Use this ONLY when you actually want to ping the user (they will be notified). For prose references where no notification is desired, write the user's name or email as plain text — do not call this tool. ` +
	"`kind`: `\"email\"` — exact match via Slack `users.lookupByEmail`; `\"name\"` — case-insensitive match against display/real name; deactivated and bot accounts are never matched, and when several active users share the name the active full member (employee) is preferred over guests; `\"any\"` — heuristically routes email (contains `@`) vs. name. On hit, the tool returns the literal token (e.g. `<@U01234>`) — paste it verbatim into your message text. When a name cannot be resolved to one person — because it matches nobody exactly (near matches such as a nickname, \"Freddie\" for \"Frederick\") or because several people share it — the tool returns a short list of candidates, each with its token and account standing (member/guest); use a token only if that candidate is clearly the person you mean, preferring the full member; when unsure, or on a plain miss, fall back to plain text in your reply."

const identityCacheCapacity = 256

// errNoAPIClient is returned when the credentials Secret yielded no usable
// Slack client — surfaced rather than silently degraded so a misconfigured
// Channel is diagnosable from logs (AGENTS.md: never silently drop errors).
var errNoAPIClient = errors.New("slack: no API client (credentials Secret missing bot-token?)")

// lookupUser is the Kind.LookupUser body. Delegates by kind, leaning on
// the per-Kind caches and a slackClient built from deps.Secret. Returns
// channelkinds sentinel errors so the generic tool can map them to
// user-friendly IsError messages.
func (k *Kind) lookupUser(
	ctx context.Context, deps channelkinds.LookupDeps,
	kind channelkinds.MentionLookupKind, value string,
) (string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", channelkinds.ErrMentionNotFound
	}
	k.resolverInit()
	cli := k.clientFactory(deps)
	if cli == nil {
		return "", "", errNoAPIClient
	}

	switch kind {
	case channelkinds.MentionLookupEmail:
		return k.lookupByEmail(ctx, cli, value)
	case channelkinds.MentionLookupName:
		return k.lookupByName(ctx, cli, value)
	case channelkinds.MentionLookupAny:
		if strings.Contains(value, "@") {
			return k.lookupByEmail(ctx, cli, value)
		}
		return k.lookupByName(ctx, cli, value)
	default:
		return "", "", channelkinds.ErrMentionUnsupported
	}
}

func (k *Kind) resolverInit() {
	k.resolverOnce.Do(func() {
		k.identityCache = NewIdentityCache(identityCacheCapacity)
		k.memberCache = &memberSnapshot{}
		if k.clientFactory == nil {
			k.clientFactory = defaultClientFactory
		}
	})
}

func defaultClientFactory(deps channelkinds.LookupDeps) slackClient {
	return newSlackAPIClient(deps.Secret)
}

func (k *Kind) lookupByEmail(ctx context.Context, cli slackClient, email string) (string, string, error) {
	if cached, ok := k.identityCache.GetByEmail(email); ok {
		return cached.UserID, cached.DisplayName, nil
	}
	u, err := cli.GetUserByEmailContext(ctx, email)
	if err != nil {
		if isUsersNotFound(err) {
			return "", "", channelkinds.ErrMentionNotFound
		}
		return "", "", err
	}
	if u == nil {
		return "", "", channelkinds.ErrMentionNotFound
	}
	disp := preferredDisplayName(u)
	k.identityCache.Put(userInfo{
		UserID:      u.ID,
		Email:       u.Profile.Email,
		TeamID:      u.TeamID,
		DisplayName: disp,
	})
	return u.ID, disp, nil
}

// memberSnapshotTTL controls how long the cached users.list result is
// reused. users.list is Tier 2 (20 req/min); 60s gives an order-of-
// magnitude safety margin while keeping the snapshot fresh enough that
// new joiners are found promptly.
const memberSnapshotTTL = 60 * time.Second

// memberSnapshot caches the users.list result with a TTL. Stale-while-
// revalidate would be nice but is overkill for v1 — name lookups are
// best-effort, and the TTL is short.
type memberSnapshot struct {
	mu       sync.Mutex
	users    []slackapi.User
	loadedAt time.Time
	ttl      time.Duration // override for tests; zero falls back to memberSnapshotTTL
}

func (m *memberSnapshot) effectiveTTL() time.Duration {
	if m.ttl > 0 {
		return m.ttl
	}
	return memberSnapshotTTL
}

// load returns the cached snapshot, refreshing if it is empty or
// expired. Concurrent callers serialize on m.mu; on miss exactly one
// request hits Slack. Errors are returned to the caller; the cache is
// NOT poisoned with empty results.
func (m *memberSnapshot) load(ctx context.Context, cli slackClient) ([]slackapi.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 && time.Since(m.loadedAt) < m.effectiveTTL() {
		return m.users, nil
	}
	users, err := cli.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	m.users = users
	m.loadedAt = time.Now()
	return m.users, nil
}

func (k *Kind) lookupByName(ctx context.Context, cli slackClient, name string) (string, string, error) {
	users, err := k.memberCache.load(ctx, cli)
	if err != nil {
		return "", "", err
	}
	want := strings.ToLower(strings.TrimSpace(name))
	// First pass: match on display_name (case-insensitive, exact).
	matches := pickMatches(users, want, func(u slackapi.User) string {
		return strings.ToLower(strings.TrimSpace(u.Profile.DisplayName))
	})
	if len(matches) == 0 {
		// Second pass: real_name fallback.
		matches = pickMatches(users, want, func(u slackapi.User) string {
			return strings.ToLower(strings.TrimSpace(u.Profile.RealName))
		})
	}
	if len(matches) == 0 {
		// Third pass: no exact match among active accounts — offer
		// near-miss candidates for the agent to judge (lookup_nearmiss.go).
		if cands := nearMissCandidates(users, want); len(cands) > 0 {
			return "", "", &channelkinds.MentionNearMissError{Candidates: cands}
		}
		return "", "", channelkinds.ErrMentionNotFound
	}
	if len(matches) == 1 {
		u := matches[0]
		return u.ID, preferredDisplayName(&u), nil
	}
	// Several active accounts share the name. Prefer a lone full member
	// (an employee) over any guests: a wrong mention pings a real person,
	// and when one match is a member and the rest are guests the member is
	// almost always who was meant. When the preference cannot single one
	// out — two members, or only guests — surface all of them, member-
	// first, as candidates for the model to choose, rather than a bare
	// ambiguity error it cannot act on.
	if members := membersOf(matches); len(members) == 1 {
		u := members[0]
		return u.ID, preferredDisplayName(&u), nil
	}
	return "", "", &channelkinds.MentionNearMissError{Exact: true, Candidates: rankedCandidates(matches)}
}

// pickMatches returns the subset of ACTIVE, human users whose extracted
// key equals want. Deactivated (Deleted) and bot accounts are excluded: a
// name lookup exists to ping a person, and a former employee's deactivated
// account — or a bot that happens to share a display name — is never the
// intended target. Guests are kept (they are real, active people) but the
// caller deprioritizes them; see rankedCandidates.
func pickMatches(users []slackapi.User, want string, key func(slackapi.User) string) []slackapi.User {
	if want == "" {
		return nil
	}
	out := make([]slackapi.User, 0, 2)
	for _, u := range users {
		if u.Deleted || u.IsBot {
			continue
		}
		if key(u) == want {
			out = append(out, u)
		}
	}
	return out
}

// mentionStanding is the standing label shown to the model for a name
// candidate, kept consistent with the member/guest ranking membersOf
// applies: anything limitedAccount treats as not-a-full-member (a guest,
// or a shared-channel stranger) is "guest"; otherwise accountTypeOf's
// label ("member"/"admin"/"owner"). Deleted and bot accounts never reach
// here — the exact and near-miss passes drop them — so this cannot label
// one of those. Descriptive only, per accountTypeOf's contract.
func mentionStanding(u *slackapi.User) string {
	if limitedAccount(u) {
		return "guest"
	}
	return accountTypeOf(u)
}

// membersOf returns the subset that are full workspace members — not
// guests or shared-channel strangers. Callers pass only already-active,
// non-bot matches (pickMatches filters those), so this reduces to
// !limitedAccount, reusing the same standing predicate the session-start
// gate uses (listener.go) rather than a second definition of "employee".
func membersOf(users []slackapi.User) []slackapi.User {
	out := make([]slackapi.User, 0, len(users))
	for i := range users {
		if !limitedAccount(&users[i]) {
			out = append(out, users[i])
		}
	}
	return out
}

// rankedCandidates orders name matches members-first (then by user ID for
// determinism) and converts them to MentionCandidates, so the model sees
// the most-likely-intended (employee) accounts at the top of the list.
func rankedCandidates(matches []slackapi.User) []channelkinds.MentionCandidate {
	ranked := append([]slackapi.User(nil), matches...)
	sort.Slice(ranked, func(i, j int) bool {
		li, lj := limitedAccount(&ranked[i]), limitedAccount(&ranked[j])
		if li != lj {
			return !li // members (not limited) sort ahead of guests
		}
		return ranked[i].ID < ranked[j].ID
	})
	out := make([]channelkinds.MentionCandidate, 0, len(ranked))
	for i := range ranked {
		out = append(out, channelkinds.MentionCandidate{
			ExternalID:  ranked[i].ID,
			DisplayName: preferredDisplayName(&ranked[i]),
			AccountType: mentionStanding(&ranked[i]),
		})
	}
	return out
}

// preferredDisplayName picks the display string Slack would render in
// the message UI: profile.display_name if set, else profile.real_name,
// else the bare user ID. The same precedence is used by the name path
// so matches are symmetric.
func preferredDisplayName(u *slackapi.User) string {
	if u == nil {
		return ""
	}
	if d := strings.TrimSpace(u.Profile.DisplayName); d != "" {
		return d
	}
	if d := strings.TrimSpace(u.Profile.RealName); d != "" {
		return d
	}
	return u.ID
}

// isUsersNotFound checks Slack's "users_not_found" error code without
// pinning to a specific error type — slack-go has flipped between
// SlackErrorResponse, slackapi.Error and an opaque string at various
// versions.
func isUsersNotFound(err error) bool {
	if err == nil {
		return false
	}
	type errCoded interface{ Error() string }
	var coded errCoded = err
	return strings.Contains(strings.ToLower(coded.Error()), "users_not_found")
}
