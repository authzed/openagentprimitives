package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// SyncKind is the relsync.Kind for the Slack directory: it walks
// conversations.list / conversations.members / users.info and turns
// Slack's channel/user graph into the tuples DirectorySyncSource
// (directory_sync_source.go) claims. Registered under KindName ("slack") —
// the same string channelkinds.Kind uses, since both name the connector a
// spec.kind selects, not two different things that happen to share a
// string.
//
// slack_bot#agent is deliberately NOT written here, and deliberately NOT
// added to DirectorySyncSource.Claims: this kind syncs human members only.
// A bot member surfaced by conversations.members is excluded outright (see
// FetchScope) rather than counted as a join miss — it isn't an
// identity-join failure, it's a member kind this sync doesn't sync yet.
// Wiring slack_bot up to a real Agent identity is future work.
//
// slack_workspace#member is derived, not independently enumerated: this
// kind never calls users.list, so a workspace member invisible in every
// channel this bot's token can see is never asserted a workspace member at
// all. Every resolved channel member IS asserted a workspace member (Slack
// has no channel membership without workspace membership), so coverage is
// exactly the union of members across every channel scope this kind
// fetches — full for a bot present in every channel, partial otherwise.
type SyncKind struct {
	// userInfoCache memoizes users.info responses across FetchScope calls
	// that share the same credential — see userInfoCache's own doc.
	userInfoCache slackUserInfoCache
}

func init() { relsync.Register(&SyncKind{}) }

// Name is the spec.kind value that selects this kind.
func (k *SyncKind) Name() string { return KindName }

// Source returns the already-registered DirectorySyncSource
// (directory_sync_source.go) — one Source per connector, not one per
// Kind, since a future usergroup/DM sync engine for this same connector
// would still be "the Slack directory sync" and must claim from the same
// list.
func (k *SyncKind) Source() relsource.Source { return DirectorySyncSource }

var _ relsync.ScopeLabeler = (*SyncKind)(nil)

// ScopeLabelBridges points the console at the #label tuple FetchScope writes,
// so a Directory row reads `demo-channel` instead of
// `slack_channel:C08TUFTDYTC` — see relsync.ScopeLabeler.
//
// This kind stores its own name rather than pointing at a URL-keyed object the
// way the GitHub kind does, because it had nothing of the sort: the channel
// name arrives on every conversations.info call FetchScope already makes and
// used to be thrown away. Hence spicedb.ShapeNameOnSubject — the scope is the
// tuple's RESOURCE and the encoded name is its subject.
//
// slack_channel is the only entry because it is the only type this kind
// enumerates as a scope (ListScopes). slack_workspace and slack_usergroup have
// no rows on this panel to name.
//
// The decoder is DecoderB64Text, not DecoderB64URL: the payload is text a
// person typed into Slack, so it resolves to a title and never to an href. A
// channel named after a URL is therefore rendered as text, not as a link
// somebody else chose the target of.
func (k *SyncKind) ScopeLabelBridges() []spicedb.ScopeLabelBridge {
	return []spicedb.ScopeLabelBridge{{
		ScopeDefinition:  slackChannelResourceType,
		BridgeDefinition: relsource.LabelSubjectType,
		BridgeRelation:   relsource.LabelRelation,
		Shape:            spicedb.ShapeNameOnSubject,
		Decoder:          resourcedisplay.DecoderB64Text,
	}}
}

// slack_channel is the only relsync.Scope.ResourceType this kind ever
// enumerates. slack_workspace and slack_user tuples ride as FetchScope
// cross-resource content (sync.go's "Ruling 4": a fetch may assert a tuple
// on a resource other than the scope's own object) and are never scopes of
// their own.
const slackChannelResourceType = "slack_channel"
const slackWorkspaceResourceType = "slack_workspace"
const slackUserResourceType = "slack_user"

// conversationsListPageSize bounds one conversations.list call. Slack's
// documented max is 1000; a smaller page keeps one retry's blast radius
// small on a large workspace.
const conversationsListPageSize = 200

