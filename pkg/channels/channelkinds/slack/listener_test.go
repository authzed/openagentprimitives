package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestEventIDCacheAddDeduplicates verifies that Add returns true on first
// insertion and false on subsequent insertions of the same key.
func TestEventIDCacheAddDeduplicates(t *testing.T) {
	c := newEventIDCache(8)
	assert.True(t, c.Add("a"), "first add should return true")
	assert.False(t, c.Add("a"), "second add of same key should return false")
	assert.True(t, c.Add("b"), "different key should return true")
}

// TestEventIDCacheLRUEviction verifies that the LRU eviction policy works:
// touching a key moves it to front so the un-touched key gets evicted when
// capacity is exceeded.
func TestEventIDCacheLRUEviction(t *testing.T) {
	c := newEventIDCache(2)
	c.Add("a")
	c.Add("b")
	c.Add("a") // touch a → b is now LRU
	c.Add("c") // evicts b
	assert.False(t, c.Add("a"), "a should still be cached")
	assert.False(t, c.Add("c"), "c should still be cached")
	assert.True(t, c.Add("b"), "b should have been evicted")
}

// TestThreadIndexHas verifies basic put/has semantics.
func TestThreadIndexHas(t *testing.T) {
	idx := newThreadIndex()
	assert.False(t, idx.has("c1:t1"), "empty index should not have anything")
	idx.put("c1:t1", "")
	assert.True(t, idx.has("c1:t1"), "after put, has should return true")
	assert.False(t, idx.has("c1:t2"), "different key should not be present")
}

// TestThreadIndexRoutingMode verifies put stores and lookup returns the
// routing mode, and that a non-empty mode is never downgraded.
func TestThreadIndexRoutingMode(t *testing.T) {
	idx := newThreadIndex()
	idx.put("c1:adopted", "mention_only")
	idx.put("c1:plain", "")

	mode, _, ok := idx.lookup("c1:adopted")
	assert.True(t, ok)
	assert.Equal(t, "mention_only", mode)

	mode, _, ok = idx.lookup("c1:plain")
	assert.True(t, ok)
	assert.Equal(t, "", mode)

	_, _, ok = idx.lookup("c1:missing")
	assert.False(t, ok)

	// A later put("") must not downgrade a mention_only entry.
	idx.put("c1:adopted", "")
	mode, _, _ = idx.lookup("c1:adopted")
	assert.Equal(t, "mention_only", mode, "non-empty routing mode must not be downgraded")

	// A later non-empty put upgrades an entry first seen as "".
	idx.put("c1:plain", "mention_only")
	mode, _, _ = idx.lookup("c1:plain")
	assert.Equal(t, "mention_only", mode, "empty entry should upgrade")
}

// A refusal is remembered so the apiserver lookup behind it costs one
// round-trip per thread, not one per message — and a later positive claim
// (this listener was summoned into the thread) must clear it.
func TestThreadIndexRefusalIsCachedAndSupersededByAClaim(t *testing.T) {
	idx := newThreadIndex()
	idx.putRefused("c1:foreign")

	_, refused, ok := idx.lookup("c1:foreign")
	assert.True(t, ok, "a refusal is a resolved thread, so later messages skip the lookup")
	assert.True(t, refused)

	idx.put("c1:foreign", "mention_only")
	mode, refused, ok := idx.lookup("c1:foreign")
	require.True(t, ok)
	assert.False(t, refused, "being summoned into the thread supersedes the refusal")
	assert.Equal(t, "mention_only", mode)
}

// TestThreadIndexPutIdempotent verifies that putting the same key twice does
// not grow the index beyond one entry for that key.
func TestThreadIndexPutIdempotent(t *testing.T) {
	idx := newThreadIndex()
	idx.put("c1:t1", "")
	idx.put("c1:t1", "") // should be idempotent
	assert.True(t, idx.has("c1:t1"), "key should still be present after duplicate put")
	// Order list should not have grown to 2 entries for the same key.
	idx.mu.Lock()
	l := idx.order.Len()
	idx.mu.Unlock()
	assert.Equal(t, 1, l, "order list length")
}

