package inproc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// resolveCount returns the current count of underlying Secret reads the
// broker has made (i.e. cache misses + initial fills). We measure this by
// checking what tokens are served: a fresh fake-client has the Secret; we
// swap the fake client after the first hit so subsequent misses would
// return a different token. Instead we count by intercepting via a pair of
// servers that count requests.
//
// Since the broker reads Kubernetes Secrets via the injected client.Client
// rather than a pluggable resolver, we count cache hits differently: we
// verify that the token value returned was the one cached (meaning no new
// round-trip was made). See individual test cases for the concrete approach.

// oauthSecretAt builds a Secret with an unexpired oauth token.
func oauthSecretAt(ns, name, token string, exp time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data: map[string][]byte{
			"access_token": []byte(token),
			"expires_at":   []byte(exp.UTC().Format(time.RFC3339)),
		},
	}
}

// makeSubjectSecretName returns the Secret name for (subject, credName) as the
// broker's Invalidate method computes it. This is the same formula used by
// useridentity.MasterSecretName + useridentity.NameForSubject.
func makeSubjectSecretName(subject identity.Subject, credName string) string {
	return useridentity.MasterSecretName(useridentity.NameForSubject(subject), credName)
}

// oauthDesc builds a CredentialDescriptor for an oauth credential stored in
// the given namespace under the given secretName.
func oauthDesc(ns, secretName, headerName string) spiceboxv1alpha1.CredentialDescriptor {
	return spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{
			Type: "oauth", Namespace: ns, Name: secretName,
		},
		Inject: spiceboxv1alpha1.CredentialInjection{
			Header: &spiceboxv1alpha1.HeaderInjection{Name: headerName, ValuePrefix: "Bearer "},
		},
	}
}

// TestBroker_CacheHit_SkipsUnderlyingResolver verifies that a second Resolve
// for the same credential returns the cached token without re-reading the
// Secret. We mutate the Secret after the first Resolve; if the broker re-reads
// it the token would change, proving a cache miss. A stable token proves a hit.
func TestBroker_CacheHit_SkipsUnderlyingResolver(t *testing.T) {
	const (
		subject  = "user:alice"
		credName = "linear-oauth"
	)
	secretName := makeSubjectSecretName(subject, credName)
	ns := spiceboxv1alpha1.IdentitiesNamespace
	exp := time.Now().Add(time.Hour)

	sec := oauthSecretAt(ns, secretName, "token-v1", exp)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)
	desc := oauthDesc(ns, secretName, "Authorization")

	// First resolve — should hit the underlying client.
	res1, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v1", res1.HTTPHeaders["Authorization"])

	// Mutate the Secret so a fresh read would return a different token.
	sec.Data["access_token"] = []byte("token-v2")
	require.NoError(t, c.Update(context.Background(), sec))

	// Second resolve — should return the cached token-v1, NOT the updated token-v2.
	res2, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v1", res2.HTTPHeaders["Authorization"],
		"second Resolve should serve cache without re-reading Secret")
}

// TestBroker_DifferentKeysAreIndependent verifies that two credentials with
// different cache keys are resolved independently.
func TestBroker_DifferentKeysAreIndependent(t *testing.T) {
	ns := spiceboxv1alpha1.IdentitiesNamespace
	exp := time.Now().Add(time.Hour)

	secA := oauthSecretAt(ns, "secret-a", "token-a", exp)
	secB := oauthSecretAt(ns, "secret-b", "token-b", exp)
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(secA, secB).
		Build()
	b := New(c)

	descA := oauthDesc(ns, "secret-a", "X-Token-A")
	descB := oauthDesc(ns, "secret-b", "X-Token-B")

	resA, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{descA},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-a", resA.HTTPHeaders["X-Token-A"])

	resB, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{descB},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-b", resB.HTTPHeaders["X-Token-B"])

	// Mutate both Secrets.
	secA.Data["access_token"] = []byte("token-a2")
	secB.Data["access_token"] = []byte("token-b2")
	require.NoError(t, c.Update(context.Background(), secA))
	require.NoError(t, c.Update(context.Background(), secB))

	// Both should still be served from cache.
	resA2, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{descA},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-a", resA2.HTTPHeaders["X-Token-A"],
		"credential A should still be cached")

	resB2, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{descB},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-b", resB2.HTTPHeaders["X-Token-B"],
		"credential B should still be cached")
}

