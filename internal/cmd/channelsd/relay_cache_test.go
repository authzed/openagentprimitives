package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// relayCacheFixture models the cache as a SEPARATE store from the API server,
// so a test can hold it stale (or empty) while the API server moves on — the
// gap the relay client exists to manage.
type relayCacheFixture struct {
	api   client.Client
	cache client.Client
	rc    *relayClient
	now   time.Time
}

func newRelayCacheFixture(t *testing.T, apiObjs, cacheObjs []client.Object) *relayCacheFixture {
	t.Helper()
	f := &relayCacheFixture{
		api:   fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(apiObjs...).Build(),
		cache: fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(cacheObjs...).Build(),
		now:   time.Now(),
	}
	f.rc = newRelayClient(f.api, f.cache)
	f.rc.now = func() time.Time { return f.now }
	return f
}

func relayTestSession(annotations map[string]string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "sess-1", Annotations: annotations,
	}}
}

var relayTestSessionKey = client.ObjectKey{Namespace: "default", Name: "sess-1"}

func (f *relayCacheFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.rc.Get(context.Background(), relayTestSessionKey, &got), "Get through the relay client")
	return &got
}

// markOpeningSent patches the session through the relay client exactly as the
// relay's markSessionOpeningSent does: a merge patch on a bare object.
func (f *relayCacheFixture) markOpeningSent(t *testing.T) {
	t.Helper()
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{spiceboxv1alpha1.AnnotationSessionOpeningSent: "true"},
	}})
	require.NoError(t, err)
	require.NoError(t, f.rc.Patch(context.Background(),
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess-1"}},
		client.RawPatch(types.MergePatchType, patch)), "patch through the relay client")
}

func TestRelayClient_CachedTypeServedFromCache(t *testing.T) {
	f := newRelayCacheFixture(t,
		[]client.Object{relayTestSession(map[string]string{"from": "api"})},
		[]client.Object{relayTestSession(map[string]string{"from": "cache"})})

	assert.Equal(t, "cache", f.getSession(t).Annotations["from"],
		"an AgentSession read is served by the cache, costing the relay no API round trip")
}

func TestRelayClient_UncachedTypeGoesDirect(t *testing.T) {
	secret := func(v string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tok"}, Data: map[string][]byte{"v": []byte(v)}}
	}
	f := newRelayCacheFixture(t, []client.Object{secret("api")}, []client.Object{secret("cache")})

	var got corev1.Secret
	require.NoError(t, f.rc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "tok"}, &got))
	assert.Equal(t, "api", string(got.Data["v"]), "a type off the allowlist is never read from the cache")
}

func TestRelayClient_CacheMissFallsBackToAPI(t *testing.T) {
	f := newRelayCacheFixture(t, []client.Object{relayTestSession(map[string]string{"from": "api"})}, nil)

	assert.Equal(t, "api", f.getSession(t).Annotations["from"],
		"a session created a moment ago and not yet cached is still found — its first envelopes are not dropped")
}

// The read-your-writes cases. The relay patches the opening-sent marker and
// must see it on the very next envelope, or it posts the opening (and opens a
// thread) a second time.
func TestRelayClient_ReadYourWrites(t *testing.T) {
	stale := relayTestSession(nil)

	t.Run("cache behind the write: next read goes direct and sees it", func(t *testing.T) {
		f := newRelayCacheFixture(t, []client.Object{stale.DeepCopy()}, []client.Object{stale.DeepCopy()})
		f.markOpeningSent(t)

		assert.Equal(t, "true", f.getSession(t).Annotations[spiceboxv1alpha1.AnnotationSessionOpeningSent],
			"the relay's own patch is visible to its next read even though the cache has not caught up")
	})

	t.Run("cache shows the write: back on the cache", func(t *testing.T) {
		f := newRelayCacheFixture(t, nil, nil)
		k := freshKey{typ: reflect.TypeOf(&spiceboxv1alpha1.AgentSession{}), key: relayTestSessionKey}
		f.rc.fresh[k] = freshMark{resourceVersion: "42", until: f.now.Add(relayFreshWindow)}

		assert.True(t, f.rc.needsDirectRead(k, "41"), "a cached copy older than the write is not trusted")
		assert.False(t, f.rc.needsDirectRead(k, "42"), "a cached copy AT the write's version is")
		assert.Empty(t, f.rc.fresh, "and the mark is cleared, so later reads stay on the cache")
	})

	t.Run("window elapsed: back on the cache", func(t *testing.T) {
		f := newRelayCacheFixture(t, []client.Object{stale.DeepCopy()}, []client.Object{relayTestSession(map[string]string{"from": "cache"})})
		f.markOpeningSent(t)
		f.now = f.now.Add(relayFreshWindow)

		assert.Equal(t, "cache", f.getSession(t).Annotations["from"],
			"a write older than the window no longer forces direct reads")
		assert.Empty(t, f.rc.fresh, "the expired mark is cleared")
	})
}