// TestEventIDCacheConcurrentAccess exercises the mutex under concurrent usage.
// It is not a correctness proof but will surface data races with -race.
func TestEventIDCacheConcurrentAccess(t *testing.T) {
	c := newEventIDCache(64)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			for j := 0; j < 128; j++ {
				key := "key"
				if j%2 == 0 {
					key = "other"
				}
				c.Add(key)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// onInteraction tests
// ---------------------------------------------------------------------------

// TestOnInteractionIgnored covers the two block-action shapes that
// must NOT publish anything: unknown action_id and non-block-actions
// interaction types. They share the shape (cb in → published? out).
func TestOnInteractionIgnored(t *testing.T) {
	cases := []struct {
		name string
		cb   slackapi.InteractionCallback
	}{
		{
			name: "unknown action_id: no publish",
			cb: slackapi.InteractionCallback{
				Type: slackapi.InteractionTypeBlockActions,
				User: slackapi.User{ID: "U1"},
				ActionCallback: slackapi.ActionCallbacks{
					BlockActions: []*slackapi.BlockAction{
						{ActionID: "some_other_action", Value: "{}"},
					},
				},
			},
		},
		{
			name: "non-block-actions interaction (shortcut): no publish",
			cb: slackapi.InteractionCallback{
				Type: slackapi.InteractionType("shortcut"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			published := false
			l := &slackListener{
				deps: channelkinds.Deps{
					NATSPublish: func(_ string, _ []byte) error {
						published = true
						return nil
					},
				},
			}
			require.NoError(t, l.onInteraction(context.Background(), tc.cb), "onInteraction")
			assert.False(t, published, "NATSPublish must not be called")
		})
	}
}

// TestListener_SessionUpdated_AddsToThreadIndex asserts that the
// SessionWatcher hook (invoked by channelsd's session_attached NATS
// dispatcher) registers the OutputChannel.External thread anchor with
// the in-memory threadIndex, so the next thread reply passes the
// listener's threadIndex gate without waiting for the next channelsd
// restart's startup walk.
func TestListener_SessionUpdated_AddsToThreadIndex(t *testing.T) {
	slackCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           KindName,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	l := &slackListener{
		threads: newThreadIndex(),
		deps: channelkinds.Deps{
			Channel: slackCh,
		},
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-sess", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "bento-in", Kind: "bento",
			},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: KindName,
				Key:      "thread:C_CRON:222.000",
				External: map[string]string{"channel_id": "C_CRON", "thread_ts": "222.000"},
			},
		},
	}

	l.SessionUpdated(context.Background(), sess)
	assert.True(t, l.threads.has("C_CRON:222.000"),
		"SessionUpdated must add OutputChannel thread anchor to threadIndex")

	// Idempotent: a second call must not panic and the index entry
	// must still be present.
	l.SessionUpdated(context.Background(), sess)
	assert.True(t, l.threads.has("C_CRON:222.000"),
		"SessionUpdated must be idempotent; threadIndex entry lost after second call")
}

// TestListener_SessionUpdated_SkipsNonSlackOutput asserts that a
// session whose OutputChannel is bound to a non-slack kind (or
// missing fields) is a no-op for the slack listener — even though
// the dispatcher already filters by OutputChannelName, defense-in-
// depth keeps the listener from indexing foreign anchors if a stray
// event slips through. All cases share the shape: build a session,
// SessionUpdated, assert the listener did NOT index a particular key.
func TestListener_SessionUpdated_SkipsNonSlackOutput(t *testing.T) {
	slackCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           KindName,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	cases := []struct {
		name        string
		output      *spiceboxv1alpha1.ChannelBinding
		mustNotHave string // empty → just assert no-panic
	}{
		{
			name: "OutputChannel.Kind not slack: skipped",
			output: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: "fake",
				External: map[string]string{"channel_id": "X", "thread_ts": "Y"},
			},
			mustNotHave: "X:Y",
		},
		{
			name: "thread_ts missing: skipped",
			output: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: KindName,
				External: map[string]string{"channel_id": "C_ONLY"},
			},
			mustNotHave: "C_ONLY:",
		},
		{
			name:   "nil OutputChannel: no panic, no index",
			output: nil,
		},
		{
			name: "different slack Channel CR name: skipped",
			output: &spiceboxv1alpha1.ChannelBinding{
				Name: "different-slack", Kind: KindName,
				External: map[string]string{"channel_id": "C_OTHER", "thread_ts": "999.000"},
			},
			mustNotHave: "C_OTHER:999.000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &slackListener{
				threads: newThreadIndex(),
				deps:    channelkinds.Deps{Channel: slackCh},
			}
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "weird-sess", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentSessionSpec{
					OutputChannel: tc.output,
				},
			}
			l.SessionUpdated(context.Background(), sess)
			if tc.mustNotHave != "" {
				assert.False(t, l.threads.has(tc.mustNotHave),
					"SessionUpdated must not index %q", tc.mustNotHave)
			}
		})
	}
}

