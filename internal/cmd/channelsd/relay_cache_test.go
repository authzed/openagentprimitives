package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The fake API server stamps a seeded object with this version and counts up
// from it on every write, so a test can place the cache behind, at, or past a
// write by number.
const (
	relayTestSeedRV  = "999"
	relayTestWriteRV = "1000"
	relayTestLaterRV = "1001"
)

// relayTestAnchor stands in for a kind's thread-anchor annotation: written by
// the inbound side before the turn, read back by the relay and the sender.
const relayTestAnchor = "test.agentprimitives.authzed.com/last-inbound-ts"

// relayCacheFixture models the cache as a SEPARATE store from the API server,
// so a test can hold it stale (or empty) while the API server moves on — the
// gap the relay client exists to manage. cli and rc are wired exactly as
// channelsd's run() wires them: one tracking client for every writer, one
// relay client over it for the cached readers.
type relayCacheFixture struct {
	api client.Client
	cli *trackingClient
	rc  *relayClient
	now time.Time

	logMu sync.Mutex
	logs  []string
}

func newRelayCacheFixture(t *testing.T, apiObjs, cacheObjs []client.Object) *relayCacheFixture {
	t.Helper()
	f := &relayCacheFixture{
		api: fake.NewClientBuilder().WithScheme(makeScheme()).
			WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
			WithObjects(apiObjs...).Build(),
		now: time.Now(),
	}
	tracker := newWriteTracker()
	tracker.now = func() time.Time { return f.now }
	tracker.lastSweep = f.now
	f.cli = newTrackingClient(f.api, tracker)
	f.rc = newRelayClient(f.cli, nil)
	f.setCache(t, cacheObjs...)
	return f
}

// setCache replaces what the cache holds, as a watch event arriving would.
func (f *relayCacheFixture) setCache(t *testing.T, objs ...client.Object) {
	t.Helper()
	f.rc.cache = fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(objs...).Build()
}

// ctx carries a logger that records every line, so a test can assert on what
// an operator would be able to grep for.
func (f *relayCacheFixture) ctx() context.Context {
	return ctrllog.IntoContext(context.Background(), funcr.New(func(_, args string) {
		f.logMu.Lock()
		defer f.logMu.Unlock()
		f.logs = append(f.logs, args)
	}, funcr.Options{}))
}

func (f *relayCacheFixture) logged() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return strings.Join(f.logs, "\n")
}

func relayTestSession(rv string, annotations map[string]string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "sess-1", ResourceVersion: rv, Annotations: annotations,
	}}
}

var relayTestSessionKey = client.ObjectKey{Namespace: "default", Name: "sess-1"}

func (f *relayCacheFixture) getSession(t *testing.T) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, f.rc.Get(f.ctx(), relayTestSessionKey, &got), "Get through the relay client")
	return &got
}

// annotate merge-patches one annotation onto the session through w, on a bare
// object — the shape of the relay's markSessionOpeningSent and of the inbound
// pipeline's pre-turn stamp alike.
func annotate(t *testing.T, w client.Writer, key, value string) {
	t.Helper()
	require.NoError(t, tryAnnotate(w, key, value), "annotation patch")
}

func tryAnnotate(w client.Writer, key, value string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{key: value},
	}})
	if err != nil {
		return err
	}
	return w.Patch(context.Background(),
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess-1"}},
		client.RawPatch(types.MergePatchType, patch))
}

func TestRelayClient_CachedTypeServedFromCache(t *testing.T) {
	f := newRelayCacheFixture(t,
		[]client.Object{relayTestSession("", map[string]string{"from": "api"})},
		[]client.Object{relayTestSession("", map[string]string{"from": "cache"})})

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
	f := newRelayCacheFixture(t, []client.Object{relayTestSession("", map[string]string{"from": "api"})}, nil)

	assert.Equal(t, "api", f.getSession(t).Annotations["from"],
		"a session created a moment ago and not yet cached is still found — its first envelopes are not dropped")
}