// TestBroker_InvalidateDropsEntry verifies that InvalidateSecret causes the
// next Resolve to re-read the underlying Secret.
func TestBroker_InvalidateDropsEntry(t *testing.T) {
	const (
		subject  = "user:bob"
		credName = "github-pat"
	)
	secretName := makeSubjectSecretName(subject, credName)
	ns := spiceboxv1alpha1.IdentitiesNamespace
	exp := time.Now().Add(time.Hour)

	sec := oauthSecretAt(ns, secretName, "token-v1", exp)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)
	desc := oauthDesc(ns, secretName, "Authorization")

	// Prime the cache.
	res1, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v1", res1.HTTPHeaders["Authorization"])

	// Update the Secret with a new token.
	sec.Data["access_token"] = []byte("token-v2")
	require.NoError(t, c.Update(context.Background(), sec))

	// Invalidate the cache entry via the Secret coordinates.
	require.NoError(t, b.InvalidateSecret(ns, secretName))

	// Next Resolve must re-read the Secret and return the updated token.
	res2, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-v2", res2.HTTPHeaders["Authorization"],
		"after InvalidateSecret, Resolve must return freshly resolved token")
}

// TestBroker_InvalidateUnknownKey_NoError verifies that InvalidateSecret on a
// key with no cache entry is a safe no-op (no panic, no error).
func TestBroker_InvalidateUnknownKey_NoError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	b := New(c)
	err := b.InvalidateSecret(spiceboxv1alpha1.IdentitiesNamespace, "no-such-secret")
	assert.NoError(t, err, "InvalidateSecret on unknown key must be a no-op")
}

// TestBroker_InvalidateOnlyAffectsMatchingKey verifies that InvalidateSecret
// for one credential does not evict other cached entries.
func TestBroker_InvalidateOnlyAffectsMatchingKey(t *testing.T) {
	const (
		subjectA = "user:alice"
		subjectB = "user:bob"
		credName = "linear-oauth"
	)
	secretNameA := makeSubjectSecretName(subjectA, credName)
	secretNameB := makeSubjectSecretName(subjectB, credName)
	ns := spiceboxv1alpha1.IdentitiesNamespace
	exp := time.Now().Add(time.Hour)

	secA := oauthSecretAt(ns, secretNameA, "token-alice", exp)
	secB := oauthSecretAt(ns, secretNameB, "token-bob", exp)
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(secA, secB).
		Build()
	b := New(c)

	descA := oauthDesc(ns, secretNameA, "X-Alice")
	descB := oauthDesc(ns, secretNameB, "X-Bob")

	// Prime both cache entries.
	_, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{descA}})
	require.NoError(t, err)
	_, err = b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{descB}})
	require.NoError(t, err)

	// Mutate both Secrets.
	secA.Data["access_token"] = []byte("token-alice2")
	secB.Data["access_token"] = []byte("token-bob2")
	require.NoError(t, c.Update(context.Background(), secA))
	require.NoError(t, c.Update(context.Background(), secB))

	// Invalidate only Alice's entry via Secret coordinates.
	require.NoError(t, b.InvalidateSecret(ns, secretNameA))

	// Alice: cache miss → new token.
	resA, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{descA}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-alice2", resA.HTTPHeaders["X-Alice"],
		"Alice's entry should have been invalidated")

	// Bob: still cached → old token.
	resB, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{descB}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer token-bob", resB.HTTPHeaders["X-Bob"],
		"Bob's entry should remain cached")
}

// TestBroker_CacheHit_StaticCredential verifies that static credential
// resolutions are also cached (not just oauth).
func TestBroker_CacheHit_StaticCredential(t *testing.T) {
	ns := "default"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-token"},
		Data:       map[string][]byte{"token": []byte("static-v1")},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)

	desc := staticDesc(ns, "my-token", "token", "MY_TOKEN")

	res1, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err)
	assert.Equal(t, "static-v1", res1.EnvVars["MY_TOKEN"])

	// Mutate the Secret.
	sec.Data["token"] = []byte("static-v2")
	require.NoError(t, c.Update(context.Background(), sec))

	// Should still serve from cache.
	res2, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err)
	assert.Equal(t, "static-v1", res2.EnvVars["MY_TOKEN"],
		"static credential should be served from cache on second Resolve")
}