// TestRepopulateThreadIndex_IncludesCronSpawnedThreads asserts that
// repopulateThreadIndex's second walk picks up sessions whose
// OutputChannel binds to THIS slack Channel CR — even when their
// InputChannel kind (and therefore LabelChannelKind) is not slack.
// Without this, a human reply to a cron-spawned thread arriving after
// a channelsd restart would drop at the threadIndex gate.
func TestRepopulateThreadIndex_IncludesCronSpawnedThreads(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	slackCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-out", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           KindName, // "slack"
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
	// Standard human-initiated thread session (input=slack); walk 1
	// indexes this one via labels.
	humanSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "human-sess", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "slack-out",
				spiceboxv1alpha1.LabelChannelKind: KindName,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: KindName,
				External: map[string]string{"channel_id": "C_HUMAN", "thread_ts": "111.000"},
			},
		},
	}
	// Cron-spawned session: InputChannel kind=bento (LabelChannelKind
	// is bento, so walk 1 misses it). The slack thread anchor lives on
	// OutputChannel.External, patched by the outbound relay after first
	// send. Walk 2 must catch this.
	cronSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cron-sess", Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "bento-in",
				spiceboxv1alpha1.LabelChannelKind: "bento",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "bento-in", Kind: "bento",
				Key: "cron:bento-in:42",
			},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: KindName,
				Key: "thread:C_CRON:222.000",
				External: map[string]string{
					"channel_id": "C_CRON",
					"thread_ts":  "222.000",
				},
			},
		},
	}
	// Cron session whose first send hasn't happened yet — no thread_ts.
	// Walk 2 must skip it without panicking.
	preFirstSendSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cron-pre", Namespace: "default",
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "bento-in", Kind: "bento",
			},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: KindName,
				External: map[string]string{"channel_id": "C_CRON"},
			},
		},
	}
	// Cron session whose OutputChannel binds to a DIFFERENT slack Channel
	// CR — must NOT be indexed by this listener.
	otherSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-sess", Namespace: "default",
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "bento-in", Kind: "bento"},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "different-slack", Kind: KindName, // not this listener's CR
				External: map[string]string{"channel_id": "C_OTHER", "thread_ts": "333.000"},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(slackCh, humanSess, cronSess, preFirstSendSess, otherSess).
		Build()

	l := &slackListener{
		threads: newThreadIndex(),
		deps: channelkinds.Deps{
			K8sClient: cli,
			Channel:   slackCh,
		},
	}

	l.repopulateThreadIndex(context.Background())

	assert.True(t, l.threads.has("C_HUMAN:111.000"),
		"walk 1 (input=slack) should have indexed human-sess thread")
	assert.True(t, l.threads.has("C_CRON:222.000"),
		"walk 2 (output=slack-out via cron session) should have indexed cron-sess thread")
	assert.False(t, l.threads.has("C_OTHER:333.000"),
		"other-sess routes through a different slack Channel CR; must not be indexed by this listener")
}