// The read-your-writes cases. Each starts with the API server and the cache
// agreeing, makes one write, and then moves the cache (or does not) to see
// which store the relay's next read is served from.
func TestRelayClient_ReadYourWrites(t *testing.T) {
	const marker = spiceboxv1alpha1.AnnotationSessionOpeningSent
	seeded := func() []client.Object { return []client.Object{relayTestSession("", nil)} }
	cachedAt := func(rv string) *spiceboxv1alpha1.AgentSession {
		return relayTestSession(rv, map[string]string{"from": "cache"})
	}

	t.Run("relay's own patch, cache behind: next read goes direct and sees it", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), seeded())
		annotate(t, f.rc, marker, "true")

		assert.Equal(t, "true", f.getSession(t).Annotations[marker],
			"the relay's own patch is visible to its next read even though the cache has not caught up")
	})

	t.Run("another component's patch, cache behind: the relay's read still sees it", func(t *testing.T) {
		// The inbound pipeline and the listeners hold the tracking client, not
		// the relay client. Their thread-anchor stamp must reach the relay's
		// (and the sender's) next read, or the reply is threaded under the
		// previous message.
		f := newRelayCacheFixture(t, seeded(), seeded())
		annotate(t, f.cli, relayTestAnchor, "1700000000.000200")

		assert.Equal(t, "1700000000.000200", f.getSession(t).Annotations[relayTestAnchor],
			"a write made through channelsd's shared client is read back by the relay client")
	})

	t.Run("status write, cache behind: next read goes direct", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), seeded())
		var sess spiceboxv1alpha1.AgentSession
		require.NoError(t, f.cli.Get(context.Background(), relayTestSessionKey, &sess))
		base := sess.DeepCopy()
		sess.Status.Phase = "Running"
		require.NoError(t, f.cli.Status().Patch(context.Background(), &sess, client.MergeFrom(base)), "status patch")

		assert.Equal(t, "Running", f.getSession(t).Status.Phase,
			"a status write is tracked like any other")
	})

	t.Run("cache at the write's version: back on the cache", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), seeded())
		annotate(t, f.cli, marker, "true")
		f.setCache(t, cachedAt(relayTestWriteRV))

		assert.Equal(t, "cache", f.getSession(t).Annotations["from"], "a cached copy AT the write's version is trusted")
		assert.Empty(t, f.cli.tracker.marks, "and the mark is cleared, so later reads stay on the cache")
	})

	t.Run("cache past the write's version: back on the cache", func(t *testing.T) {
		// Someone else wrote after channelsd did, and the watch delivered only
		// the later version. The cache will never show the exact version
		// channelsd's write produced, and it does not have to.
		f := newRelayCacheFixture(t, seeded(), seeded())
		annotate(t, f.cli, marker, "true")
		f.setCache(t, cachedAt(relayTestLaterRV))

		assert.Equal(t, "cache", f.getSession(t).Annotations["from"], "a cached copy PAST the write's version is trusted")
		assert.Empty(t, f.cli.tracker.marks)
	})

	t.Run("cache still behind after a long watch stall: still direct", func(t *testing.T) {
		// No timer hands the read back to a cache that has not shown the
		// write: a relist that takes a minute costs a minute of direct reads.
		f := newRelayCacheFixture(t, seeded(), []client.Object{cachedAt(relayTestSeedRV)})
		annotate(t, f.cli, marker, "true")
		f.now = f.now.Add(writeMarkMaxAge - time.Second)

		assert.Equal(t, "true", f.getSession(t).Annotations[marker],
			"the opening-sent marker is still read from the API server, so the opening is not posted twice")
		assert.Empty(t, f.logged(), "nothing was given up on, so nothing is logged")
	})

	t.Run("failed write: next read goes direct, then the cache is held to what that read returned", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), []client.Object{cachedAt(relayTestSeedRV)})
		// Move the API server on without the tracker knowing, then fail a
		// write on the resulting conflict: proof that some held copy is stale.
		annotate(t, f.api, "other-writer", "yes")
		var sess spiceboxv1alpha1.AgentSession
		require.NoError(t, f.cli.Get(context.Background(), relayTestSessionKey, &sess))
		sess.ResourceVersion = relayTestSeedRV
		err := f.cli.Update(context.Background(), &sess)
		require.True(t, apierrors.IsConflict(err), "the stale Update must conflict, got %v", err)

		assert.Equal(t, "yes", f.getSession(t).Annotations["other-writer"],
			"after a failed write the next read is live, not the copy the conflict just proved stale")

		f.setCache(t, cachedAt(relayTestSeedRV))
		assert.Equal(t, "yes", f.getSession(t).Annotations["other-writer"],
			"a cache still behind that live read is not trusted")
		f.setCache(t, cachedAt(relayTestWriteRV))
		assert.Equal(t, "cache", f.getSession(t).Annotations["from"],
			"a cache that has reached the version the live read returned is")
	})

	t.Run("delete: read is NotFound although the cache still holds the object", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), seeded())
		require.NoError(t, f.cli.Delete(context.Background(), relayTestSession("", nil)))

		var got spiceboxv1alpha1.AgentSession
		err := f.rc.Get(f.ctx(), relayTestSessionKey, &got)
		assert.True(t, apierrors.IsNotFound(err), "a session channelsd deleted is gone to its next read, got %v", err)
	})
}

