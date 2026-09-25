package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// recordingPacer stands in for directoryPacer and records, in call order,
// the Slack method each wait was asked to pace and the token fingerprint it
// was keyed by. It never delays — the wiring is what these tests prove, not
// the timing (that is tierLimiter's own tests, below, and rate.Limiter's).
type recordingPacer struct {
	mu           sync.Mutex
	methods      []string
	fingerprints []string
}

func (r *recordingPacer) wait(_ context.Context, fingerprint string, m slackMethod) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, m.name)
	r.fingerprints = append(r.fingerprints, fingerprint)
	return nil
}

func (r *recordingPacer) recorded() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...), append([]string(nil), r.fingerprints...)
}

// noopPacer is the pacer installed by startDirectoryTestServer for every
// directory test: pacing under Slack's real per-minute tiers would add
// seconds of real sleeping to the suite, and the only test that cares which
// method was paced installs its own recordingPacer instead.
type noopPacer struct{}

func (noopPacer) wait(context.Context, string, slackMethod) error { return nil }

// installPacer swaps directoryPacer for p for the life of t.
func installPacer(t *testing.T, p directoryRateLimiter) {
	t.Helper()
	orig := directoryPacer
	directoryPacer = p
	t.Cleanup(func() { directoryPacer = orig })
}

// Every Slack call FetchScope makes must be paced first, at its own method's
// tier, in call order, and keyed by the credential's fingerprint — a call
// site that skips the pacer is exactly how the proactive limit springs a
// leak and the 429s come back, the same failure shape withRetryAfter's
// "must be called at every call site" comment guards against.
func TestDirectoryPacer_FetchScopePacesEverySlackCallInOrder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", jsonHandler(map[string]any{
		"ok": true, "channel": map[string]any{"id": "C1", "is_private": false},
	}))
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1", "U2"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "user": map[string]any{"id": r.FormValue("user"), "is_bot": false, "profile": map[string]any{"email": r.FormValue("user") + "@example.com"}},
		})
	})
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	rec := &recordingPacer{}
	installPacer(t, rec)

	k := &SyncKind{}
	_, err := k.FetchScope(context.Background(), testCreds(), relsync.Scope{ID: "C1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	methods, fingerprints := rec.recorded()
	assert.Equal(t, []string{
		"conversations.info",
		"conversations.members",
		"auth.test",
		"users.info", // U1
		"users.info", // U2
	}, methods, "every Slack call must be paced, at its own method, in call order")

	wantFP := tokenFingerprint(testCreds().Token.UnderlyingValue())
	for _, fp := range fingerprints {
		assert.Equal(t, wantFP, fp, "each pace must be keyed by the credential's fingerprint, never a shared bucket")
	}
}

// A member already resolved earlier in the pass is served from
// userInfoCache and makes NO users.info call — so it must not consume a
// users.info pace either. The pacer tracks real upstream calls, not loop
// iterations.
func TestDirectoryPacer_CachedMemberIsNotPaced(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": map[string]any{"id": r.FormValue("channel"), "is_private": false},
		})
	})
	mux.HandleFunc("/conversations.members", jsonHandler(map[string]any{
		"ok": true, "members": []string{"U1"}, "response_metadata": map[string]any{"next_cursor": ""},
	}))
	mux.HandleFunc("/users.info", jsonHandler(map[string]any{
		"ok": true, "user": map[string]any{"id": "U1", "is_bot": false, "profile": map[string]any{"email": "u1@example.com"}},
	}))
	mux.HandleFunc("/auth.test", jsonHandler(map[string]any{"ok": true, "team_id": "T1"}))
	startDirectoryTestServer(t, mux)

	rec := &recordingPacer{}
	installPacer(t, rec)

	k := &SyncKind{}
	ctx := context.Background()
	creds := testCreds()
	_, err := k.FetchScope(ctx, creds, relsync.Scope{ID: "CHAN1", ResourceType: slackChannelResourceType})
	require.NoError(t, err)
	_, err = k.FetchScope(ctx, creds, relsync.Scope{ID: "CHAN2", ResourceType: slackChannelResourceType})
	require.NoError(t, err)

	methods, _ := rec.recorded()
	usersInfoPaces := 0
	for _, m := range methods {
		if m == "users.info" {
			usersInfoPaces++
		}
	}
	assert.Equal(t, 1, usersInfoPaces, "the second channel's cached member must not consume a users.info pace")
}

// ListScopes must pace its one conversations.list call.
func TestDirectoryPacer_ListScopesPacesConversationsList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.list", jsonHandler(map[string]any{
		"ok": true, "channels": []map[string]any{{"id": "C1", "is_private": false}},
		"response_metadata": map[string]any{"next_cursor": ""},
	}))
	startDirectoryTestServer(t, mux)

	rec := &recordingPacer{}
	installPacer(t, rec)

	k := &SyncKind{}
	_, err := k.ListScopes(context.Background(), testCreds(), relsync.Cursor{})
	require.NoError(t, err)

	methods, _ := rec.recorded()
	assert.Equal(t, []string{"conversations.list"}, methods)
}

// slowMethod is a synthetic method whose token refills once an hour: after
// the burst is spent, the next call cannot proceed within any test-sized
// deadline, which is how these tests prove gating deterministically without
// sleeping.
var slowMethod = slackMethod{name: "test.slow", limit: rate.Every(time.Hour)}

// Once a (token, method) bucket's burst is spent, the next call for the SAME
// token and method is gated — it blocks, so a short context deadline expires
// rather than the call sailing through. This is the property that actually
// keeps us under Slack's ceiling.
func TestTierLimiter_SameMethodSameTokenIsGatedOnceBurstSpent(t *testing.T) {
	lim := newTierLimiter()
	for i := 0; i < directoryRateLimitBurst; i++ {
		require.NoError(t, lim.wait(context.Background(), "fpA", slowMethod), "the burst allowance must admit the first calls immediately")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := lim.wait(ctx, "fpA", slowMethod)
	require.Error(t, err, "with the burst spent and a one-hour refill, the next call must block past a short deadline, not proceed")
}

// A different workspace token has an independent budget: fpA being exhausted
// must never gate fpB. Two RelationshipSources share this package's one
// registered *SyncKind, and Slack limits per token — a shared bucket would
// throttle one workspace on another's traffic.
func TestTierLimiter_DifferentTokenHasIndependentBudget(t *testing.T) {
	lim := newTierLimiter()
	for i := 0; i < directoryRateLimitBurst; i++ {
		require.NoError(t, lim.wait(context.Background(), "fpA", slowMethod))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.NoError(t, lim.wait(ctx, "fpB", slowMethod), "a different token must not share fpA's exhausted bucket")
}

// A different method for the same token has an independent budget too: Slack
// throttles per method, so conversations.info being spent must not gate
// users.info.
func TestTierLimiter_DifferentMethodHasIndependentBudget(t *testing.T) {
	lim := newTierLimiter()
	for i := 0; i < directoryRateLimitBurst; i++ {
		require.NoError(t, lim.wait(context.Background(), "fpA", slowMethod))
	}
	otherSlow := slackMethod{name: "test.slow.other", limit: rate.Every(time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.NoError(t, lim.wait(ctx, "fpA", otherSlow), "a different method must have its own bucket")
}