// TestResolveIdentity_CacheReturnsGatedEmail verifies that the email stored
// in the identity LRU cache is the POST-GATE value: a cache hit for a trusted
// user yields a non-empty Email and a cache hit for an untrusted user (whose
// email was withheld at write time) yields an empty Email. This ensures the
// cache cannot serve a different trust decision than a live lookup would.
func TestResolveIdentity_CacheReturnsGatedEmail(t *testing.T) {
	cases := []struct {
		name      string
		cached    userInfo
		wantEmail string
	}{
		{
			name:      "trusted user: cache hit returns non-empty email",
			cached:    userInfo{UserID: "U_TRUSTED", Email: "alice@example.com", TeamID: "T_HOME"},
			wantEmail: "alice@example.com",
		},
		{
			name:      "untrusted user: cache hit returns empty email (gated at write)",
			cached:    userInfo{UserID: "U_GUEST", Email: "", TeamID: "T_HOME"},
			wantEmail: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewIdentityCache(4)
			cache.Put(tc.cached)

			l := &slackListener{
				idents:          cache,
				installedTeamID: "T_HOME",
				// api is nil — resolveIdentity must return on the cache hit
				// path without calling the API.
			}

			got := l.resolveIdentity(context.Background(), tc.cached.UserID)
			assert.Equal(t, identity.Kind(KindName), got.Kind, "Kind must always be slack")
			assert.Equal(t, identity.RawExternalID(tc.cached.UserID), got.ExternalID, "ExternalID must be the user ID")
			assert.Equal(t, identity.Email(tc.wantEmail), got.Email, "Email from cache hit")
			assert.Equal(t, identity.TeamScope(tc.cached.TeamID), got.TeamScope, "TeamScope from cache hit")
		})
	}
}

// TestResolveIdentity_CapturesDisplayName verifies that resolveIdentity
// carries the Slack profile display name (preferredDisplayName precedence:
// display_name → real_name → user ID) onto ExternalIdentity.DisplayName on
// both the live users.info path and the identity-cache hit path, and that a
// live lookup caches the display name so a later cache hit still returns it.
func TestResolveIdentity_CapturesDisplayName(t *testing.T) {
	t.Run("live users.info lookup populates DisplayName and caches it", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U0ALICE","team_id":"T0COMPANY","profile":{"display_name":"Alice A","real_name":"Alice Anderson"}}}`)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		l := &slackListener{
			api:             slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/")),
			idents:          NewIdentityCache(4),
			installedTeamID: "T0COMPANY",
		}

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, "Alice A", id.DisplayName, "DisplayName from users.info lookup")

		cached, ok := l.idents.Get("U0ALICE")
		require.True(t, ok, "resolveIdentity must cache the users.info result")
		assert.Equal(t, "Alice A", cached.DisplayName, "DisplayName cached alongside Email/TeamID")
	})

	t.Run("cache hit returns cached DisplayName without calling the API", func(t *testing.T) {
		cache := NewIdentityCache(4)
		cache.Put(userInfo{UserID: "U0ALICE", TeamID: "T0COMPANY", DisplayName: "Alice A"})

		l := &slackListener{
			idents: cache,
			// api is nil — resolveIdentity must return on the cache hit
			// path without calling the API.
		}

		id := l.resolveIdentity(context.Background(), "U0ALICE")
		assert.Equal(t, "Alice A", id.DisplayName, "DisplayName from cache hit")
	})
}

func TestFormatJoinNotice(t *testing.T) {
	got := formatJoinNotice([]string{"Alice", "Bob", "Carol"}, nil)
	assert.Contains(t, got, "Alice")
	assert.Contains(t, got, "Bob")
	assert.Contains(t, got, "Carol")
	assert.Contains(t, got, "@-mention")

	// No participants resolved — still produces a sane notice.
	got = formatJoinNotice(nil, nil)
	assert.Contains(t, got, "@-mention")
}

// A thread author the agent's interact policy excludes is NAMED in the notice.
// Without this they are indistinguishable from a granted author until they
// @-mention the bot and it silently ignores them — the notice is the only place
// that silence can be explained before it happens.
func TestFormatJoinNotice_namesWithheldParticipants(t *testing.T) {
	got := formatJoinNotice([]string{"Alice"}, []string{"Bob"})
	assert.Contains(t, got, "Alice")
	assert.Contains(t, got, "Bob")
	assert.Contains(t, got, "not on this agent's access list")
	assert.Contains(t, got, "won't respond")

	// Plural reads correctly too — the notice is user-facing copy.
	got = formatJoinNotice(nil, []string{"Bob", "Carol"})
	assert.Contains(t, got, "Bob, Carol are not on this agent's access list")
}

func TestThreadBootstrapPlan(t *testing.T) {
	cases := []struct {
		name        string
		newSession  bool
		adopted     bool
		wantStarter bool
		wantJoin    bool
	}{
		{"reply on an established thread: neither", false, false, false, false},
		{"reply on an established adopted thread: neither", false, true, false, false},
		{"new bot-rooted thread: starter, no join notice", true, false, true, false},
		{"new adopted thread: join notice, NO starter", true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStarter, gotJoin := threadBootstrapPlan(tc.newSession, tc.adopted)
			assert.Equal(t, tc.wantStarter, gotStarter, "postStarter")
			assert.Equal(t, tc.wantJoin, gotJoin, "postJoinNotice")
		})
	}
}