// slackDirectoryClient is the slack-go subset ListScopes/FetchScope need.
// *slackapi.Client satisfies it directly — conversations.list/.members/
// .info, users.info and auth.test are all real methods on it, so ONE
// client built from the resolved credential answers every call this kind
// makes, rather than a second client construction path for the users.info
// half (mirrors historyClient/userNamer's own single-client seam
// elsewhere in this package).
type slackDirectoryClient interface {
	GetConversationsContext(ctx context.Context, params *slackapi.GetConversationsParameters) (channels []slackapi.Channel, nextCursor string, err error)
	GetConversationInfoContext(ctx context.Context, input *slackapi.GetConversationInfoInput) (*slackapi.Channel, error)
	GetUsersInConversationContext(ctx context.Context, params *slackapi.GetUsersInConversationParameters) ([]string, string, error)
	GetUserInfoContext(ctx context.Context, user string) (*slackapi.User, error)
	AuthTestContext(ctx context.Context) (*slackapi.AuthTestResponse, error)
}

// slackUserInfoCacheMax bounds slackUserInfoCache's total entries. A crude
// cap rather than an LRU: this cache's whole job is "don't call users.info
// twice for the same member in one pass" (a member can sit in dozens of
// channels a pass walks sequentially), not long-term identity storage, so
// hitting the cap and resetting costs at most one extra call per member
// per workspace the next time each is needed.
const slackUserInfoCacheMax = 8192

// slackUserInfoCache memoizes users.info responses across FetchScope calls
// that share the same credential — the exact case the resumability design
// exists to survive Slack's rate limits for: without it, a member present
// in 40 channels is resolved 40 times in one pass. Keyed by a fingerprint
// of the resolved token (never the token bytes themselves) plus the Slack
// user id, so two different workspaces sharing this package's one
// registered *SyncKind singleton (see relsync.Get) never see each other's
// cached profile.
//
// Entries are not otherwise invalidated between passes — a user's email or
// bot flag changes rarely enough that a bounded, unexpired cache is the
// same trade-off identity.go's IdentityCache already makes for the live
// listener's users.info lookups. auth.test's team id is deliberately NOT
// cached this way (see directoryClientFactory's callers in FetchScope):
// a wrong team id silently produces tuples on the WRONG workspace, whereas
// a stale cached profile here is bounded staleness of the same kind this
// package already accepts elsewhere.
type slackUserInfoCache struct {
	mu   sync.Mutex
	byID map[string]*slackapi.User
}

func (c *slackUserInfoCache) get(fingerprint, userID string) (*slackapi.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.byID[fingerprint+":"+userID]
	return u, ok
}

func (c *slackUserInfoCache) put(fingerprint, userID string, u *slackapi.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]*slackapi.User{}
	}
	if len(c.byID) >= slackUserInfoCacheMax {
		c.byID = map[string]*slackapi.User{}
	}
	c.byID[fingerprint+":"+userID] = u
}

// tokenFingerprint derives a cache-scoping key from a credential's raw
// token bytes without ever storing or comparing the token itself: a
// one-way hash is exactly as good for "is this the same credential as
// last time" as the plaintext would be, with none of the handling risk.
func tokenFingerprint(tok []byte) string {
	sum := sha256.Sum256(tok)
	return hex.EncodeToString(sum[:])
}

// directoryClientFactory builds the client ListScopes/FetchScope use from
// the resolved upstream credential. Overridden in tests to point a real
// *slackapi.Client at an httptest server (slackapi.OptionAPIURL) rather
// than the shared fakeslack double: fakeslack answers per Go method call
// with no notion of "page 2 of this call fails", which is exactly the
// atomicity property FetchScope must prove.
var directoryClientFactory = func(creds relsync.SourceParams) slackDirectoryClient {
	return slackapi.New(string(creds.Token.UnderlyingValue()))
}