// TestBroker_SameSecretDifferentKeys_DoNotCollide is a regression test for a
// broker cache-collision bug. The cache keyed resolved credentials by
// {namespace, name, resource} but NOT by the Secret data KEY. Two credentials
// stored in ONE Secret under different keys — e.g. the per-session passthrough
// Secret holding both anthropic-oauth and github-token — therefore collided:
// whichever resolved first won, and the other was served the wrong token.
//
// In production the agent cloned its repo first (resolving github-token, which
// primed the shared cache key), then ran claude (anthropic-oauth) — which was
// handed the cached GITHUB token in CLAUDE_CODE_OAUTH_TOKEN, so claude failed
// with "401 Invalid bearer token". The fix keys the cache by Secret data key too.
func TestBroker_SameSecretDifferentKeys_DoNotCollide(t *testing.T) {
	ns := "default"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "session-passthrough-creds"},
		Data: map[string][]byte{
			"anthropic-oauth": []byte("sk-ant-oat01-claude"),
			"github-token":    []byte("ghp_github"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)

	gitDesc := staticDesc(ns, "session-passthrough-creds", "github-token", "GIT_TOKEN")
	claudeDesc := staticDesc(ns, "session-passthrough-creds", "anthropic-oauth", "CLAUDE_CODE_OAUTH_TOKEN")

	// Resolve github-token FIRST (the agent clones its repo before running
	// claude), priming the cache under the shared Secret coordinates.
	resGit, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{gitDesc},
	})
	require.NoError(t, err)
	require.Equal(t, "ghp_github", resGit.EnvVars["GIT_TOKEN"])

	// Resolve anthropic-oauth from the SAME Secret under a DIFFERENT key. It
	// must return claude's token, NOT github's cached one.
	resClaude, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{claudeDesc},
	})
	require.NoError(t, err)
	assert.Equal(t, "sk-ant-oat01-claude", resClaude.EnvVars["CLAUDE_CODE_OAUTH_TOKEN"],
		"anthropic-oauth and github-token share one Secret but differ by key; the cache must not serve the github token for the claude credential")
	assert.NotContains(t, resClaude.EnvVars, "GIT_TOKEN",
		"resolving anthropic-oauth must not leak github's env var from a colliding cache entry")
}

// resolveOnce is the one-descriptor Resolve every cache test performs.
func resolveOnce(t *testing.T, b *Broker, desc spiceboxv1alpha1.CredentialDescriptor) broker.Resolution {
	t.Helper()
	res, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err, "Resolve")
	return res
}

// staticSecret builds a Secret holding one static credential value.
func staticSecret(ns, name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// setSecretValue rewrites one key of an existing Secret through c, so a cache
// miss would observe a different value than the cache holds.
func setSecretValue(t *testing.T, c client.Client, ns, name, key, value string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sec),
		"get %s/%s before mutation", ns, name)
	sec.Data[key] = []byte(value)
	require.NoError(t, c.Update(context.Background(), &sec), "update %s/%s", ns, name)
}

// TestBroker_CacheEntryExpires_AfterTTL pins that a cached credential has a
// BOUNDED lifetime. resolveOneSource used to stamp a zero expiresAt on every
// non-federated resolution and the cache read it as "never expires", so a
// static or oauth token was served for the life of the process and the only
// eviction was an explicit InvalidateSecret. That is a revocation bypass
// wherever no invalidation arrives — most sharply when the operator's own
// pre-expiry refresh rewrites the SAME Secret name in place, which changes no
// revocation fingerprint and therefore emits no revoke at all.
func TestBroker_CacheEntryExpires_AfterTTL(t *testing.T) {
	const (
		ns     = "agent-ns"
		name   = "ttl-secret"
		key    = "token"
		envVar = "MY_TOKEN"
	)
	sec := staticSecret(ns, name, key, "token-v1")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	fakeClock := clocktesting.NewFakeClock(time.Now())
	b := New(c)
	b.clk = fakeClock

	desc := staticDesc(ns, name, key, envVar)
	require.Equal(t, "token-v1", resolveOnce(t, b, desc).EnvVars[envVar], "first Resolve fills the cache")

	setSecretValue(t, c, ns, name, key, "token-v2")

	// Within the cache's lifetime the old value is still served — that is the
	// cache doing its job, and the behaviour the runner's per-tool-call
	// resolves depend on.
	assert.Equal(t, "token-v1", resolveOnce(t, b, desc).EnvVars[envVar],
		"immediately after the fill, Resolve must still serve the cached value")

	// An hour later it must not be. Any bounded TTL the broker picks is far
	// below this; an unbounded entry is the defect.
	fakeClock.Step(time.Hour)
	assert.Equal(t, "token-v2", resolveOnce(t, b, desc).EnvVars[envVar],
		"an hour after the fill, the entry must have expired and Resolve must re-read the Secret")
}