// TestStartingMessage_Format verifies the starter message contains the agent
// name and Slack's <!date^TS^...|fallback> token for live relative timestamps.
func TestStartingMessage_Format(t *testing.T) {
	fixedTime := int64(1700000000)
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel: &spiceboxv1alpha1.Channel{
				Spec: spiceboxv1alpha1.ChannelSpec{AgentClass: "summarizer"},
			},
		},
		now: func() time.Time { return time.Unix(fixedTime, 0) },
	}
	msg, unix := l.startingMessage()
	assert.Contains(t, msg, "🤖", "starter must contain robot emoji")
	assert.Contains(t, msg, "*summarizer*", "starter must contain bold agent name")
	assert.Contains(t, msg, "<!date^1700000000^", "starter must contain Slack date token with fixed unix ts")
	assert.Contains(t, msg, "|just now>", "starter must contain fallback text")
	assert.Equal(t, fixedTime, unix, "startingMessage must return the same unix ts baked into the date token")
}

// TestPostStarter_RecordsStarterCoords verifies that a successful postStarter
// call records the posted message's coordinates into the shared starterCache,
// keyed by "<sessionNS>/<sessionName>", so the thread_title sender can later
// find and edit this exact message.
func TestPostStarter_RecordsStarterCoords(t *testing.T) {
	fc := fakeslack.New()
	l := &slackListener{
		deps: channelkinds.Deps{
			Channel: &spiceboxv1alpha1.Channel{
				Spec: spiceboxv1alpha1.ChannelSpec{AgentClass: "summarizer"},
			},
		},
		api:      fc,
		starters: newStarterCache(),
	}

	l.postStarter(context.Background(), "default", "s1", "C1", "1.0", false)

	got, ok := l.starters.get("default/s1")
	require.True(t, ok, "postStarter must record starter coords on a successful post")
	assert.Equal(t, "C1", got.ChannelID)
	assert.NotEmpty(t, got.MessageTS)
	assert.Equal(t, "summarizer", got.AgentName)
	assert.NotZero(t, got.StartedUnix)
}

func TestClassifyThreadEntry(t *testing.T) {
	cases := []struct {
		name            string
		threadTS, msgTS string
		want            string
	}{
		{"top-level mention starts a thread: root", "", "11.0", channelkinds.ThreadEntryRoot},
		{"mention as its own thread root: root", "11.0", "11.0", channelkinds.ThreadEntryRoot},
		{"mention replying into an older thread: reply", "9.0", "11.0", channelkinds.ThreadEntryReply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyThreadEntry(tc.threadTS, tc.msgTS))
		})
	}
}

// TestOnInteraction_ProviderErrorRetry_PublishesInteractionDecision verifies
// that a click on the Retry button — an Interaction-model decision action —
// flows through the shared handleInteractionDecisionClick path and publishes a
// KindInteractionDecision envelope carrying Category=provider_error_retry /
// ActionID=retry / RequestRef on the inbound subject. The clicker's interact
// standing is re-checked server-side by the decision pipe's DecideParticipant
// policy (fail-closed); the listener runs no authz gate of its own on the
// click, so an unauthorized clicker is rejected silently rather than with an
// ephemeral reply.
func TestOnInteraction_ProviderErrorRetry_PublishesInteractionDecision(t *testing.T) {
	value := encodeInteractionButtonValue(
		"provider-error-retry-ns-sess-0", "retry", "provider_error_retry", "ns/sess")

	pub := &recordingNATSPub{}
	l := &slackListener{
		deps:   channelkinds.Deps{NATSPublish: pub.Publish},
		idents: NewIdentityCache(8),
	}
	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U-clicker"},
		ResponseURL: "https://hooks.slack.com/actions/retry",
		Container: slackapi.Container{
			ChannelID: "C123",
			MessageTs: "1700000000.000100",
		},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "retry", Value: value},
			},
		},
	}

	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")
	require.Len(t, pub.published, 1)
	got := pub.published[0]
	assert.Equal(t, "ap.session.ns.sess.in.interaction_decision", got.subject,
		"retry click must publish interaction_decision, NOT the retired provider_error_retry_requested")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(got.body, &env))
	assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "provider_error_retry", pl.Category, "Category")
	assert.Equal(t, "retry", pl.ActionID, "ActionID")
	assert.Equal(t, "provider-error-retry-ns-sess-0", pl.RequestRef, "RequestRef")
	assert.Equal(t, identity.RawExternalID("U-clicker"), pl.Decider.ExternalID, "Decider.ExternalID")
	assert.Equal(t, "https://hooks.slack.com/actions/retry", pl.ResponseRef,
		"ResponseRef round-trips so the applied edit can strip the button in place")
}