// ListScopes pages conversations.list, restricted to public and private
// channels (conversations.list's own "types" filter excludes IMs/MPIMs,
// which are not directory scopes) and excluding already-archived channels:
// an archived channel simply drops out of enumeration and is reaped by the
// next complete pass, the same as a deleted one.
func (k *SyncKind) ListScopes(ctx context.Context, creds relsync.SourceParams, after relsync.Cursor) (relsync.ScopePage, error) {
	token, err := after.ForKind(k.Name())
	if err != nil {
		return relsync.ScopePage{}, err
	}

	cli := directoryClientFactory(creds)
	fp := tokenFingerprint(creds.Token.UnderlyingValue())
	if err := directoryPacer.wait(ctx, fp, methodConversationsList); err != nil {
		return relsync.ScopePage{}, fmt.Errorf("slack conversations.list: pace: %w", err)
	}
	channels, nextCursor, err := cli.GetConversationsContext(ctx, &slackapi.GetConversationsParameters{
		Cursor:          token,
		Types:           []string{"public_channel", "private_channel"},
		ExcludeArchived: true,
		Limit:           conversationsListPageSize,
	})
	if err != nil {
		return relsync.ScopePage{}, fmt.Errorf("slack conversations.list: %w", withRetryAfter(err))
	}

	scopes := make([]relsync.Scope, 0, len(channels))
	for _, ch := range channels {
		scopes = append(scopes, relsync.Scope{ID: relsync.ScopeID(ch.ID), ResourceType: slackChannelResourceType})
	}

	// The zero Cursor{} (not one merely carrying an empty Token) means
	// "exhausted" to enumerate()'s == comparison, so a finished listing
	// must leave Next entirely unset, not tagged-with-an-empty-token.
	var next relsync.Cursor
	if nextCursor != "" {
		next = relsync.Cursor{Kind: k.Name(), Token: nextCursor}
	}
	return relsync.ScopePage{
		Scopes: scopes,
		Next:   next,
		// A non-zero Next cursor means, by definition, more scopes remain
		// upstream that this page did not enumerate — Complete is false
		// exactly then, never by reflex true (see kind.go's ScopePage.Complete
		// doc and TestSlackKind_TruncatedListReportsIncomplete).
		Complete: nextCursor == "",
	}, nil
}