// A mark is bookkeeping and cannot be kept forever behind a watch that has
// stopped delivering. When one is given up on, that is said out loud, with the
// object's identity — from then on the cache is trusted without proof.
func TestRelayClient_UnconfirmedWriteIsLoggedWhenGivenUpOn(t *testing.T) {
	const marker = spiceboxv1alpha1.AnnotationSessionOpeningSent
	seeded := func() []client.Object { return []client.Object{relayTestSession("", nil)} }
	stale := relayTestSession(relayTestSeedRV, map[string]string{"from": "cache"})

	t.Run("read past the bound, cache behind: this read is live, the give-up is logged, later reads trust the cache", func(t *testing.T) {
		f := newRelayCacheFixture(t, seeded(), []client.Object{stale.DeepCopy()})
		annotate(t, f.cli, marker, "true")
		f.now = f.now.Add(writeMarkMaxAge)
		f.cli.tracker.lastSweep = f.now // isolate the read path from the sweep

		assert.Equal(t, "true", f.getSession(t).Annotations[marker], "the read that gives up is still served live")
		logs := f.logged()
		assert.Contains(t, logs, "gave up waiting for the cache", "the give-up must be logged")
		assert.Contains(t, logs, "AgentSession default/sess-1", "with the object's identity")
		assert.Contains(t, logs, `"writeResourceVersion"="`+relayTestWriteRV+`"`, "the version the cache never showed")
		assert.Contains(t, logs, `"cachedResourceVersion"="`+relayTestSeedRV+`"`, "and the version it was stuck at")
		assert.Empty(t, f.cli.tracker.marks, "the mark is dropped")

		assert.Equal(t, "cache", f.getSession(t).Annotations["from"], "after which the cache is served")
	})

	sweepCases := []struct {
		name    string
		cache   []client.Object
		wantLog bool
	}{
		{name: "never read back, cache caught up: dropped quietly",
			cache: []client.Object{relayTestSession(relayTestWriteRV, nil)}},
		{name: "never read back, object gone from the cache: dropped quietly",
			cache: nil},
		{name: "never read back, cache still behind: dropped and logged",
			cache: []client.Object{stale.DeepCopy()}, wantLog: true},
	}
	for _, tc := range sweepCases {
		t.Run(tc.name, func(t *testing.T) {
			other := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess-2"}}
			f := newRelayCacheFixture(t, append(seeded(), other.DeepCopy()), nil)
			annotate(t, f.cli, marker, "true")
			f.setCache(t, append(tc.cache, other.DeepCopy())...)
			f.now = f.now.Add(writeMarkMaxAge)

			// Any cached read runs the sweep; this one is of a different session.
			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, f.rc.Get(f.ctx(), client.ObjectKey{Namespace: "default", Name: "sess-2"}, &got))

			assert.Empty(t, f.cli.tracker.marks, "a mark nobody read back does not outlive the bound")
			if tc.wantLog {
				assert.Contains(t, f.logged(), "gave up waiting for the cache")
				assert.Contains(t, f.logged(), "AgentSession default/sess-1")
			} else {
				assert.Empty(t, f.logged(), "a write the cache accounts for is not worth a log line")
			}
		})
	}
}

// The real client decodes an API response INTO the object it is handed and
// leaves alone whatever the response does not mention. A direct read made on
// top of a cached copy would therefore keep every field the cached copy had and
// the live object has since lost.
func TestRelayClient_DirectReadIsNotDecodedOverTheCachedCopy(t *testing.T) {
	f := newRelayCacheFixture(t,
		[]client.Object{relayTestSession("", nil)},
		[]client.Object{relayTestSession(relayTestSeedRV, map[string]string{"removed-since": "still-in-cache"})})
	annotate(t, f.cli, relayTestAnchor, "1")

	// Decode like the real client: unmarshal over obj without clearing it.
	live := interceptor.NewClient(f.api.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			fresh := obj.DeepCopyObject().(client.Object)
			if err := c.Get(ctx, key, fresh, opts...); err != nil {
				return err
			}
			raw, err := json.Marshal(fresh)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, obj)
		},
	})
	f.rc.Client = newTrackingClient(live, f.cli.tracker)

	got := f.getSession(t)
	assert.Equal(t, "1", got.Annotations[relayTestAnchor], "the read went direct")
	assert.NotContains(t, got.Annotations, "removed-since",
		"an annotation only the stale cached copy had must not survive into a direct read")
}

// fillNonZero sets every exported field of the struct v to some non-zero value,
// descending into nested structs.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int32, reflect.Int64:
			f.SetInt(1)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Map:
			f.Set(reflect.MakeMap(f.Type()))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Struct:
			fillNonZero(t, f)
		default:
			require.Failf(t, "fillNonZero", "no rule for field %s of kind %s", v.Type().Field(i).Name, f.Kind())
		}
	}
}