// TestOnInteraction_QueuedInterruptClick_FlowsThroughGenericDecisionPath proves
// the "Interrupt & Send Now" click now flows through the generic decision path:
// a discInteraction-encoded button carrying category queued_messages publishes a
// KindInteractionDecision(queued_messages) on the inbound subject (NOT the old
// direct KindInterruptRequest). channelsd's HandleInteractionDecision then
// re-checks the clicker's interact standing server-side (queued_messages =
// DecideParticipant, fail-closed) before invoking decideQueuedInterrupt — the
// same CheckInteract boundary the deleted handleInterruptClick ran client-side,
// now owned by the generic pipe.
func TestOnInteraction_QueuedInterruptClick_FlowsThroughGenericDecisionPath(t *testing.T) {
	value := encodeInteractionButtonValue("queued-req-1", "interrupt", "queued_messages", "default/sess1")

	pub := &recordingNATSPub{}
	l := &slackListener{
		deps:            channelkinds.Deps{NATSPublish: pub.Publish},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	l.idents.Put(userInfo{UserID: "U_REQ", Email: "requester@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U_REQ"},
		ResponseURL: "https://hooks.slack.com/actions/T1/123/interrupt",
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "interrupt", Value: value},
			},
		},
	}

	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")
	require.Len(t, pub.published, 1)
	got := pub.published[0]
	assert.Equal(t, "ap.session.default.sess1.in.interaction_decision", got.subject,
		"the interrupt click must publish an interaction_decision, not a direct interrupt_request")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(got.body, &env))
	assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "queued_messages", pl.Category, "Category")
	assert.Equal(t, "queued-req-1", pl.RequestRef, "RequestRef (the enqueue-ack correlation id)")
	assert.Equal(t, "interrupt", pl.ActionID, "ActionID")
	assert.Equal(t, identity.RawExternalID("U_REQ"), pl.Decider.ExternalID, "Decider.ExternalID (raw clicker id)")
	assert.Equal(t, identity.Email("requester@example.com"), pl.Decider.Email,
		"Decider.Email enriched so DecideParticipant/canonicalization can key on it")
	assert.Equal(t, "https://hooks.slack.com/actions/T1/123/interrupt", pl.ResponseRef,
		"ResponseRef round-trips so the applied edit targets this clicker's ephemeral")
}