// FetchScope resolves one channel's current membership: conversations.info
// for the public/private split (and to notice a since-deleted channel),
// conversations.members paged to completion, and one users.info per member
// to join it to a platform user by canonicalized email. Every step either
// succeeds outright or the whole call returns an error and an empty
// ScopeContent — see relsync.Kind.FetchScope's own atomicity doc and
// TestSlackKind_FetchScopeIsAtomicAcrossPages.
func (k *SyncKind) FetchScope(ctx context.Context, creds relsync.SourceParams, s relsync.Scope) (relsync.ScopeContent, error) {
	cli := directoryClientFactory(creds)
	channelID := string(s.ID)
	// The per-workspace pacing key, computed once and reused for every Slack
	// call this scope makes (and for the users.info cache lookups below).
	fp := tokenFingerprint(creds.Token.UnderlyingValue())

	if err := directoryPacer.wait(ctx, fp, methodConversationsInfo); err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("slack conversations.info: pace: %w", err)
	}
	info, err := cli.GetConversationInfoContext(ctx, &slackapi.GetConversationInfoInput{ChannelID: channelID})
	if err != nil {
		if isChannelNotFound(err) {
			return relsync.ScopeContent{}, relsync.ErrScopeGone
		}
		return relsync.ScopeContent{}, fmt.Errorf("slack conversations.info: %w", withRetryAfter(err))
	}
	if info == nil {
		return relsync.ScopeContent{}, fmt.Errorf("slack conversations.info(%s): empty response", channelID)
	}

	memberIDs, err := fetchAllConversationMembers(ctx, cli, fp, channelID)
	if err != nil {
		if isChannelNotFound(err) {
			return relsync.ScopeContent{}, relsync.ErrScopeGone
		}
		return relsync.ScopeContent{}, fmt.Errorf("slack conversations.members: %w", withRetryAfter(err))
	}

	if err := directoryPacer.wait(ctx, fp, methodAuthTest); err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("slack auth.test: pace: %w", err)
	}
	auth, err := cli.AuthTestContext(ctx)
	if err != nil {
		return relsync.ScopeContent{}, fmt.Errorf("slack auth.test: %w", withRetryAfter(err))
	}
	if auth == nil || auth.TeamID == "" {
		return relsync.ScopeContent{}, errors.New("slack auth.test: no team id reported")
	}
	teamID := auth.TeamID

	var tuples []spicedb.Tuple
	var joinMisses int
	for _, uid := range memberIDs {
		user, cached := k.userInfoCache.get(fp, uid)
		if !cached {
			if err := directoryPacer.wait(ctx, fp, methodUsersInfo); err != nil {
				return relsync.ScopeContent{}, fmt.Errorf("slack users.info(%s): pace: %w", uid, err)
			}
			user, err = cli.GetUserInfoContext(ctx, uid)
			if err != nil {
				return relsync.ScopeContent{}, fmt.Errorf("slack users.info(%s): %w", uid, withRetryAfter(err))
			}
			if user == nil {
				return relsync.ScopeContent{}, fmt.Errorf("slack users.info(%s): empty response", uid)
			}
			k.userInfoCache.put(fp, uid, user)
		}
		if user.IsBot {
			// See SyncKind's own doc: bot membership is excluded outright,
			// not counted as a join miss — it is a member kind this sync
			// does not sync, not an identity-join failure.
			continue
		}
		email := strings.TrimSpace(user.Profile.Email)
		if email == "" {
			// Resolves to nobody: dropped, not written, but counted — see
			// ScopeContent.JoinMisses' own doc on why a silent partial
			// membership is exactly what the no-silent-errors rule forbids.
			joinMisses++
			continue
		}
		// FromExternal, not EmailReference: this email was read from Slack's
		// own users.info inside the installed workspace — FromExternal's own
		// doc names exactly that as its channel-verified case, matching
		// every other Slack identity-join site in this package (app_home.go,
		// listener.go, sender_user_echo.go, schema_fragment.go's
		// SessionOwner). EmailReference is for configuration/reference data,
		// not a channel-attributed login — using it here documented this
		// join as unproven when it plainly wasn't. The two encode a
		// non-empty email to a byte-identical CanonicalUserID (verified-ness
		// is dropped from the value itself), so this is a self-documentation
		// fix, not a behavior change.
		canonical, err := identity.FromExternal(identity.KindSlack, identity.TeamScope(teamID), identity.RawExternalID(uid), identity.Email(email)).Canonical()
		if err != nil {
			// Unreachable today (Principal.Canonical only errors on an
			// empty email with no synthetic opt-in, and FromExternal marks
			// emailVerified — hence takes the email branch — whenever email
			// is non-empty, which it is here) — fail the whole scope rather
			// than silently drop, in case that contract ever changes under
			// us.
			return relsync.ScopeContent{}, fmt.Errorf("slack: canonicalize member %s email: %w", uid, err)
		}

		// The identity link: cross-resource (Ruling 4) since this scope is
		// slack_channel:<channelID>, not slack_user:<uid>.
		tuples = append(tuples, spicedb.Tuple{
			ResourceType: slackUserResourceType,
			ResourceID:   uid,
			Relation:     "user",
			SubjectType:  "user",
			SubjectID:    canonical.String(),
		})
		// The channel membership: this scope's own resource. The subject is
		// a userset (slack_user:<uid>#user), matching slack_channel.member's
		// `slack_user#user | slack_bot#agent` union in schema_fragment.go.
		tuples = append(tuples, spicedb.Tuple{
			ResourceType:    slackChannelResourceType,
			ResourceID:      channelID,
			Relation:        "member",
			SubjectType:     slackUserResourceType,
			SubjectID:       uid,
			SubjectRelation: "user",
		})
		// The workspace membership: cross-resource, same reasoning as the
		// identity link — every channel member is necessarily a workspace
		// member, so this rides on every channel scope this kind fetches
		// (see SyncKind's own doc on what that does and does not cover).
		tuples = append(tuples, spicedb.Tuple{
			ResourceType:    slackWorkspaceResourceType,
			ResourceID:      teamID,
			Relation:        "member",
			SubjectType:     slackUserResourceType,
			SubjectID:       uid,
			SubjectRelation: "user",
		})
	}

	// The channel's own name, for the console's Directory panel. It arrives on
	// the conversations.info call above — which this kind has always made, and
	// whose name it has always discarded — so naming a row costs no extra
	// request. Ordinary scope content, hashed and diffed with everything else;
	// see relsync.ScopeLabelTuple for why that, and not the sentinel's
	// machinery, is what makes an already-synced channel pick up a name.
	if labelTuple, ok := relsync.ScopeLabelTuple(ctx, s, info.Name); ok {
		tuples = append(tuples, labelTuple)
	}

	if !info.IsPrivate {
		// Written ONLY for public channels: this is what makes
		// `view = member + workspace->member` (schema_fragment.go) give a
		// public channel's audience the whole workspace and a private
		// channel's audience only its own members.
		tuples = append(tuples, spicedb.Tuple{
			ResourceType: slackChannelResourceType,
			ResourceID:   channelID,
			Relation:     "workspace",
			SubjectType:  slackWorkspaceResourceType,
			SubjectID:    teamID,
		})
	}

	return relsync.ScopeContent{Tuples: tuples, JoinMisses: joinMisses}, nil
}