// What the cache keeps is what the relay and every Sender can read. The trimmed
// status fields are pinned by name, and every OTHER status field — including
// one added after this test was written — must survive, so a field can only
// ever be dropped from the cache deliberately.
func TestRelayCacheTransform(t *testing.T) {
	trimmed := []string{
		"BundleSessions", "ResolvedSidecarToolboxes", "ResolvedWorkspaceSource", "SidecarReachability",
		"ResolvedContentGuardDetectors", "ResolvedSkillBundles", "ObservedPins", "SlotPins",
		"CredentialAuthFailures", "Progress", "CompletionBypasses", "RunnerNotes", "SatisfiedSecretOutputs",
		"AuditChainHeads", "EstimatedCost", "ToolGuard", "ActiveWidgets", "PermissionSurface",
		"PassthroughCredHashes", "InteractorTuplesWritten",
	}
	// Read on the delivery path today; named so a failure says what broke.
	mustKeep := []string{
		"Phase", "Result", "FailureReason", "Conditions", "PendingInteractions", "PendingRequesters",
		"PendingRestart", "EffectiveSettings", "PinnedMessage", "SupersededBy",
	}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "sess-1", ResourceVersion: "7",
			Labels: map[string]string{"team": "a"},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationSessionOpeningSent: "true",
				relayTestAnchor:       "1700000000.000200",
				lastAppliedAnnotation: `{"a very":"large copy of the manifest"}`,
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "operator"}},
		},
	}
	fillNonZero(t, reflect.ValueOf(&sess.Spec).Elem())
	fillNonZero(t, reflect.ValueOf(&sess.Status).Elem())
	want := sess.DeepCopy()

	out, err := relayCacheTransform(sess)
	require.NoError(t, err)
	got, ok := out.(*spiceboxv1alpha1.AgentSession)
	require.True(t, ok, "the transform returns the object it was given")

	assert.Equal(t, want.Spec, got.Spec, "the whole spec is kept: class, bindings, parent, forkedFrom")
	assert.Equal(t, want.Labels, got.Labels)
	assert.Equal(t, "7", got.ResourceVersion, "read-your-writes compares this")
	assert.Equal(t, map[string]string{
		spiceboxv1alpha1.AnnotationSessionOpeningSent: "true",
		relayTestAnchor: "1700000000.000200",
	}, got.Annotations, "every annotation but kubectl's last-applied copy is kept")
	assert.Nil(t, got.ManagedFields)

	var zeroed []string
	st := reflect.ValueOf(got.Status)
	for i := range st.NumField() {
		if st.Field(i).IsZero() {
			zeroed = append(zeroed, st.Type().Field(i).Name)
		}
	}
	assert.ElementsMatch(t, trimmed, zeroed, "exactly the named status fields are dropped, and nothing else")
	for _, name := range mustKeep {
		f := st.FieldByName(name)
		require.True(t, f.IsValid(), "AgentSessionStatus.%s no longer exists; update this list", name)
		assert.False(t, f.IsZero(), "status.%s is read on the delivery path and must survive the transform", name)
	}

	t.Run("other kinds keep their status; only metadata is trimmed", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default", Name: "c1",
				Annotations:   map[string]string{lastAppliedAnnotation: "{}", "keep": "me"},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			},
			Spec:   spiceboxv1alpha1.ChannelSpec{Kind: "fake", Role: "both"},
			Status: spiceboxv1alpha1.ChannelStatus{Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.ChannelConditionDeliverable}}},
		}
		wantCh := ch.DeepCopy()
		_, err := relayCacheTransform(ch)
		require.NoError(t, err)
		assert.Equal(t, wantCh.Spec, ch.Spec)
		assert.Equal(t, wantCh.Status, ch.Status)
		assert.Equal(t, map[string]string{"keep": "me"}, ch.Annotations)
		assert.Nil(t, ch.ManagedFields)
	})

	t.Run("a deletion tombstone passes through untouched", func(t *testing.T) {
		in := struct{ Key string }{Key: "default/sess-1"}
		out, err := relayCacheTransform(in)
		require.NoError(t, err)
		assert.Equal(t, in, out)
	})
}

// Read-your-writes covers a write only if it went through the tracking client,
// and a writer handed the raw client instead would fail silently: its write
// would just read back stale, some of the time. So run() may name the raw
// client exactly twice — where it is built and where it is wrapped.
func TestRun_OnlyTheTrackingClientIsHandedOut(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse main.go")

	uses := 0
	wrapped := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "directCli" {
				uses++
			}
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "newTrackingClient" && len(n.Args) > 0 {
				if arg, ok := n.Args[0].(*ast.Ident); ok && arg.Name == "directCli" {
					wrapped = true
				}
			}
		}
		return true
	})
	assert.True(t, wrapped, "run() wraps the raw client in newTrackingClient")
	assert.Equal(t, 2, uses,
		"the raw client is declared and wrapped, and nothing else holds it: every component's writes must be tracked")
}