// TestOnInteraction_InteractionDecisionClick_PublishesInteractionDecision
// verifies that clicking one of identity_choice's 3-way decision buttons
// (discInteraction button value) publishes a KindInteractionDecision
// envelope on the inbound subject, carrying Category/RequestRef/ActionID
// from the button plus a Decider enriched with the clicker's verified email
// + team (CRITICAL: DecideRequester in
// pkg/channels/channelsd/pipeline/interaction_decision.go canonicalizes the Decider
// to user:<base64(email)> — without the email the click can never
// canonicalize to the addressee and is silently rejected). Also verifies
// ResponseRef round-trips from cb.ResponseURL so the applied-outcome edit
// can target the same ephemeral.
func TestOnInteraction_InteractionDecisionClick_PublishesInteractionDecision(t *testing.T) {
	value := encodeInteractionButtonValue("idc-req-1", "agent", "identity_choice", "default/sess1")

	pub := &recordingNATSPub{}
	l := &slackListener{
		deps:            channelkinds.Deps{NATSPublish: pub.Publish},
		idents:          NewIdentityCache(8),
		installedTeamID: "T1",
	}
	l.idents.Put(userInfo{UserID: "U_REQ", Email: "requester@example.com", TeamID: "T1"})

	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U_REQ"},
		ResponseURL: "https://hooks.slack.com/actions/T1/123/identity-choice",
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "agent", Value: value},
			},
		},
	}

	require.NoError(t, l.onInteraction(context.Background(), cb), "onInteraction")
	require.Len(t, pub.published, 1)
	got := pub.published[0]
	assert.Equal(t, "ap.session.default.sess1.in.interaction_decision", got.subject)

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(got.body, &env))
	assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "identity_choice", pl.Category, "Category")
	assert.Equal(t, "idc-req-1", pl.RequestRef, "RequestRef")
	assert.Equal(t, "agent", pl.ActionID, "ActionID")
	assert.Equal(t, identity.Kind("slack"), pl.Decider.Kind, "Decider.Kind")
	assert.Equal(t, identity.RawExternalID("U_REQ"), pl.Decider.ExternalID, "Decider.ExternalID")
	assert.Equal(t, identity.Email("requester@example.com"), pl.Decider.Email,
		"Decider.Email must be enriched so DecideRequester can canonicalize the click")
	assert.Equal(t, identity.TeamScope("T1"), pl.Decider.TeamScope, "Decider.TeamScope")
	assert.Equal(t, "https://hooks.slack.com/actions/T1/123/identity-choice", pl.ResponseRef,
		"ResponseRef must round-trip from cb.ResponseURL so the applied edit can target this ephemeral")
}

// TestOnInteraction_InteractionDecisionClick_PublishFailure_SurfacesNotice
// verifies a NATS publish failure surfaces a failure notice to the clicker
// via response_url (mirrors the tool_approval / interrupt click failure
// handling) rather than leaving the clicker's buttons looking live with no
// feedback.
func TestOnInteraction_InteractionDecisionClick_PublishFailure_SurfacesNotice(t *testing.T) {
	value := encodeInteractionButtonValue("idc-req-2", "cancel", "identity_choice", "default/sess1")

	var surfaced []string
	l := &slackListener{
		deps: channelkinds.Deps{
			NATSPublish: func(string, []byte) error { return fmt.Errorf("nats: no responders") },
		},
		idents: NewIdentityCache(8),
		responseURLPoster: func(_ context.Context, _ string, body any) error {
			if m, ok := body.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					surfaced = append(surfaced, s)
				}
			}
			return nil
		},
	}
	cb := slackapi.InteractionCallback{
		Type:        slackapi.InteractionTypeBlockActions,
		User:        slackapi.User{ID: "U_REQ"},
		ResponseURL: "https://hooks.slack.com/actions/T1/456/identity-choice",
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "cancel", Value: value},
			},
		},
	}

	err := l.onInteraction(context.Background(), cb)
	require.Error(t, err, "publish failure must be returned")
	require.Len(t, surfaced, 1, "publish failure must surface a notice to the clicker")
}

// TestHandleInteractionDecisionClick_IgnoresNonInteractionValue verifies
// that a non-discInteraction button value (e.g. a slice-2 tool_approval
// click) is not consumed by handleInteractionDecisionClick — it must return
// (false, nil) so the caller falls through to the handler that actually owns
// that discriminator.
func TestHandleInteractionDecisionClick_IgnoresNonInteractionValue(t *testing.T) {
	value := `{"v":"slice2_approval","r":"req-abc","d":"approve","s":"default/sess1"}`
	l := &slackListener{}
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_APPROVER"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "tool_approval_approve", Value: value},
			},
		},
	}
	handled, err := l.handleInteractionDecisionClick(context.Background(), cb)
	require.NoError(t, err)
	assert.False(t, handled, "handleInteractionDecisionClick must not claim a non-discInteraction click")
}

// TestHandleInteractionDecisionClick_MalformedSessionRef_Errors verifies a
// malformed session ref in the button value errors (true, err) rather than
// silently no-op'ing.
func TestHandleInteractionDecisionClick_MalformedSessionRef_Errors(t *testing.T) {
	value := encodeInteractionButtonValue("idc-req-3", "agent", "identity_choice", "not-a-valid-ref")
	l := &slackListener{idents: NewIdentityCache(8)}
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: "U_REQ"},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: "agent", Value: value},
			},
		},
	}
	handled, err := l.handleInteractionDecisionClick(context.Background(), cb)
	require.Error(t, err)
	assert.True(t, handled, "a recognized-but-malformed click must not fall through to another handler")
}