// fetchAllConversationMembers pages conversations.members to completion.
// An error on any page — including the first — returns nothing
// accumulated so far: FetchScope's caller must never see a partial
// membership list, only a whole one or an error.
func fetchAllConversationMembers(ctx context.Context, cli slackDirectoryClient, fp, channelID string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		if err := directoryPacer.wait(ctx, fp, methodConversationsMembers); err != nil {
			return nil, fmt.Errorf("pace: %w", err)
		}
		page, next, err := cli.GetUsersInConversationContext(ctx, &slackapi.GetUsersInConversationParameters{
			ChannelID: channelID,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, page...)
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}

// slackRetryAfterError adapts slack-go's *slackapi.RateLimitedError — which
// carries its backoff as a FIELD (RetryAfter time.Duration), not a method —
// into pkg/controllers/relationshipsource's kind-agnostic RetryAfter
// interface (RetryAfter() time.Duration). That controller stays generic on
// purpose (AGENTS.md: "a new variant gets registered, not branched on" — a
// kind-specific type switch in the reconciler is exactly the smell the rule
// forbids), so the adaptation belongs HERE, in the one kind package that
// knows slack-go's error shape, not there.
//
// Unwrap preserves the original error in the chain: errors.Is/As against
// the raw *slackapi.RateLimitedError (or channel_not_found substring checks
// elsewhere) still see through this wrapper exactly as before.
type slackRetryAfterError struct {
	err        error
	retryAfter time.Duration
}

func (e *slackRetryAfterError) Error() string             { return e.err.Error() }
func (e *slackRetryAfterError) Unwrap() error             { return e.err }
func (e *slackRetryAfterError) RetryAfter() time.Duration { return e.retryAfter }

// minRateLimitBackoff is what a rate limit reports when Slack said it was
// throttling but named no number — a bare 429, or the ratelimited error code.
// Mirrors the github kind's constant of the same name, for the same reason:
// we are inventing this duration, and inventing a zero would requeue
// immediately, which is precisely the hammering the backoff exists to stop.
// A second says "throttled, but the window is already open".
//
// It floors the header path too. When Slack DOES name a number we honour it,
// and a literal `Retry-After: 0` is the only case the floor changes — from
// "retry now" to "retry in a second", which costs one second and cannot make
// a throttled source worse. What the floor never touches is WHETHER a limit
// was reported: retryAfterFrom keys the mid-cycle exemption off the presence
// of a RetryAfter-satisfying error, not off its duration, so a zero that
// reaches it is still "upstream said stop".
const minRateLimitBackoff = time.Second

// withRetryAfter wraps err so a Slack rate limit anywhere in its chain becomes
// visible to errors.As against the RetryAfter method interface — see
// slackRetryAfterError's own doc. An err that is not a rate limit passes
// through unchanged; this must be called at every Slack API call site
// relsync's ListScopes/FetchScope make, or a rate limit on that specific call
// silently loses its backoff hint.
func withRetryAfter(err error) error {
	if d, ok := rateLimitBackoff(err); ok {
		return &slackRetryAfterError{err: err, retryAfter: d}
	}
	return err
}

// rateLimitBackoff reports how long to wait, and whether Slack said it was
// throttling at all. The second return is the load-bearing one: the reconciler
// decides whether a MID-CYCLE pass may call upstream from the mere presence of
// a RetryAfter-satisfying error, so an unrecognized rate limit is not a
// slightly-wrong delay, it is a pass that sails through the hold entirely.
//
// Slack says "slow down" in three shapes and slack-go only names one of them:
//
//   - A 429 WITH a Retry-After header becomes *slackapi.RateLimitedError.
//     slack-go's misc.go guards on the header's presence, so this is the only
//     shape it builds that type for.
//   - A 429 with NO Retry-After falls through to a plain
//     slackapi.StatusCodeError{Code: 429}. This is the shape a live run hit
//     999 times in five minutes; honouring only the first would leave it
//     indistinguishable from an ordinary failure.
//   - An ordinary 200 whose body is {"ok":false,"error":"ratelimited"} — no
//     HTTP status to key on at all. users.info answers this way, and
//     FetchScope calls users.info for every member of every scope.
func rateLimitBackoff(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var rl *slackapi.RateLimitedError
	if errors.As(err, &rl) {
		if rl.RetryAfter < minRateLimitBackoff {
			return minRateLimitBackoff, true
		}
		return rl.RetryAfter, true
	}
	var sce slackapi.StatusCodeError
	if errors.As(err, &sce) && sce.Code == http.StatusTooManyRequests {
		return minRateLimitBackoff, true
	}
	if hasRateLimitedCode(err) {
		return minRateLimitBackoff, true
	}
	return 0, false
}

// hasRateLimitedCode reports whether err carries Slack's rate-limit ERROR CODE
// — the rate limit that arrives as an ordinary 200. Mirrors isChannelNotFound
// below: a plain substring check on the Slack error code, since slack-go's
// SlackErrorResponse.Error() returns that code verbatim with no wrapping.
// Both spellings are accepted: the Web API answers "ratelimited", the Events
// and RTM surfaces "rate_limited".
//
// Deliberately NOT the same function as this package's isRateLimited (see
// stream_delta_sink.go), which also treats any error text containing "429" as
// a rate limit. That is right for a message-delivery retry, where a false
// positive costs one re-send. Here a false positive puts a whole
// RelationshipSource to sleep, and "429" is short enough to appear in a
// channel ID or a Slack timestamp, so this one keys only on the code.
func hasRateLimitedCode(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ratelimited") || strings.Contains(msg, "rate_limited")
}

// isChannelNotFound reports whether err is Slack's channel_not_found — the
// signal that a channel has been deleted, distinct from merely archived
// (an archived channel is excluded from ListScopes but still answers
// conversations.info/.members normally). Mirrors lookup.go's
// isUsersNotFound: a plain substring check on the Slack error code, since
// slack-go's SlackErrorResponse.Error() returns that code verbatim with no
// wrapping.
func isChannelNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "channel_not_found")
}