// TestBroker_ZeroTTLEntry_NotServed pins the fail-closed reading of an absent
// TTL, independent of how the TTL is stamped: an entry whose lifetime the
// broker cannot bound is UNUSABLE, not eternal. Without this, any future
// resolve path that forgets to stamp an expiry silently reintroduces the
// never-expiring cache.
func TestBroker_ZeroTTLEntry_NotServed(t *testing.T) {
	const (
		ns     = "agent-ns"
		name   = "zero-ttl-secret"
		key    = "token"
		envVar = "MY_TOKEN"
	)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(staticSecret(ns, name, key, "real-token")).Build()
	b := New(c)

	// Plant an entry with no expiry, as resolveOneSource used to produce.
	b.cache = map[cacheKey]cacheEntry{
		{Namespace: ns, Name: name, Key: key}: {
			cred: authkind.ResolvedCredential{AccessToken: sensitive.NewSensitiveValue([]byte("planted-token"))},
			// expiresAt deliberately left as the zero value.
		},
	}

	assert.Equal(t, "real-token", resolveOnce(t, b, staticDesc(ns, name, key, envVar)).EnvVars[envVar],
		"an entry with no expiry must be treated as unusable and re-resolved, not served forever")
}

// gatedClient delegates every call to the wrapped client, but blocks the FIRST
// Get of target *after* performing it — so a resolve in flight is holding the
// pre-block bytes while the test drives an invalidation on another goroutine.
type gatedClient struct {
	client.Client
	target  client.ObjectKey
	once    sync.Once
	entered chan struct{} // closed once the gated Get has read the Secret
	release chan struct{} // closed by the test to let that Get return
}