// recordingNATSPub satisfies the NATSPublish func in channelkinds.Deps.
type recordingNATSPub struct {
	published []struct {
		subject string
		body    []byte
	}
}

func (r *recordingNATSPub) Publish(subject string, body []byte) error {
	r.published = append(r.published, struct {
		subject string
		body    []byte
	}{subject, body})
	return nil
}

// recordingViewsOpenAPI is a *slackapi.Client backed by an httptest server
// that acks every call and records the ModalViewRequest sent to
// views.open. slackListener.api is the concrete *slackapi.Client (not the
// slackClient interface fakeSlackClient satisfies for the sender), so
// handler tests that need to inspect an opened modal's blocks drive the
// real client against a fake HTTP endpoint rather than swapping in a mock
// — same approach as recordingSlackAPI in
// listener_terminal_continuation_test.go.
func recordingViewsOpenAPI(t *testing.T) (*slackapi.Client, func() []slackapi.ModalViewRequest) {
	t.Helper()
	var mu sync.Mutex
	var views []slackapi.ModalViewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TriggerID string                    `json:"trigger_id"`
			View      slackapi.ModalViewRequest `json:"view"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		views = append(views, body.View)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"view":{"id":"V1"}}`))
	}))
	t.Cleanup(srv.Close)
	api := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	return api, func() []slackapi.ModalViewRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]slackapi.ModalViewRequest(nil), views...)
	}
}

// TestHandleDM_StampsLastInboundToMessageTS: on a routed DM turn, handleDM must
// stamp LastInboundTS = the message ts so the outbound reply threads under the
// user's message. Guards the inbound half of that threading contract.
func TestHandleDM_StampsLastInboundToMessageTS(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "ns"},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()

	l := &slackListener{
		deps: channelkinds.Deps{
			K8sClient: cli,
			Inbound: fixedInbound{dec: channelkinds.InboundDecision{
				Outcome: channelkinds.OutcomeRouted,
				Session: channelkinds.SessionInfo{Namespace: "ns", Name: "sess"},
			}},
		},
	}

	l.handleDM(context.Background(), "U1", "D01ABCDEF", "1700000000.000400", "hi", nil)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess"}, &got))
	assert.Equal(t, "1700000000.000400", got.Annotations[LastInboundTSAnnotationKey],
		"handleDM must stamp LastInboundTS = the DM message ts")
}

// The "a threadIndex miss is not proof a thread is unowned" guard lives in
// TestThreadOwnership_SiblingChannelStillClaims (thread_binding_test.go). It
// moved there when the fallback gained the claim rule: recovering a thread now
// requires a fixture that says WHICH Channel the thread is bound to and which
// Channel the listener serves, and the two tests would otherwise be identical.

// A genuinely unrelated thread must still be dropped — the fallback must not
// turn every stray message in the channel into a routed one.
func TestThreadOwnership_UnrelatedThreadStillDropped(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	cli := fake.NewClientBuilder().WithScheme(scheme).Build() // no sessions

	l := &slackListener{threads: newThreadIndex()}
	l.deps.K8sClient = cli
	l.deps.Channel = &spiceboxv1alpha1.Channel{}
	l.deps.Channel.Namespace = "default"
	l.deps.Channel.Name = "slack-interactive"

	owned, _ := l.threadIsOwned(context.Background(), "C1", "9999.0000")
	assert.False(t, owned, "a thread with no matching session must stay unowned")
}

// The in-memory index remains the fast path: a hit must not incur a lookup,
// and must preserve the routing mode it recorded.
func TestThreadOwnership_IndexHitWins(t *testing.T) {
	l := &slackListener{threads: newThreadIndex()}
	l.deps.Channel = &spiceboxv1alpha1.Channel{}
	// No K8sClient wired: if the index hit did not short-circuit, this would
	// nil-panic or fail — proving the fast path is taken.
	l.threads.put("C1:1.0", "mention_only")

	owned, mode := l.threadIsOwned(context.Background(), "C1", "1.0")
	assert.True(t, owned)
	assert.Equal(t, "mention_only", mode, "the indexed routing mode must survive")
}