func newGatedClient(inner client.Client, target client.ObjectKey) *gatedClient {
	return &gatedClient{
		Client:  inner,
		target:  target,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *gatedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := g.Client.Get(ctx, key, obj, opts...)
	if key == g.target {
		g.once.Do(func() {
			close(g.entered)
			<-g.release
		})
	}
	return err
}

// TestBroker_InvalidateDuringInFlightResolve_NotResurrected pins the
// invalidation race. resolveOneCached drops cacheMu around the underlying
// resolve and then wrote the result back UNCONDITIONALLY, while
// InvalidateSecret only deletes what is already in the map. An invalidation
// landing in that window therefore deleted nothing and was immediately
// overwritten by the in-flight (pre-revocation) value — which, carrying a zero
// expiry, was then served for the life of the process.
//
// The interleaving is forced, not hoped for:
//
//	A: Resolve → cache miss → Get returns token-v1 → BLOCKED in the gate
//	M: Secret updated to token-v2; InvalidateSecret (deletes nothing); gate released
//	A: write-back of token-v1 — after the invalidation
//	M: next Resolve → serves the revoked token-v1
//
// Run under -race: A and M touch the broker's cache from two goroutines, which
// is exactly the production shape (the revoke arrives on the NATS dispatch
// goroutine while a controller worker is mid-resolve).
func TestBroker_InvalidateDuringInFlightResolve_NotResurrected(t *testing.T) {
	const (
		ns     = "agent-ns"
		name   = "raced-secret"
		key    = "token"
		envVar = "MY_TOKEN"
	)
	inner := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(staticSecret(ns, name, key, "token-v1")).Build()
	gated := newGatedClient(inner, client.ObjectKey{Namespace: ns, Name: name})
	b := New(gated)
	desc := staticDesc(ns, name, key, envVar)

	var (
		racedValue string
		racedErr   error
		done       = make(chan struct{})
	)
	go func() {
		defer close(done)
		res, err := b.Resolve(context.Background(), broker.Request{
			Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
		})
		racedValue, racedErr = res.EnvVars[envVar], err
	}()

	<-gated.entered // the resolve now holds token-v1 and has not cached it yet

	// The credential is replaced and revoked while that resolve is in flight.
	setSecretValue(t, inner, ns, name, key, "token-v2")
	require.NoError(t, b.InvalidateSecret(ns, name), "InvalidateSecret during the in-flight resolve")

	close(gated.release)
	<-done

	require.NoError(t, racedErr, "the raced Resolve must still resolve")
	assert.Equal(t, "token-v2", racedValue,
		"a resolve whose invalidation landed mid-flight must re-resolve, not return the pre-invalidation value")

	// The decisive assertion: the invalidation must not have been overwritten.
	assert.Equal(t, "token-v2", resolveOnce(t, b, desc).EnvVars[envVar],
		"the invalidated credential must not be resurrected into the cache by the in-flight resolve")
}

// invalidatingClient invalidates target on the broker after every Get of it,
// so every resolve attempt is superseded before it can be written back — the
// pathological case the attempt bound exists for.
type invalidatingClient struct {
	client.Client
	target     client.ObjectKey
	invalidate func()
}

func (i *invalidatingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := i.Client.Get(ctx, key, obj, opts...)
	if key == i.target {
		i.invalidate()
	}
	return err
}

// TestBroker_InvalidatedEveryAttempt_FailsClosed pins the exhaustion arm of
// the re-resolve loop. When an invalidation supersedes every attempt, the
// broker must surface an error rather than fall back on the value in hand:
// that value predates the most recent revoke, and returning it would be the
// resurrection the retry exists to prevent, merely delayed.
func TestBroker_InvalidatedEveryAttempt_FailsClosed(t *testing.T) {
	const (
		ns     = "agent-ns"
		name   = "always-revoked"
		key    = "token"
		envVar = "MY_TOKEN"
	)
	inner := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(staticSecret(ns, name, key, "token-v1")).Build()

	var b *Broker
	c := &invalidatingClient{
		Client: inner,
		target: client.ObjectKey{Namespace: ns, Name: name},
		invalidate: func() {
			require.NoError(t, b.InvalidateSecret(ns, name), "InvalidateSecret from within the resolve")
		},
	}
	b = New(c)

	_, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{staticDesc(ns, name, key, envVar)},
	})
	require.Error(t, err, "a credential invalidated on every attempt must not resolve")
	assert.ErrorContains(t, err, "refusing to return a credential a revocation has superseded",
		"the error must name the fail-closed reason, not a generic resolve failure")

	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	assert.Empty(t, b.cache, "no entry may be cached when every attempt was invalidated")
	assert.Empty(t, b.inflight, "every in-flight registration must be removed by the resolve that made it")
}

// TestBroker_ExpiredOAuth_CacheSkipped verifies that expired oauth credentials
// that trigger a JIT refresh get their post-refresh token cached (not the
// expired one), and a subsequent Resolve returns the refreshed token from cache.
func TestBroker_ExpiredOAuth_CacheSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-token", "refresh_token": "rt-2",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	ns := "default"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: ns},
		Data: map[string][]byte{
			"access_token":   []byte("expired-token"),
			"refresh_token":  []byte("rt-1"),
			"expires_at":     []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
			"token_endpoint": []byte(srv.URL),
			"client_id":      []byte("cid"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)
	desc := oauthDesc(ns, "o", "Authorization")

	// First resolve: expired → JIT refresh → returns refreshed token.
	res1, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err, "JIT refresh must succeed")
	assert.Equal(t, "Bearer refreshed-token", res1.HTTPHeaders["Authorization"])

	// Re-fetch the Secret (the JIT refresh updated it, bumping the resource
	// version; our local copy is stale and would conflict on Update).
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "o"}, sec))

	// Now mutate the Secret in the fake client again, so a cache miss would
	// return something different.
	sec.Data["access_token"] = []byte("another-token")
	sec.Data["expires_at"] = []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, c.Update(context.Background(), sec))

	// Second resolve should return the cached post-refresh token.
	res2, err := b.Resolve(context.Background(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer refreshed-token", res2.HTTPHeaders["Authorization"],
		"post-refresh token should be cached; second Resolve must hit cache")
}
