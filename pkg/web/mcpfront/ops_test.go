package mcpfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memartifact "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// --- scripted AccessTokenAuthz fake -----------------------------------------

// scriptedAuthz is the ops tests' fake AccessTokenAuthz: a decision table a
// test can script per-call, plus a record of every call it received — the
// "pin the exact permission names" assertions read opCalls/mirrorCalls back.
type scriptedAuthz struct {
	mu sync.Mutex

	opCalls        []spicedb.AccessTokenCheck
	opDecision     func(chk spicedb.AccessTokenCheck) (spicedb.AccessTokenDecision, error)
	mirrorCalls    []string
	mirrorDecision func(permission string) (bool, error)
	coveredClasses map[string]bool
	filterCalls    [][]string
}

func newScriptedAuthz() *scriptedAuthz {
	return &scriptedAuthz{coveredClasses: map[string]bool{}}
}

func (s *scriptedAuthz) CheckAccessTokenOp(_ context.Context, chk spicedb.AccessTokenCheck, _ bool) (spicedb.AccessTokenDecision, error) {
	s.mu.Lock()
	s.opCalls = append(s.opCalls, chk)
	s.mu.Unlock()
	if s.opDecision != nil {
		return s.opDecision(chk)
	}
	return spicedb.AccessTokenDecision{TokenGrants: true, ScopeCovers: true, OwnerHas: true}, nil
}

func (s *scriptedAuthz) CheckAccessTokenMirror(_ context.Context, _ string, _ identity.CanonicalUserID, permission string, _ bool) (bool, error) {
	s.mu.Lock()
	s.mirrorCalls = append(s.mirrorCalls, permission)
	s.mu.Unlock()
	if s.mirrorDecision != nil {
		return s.mirrorDecision(permission)
	}
	return true, nil
}

func (s *scriptedAuthz) FilterAccessTokenCoveredClasses(_ context.Context, _ string, classIDs []string, _ bool) (map[string]bool, error) {
	s.mu.Lock()
	s.filterCalls = append(s.filterCalls, append([]string(nil), classIDs...))
	s.mu.Unlock()
	out := make(map[string]bool, len(classIDs))
	for _, c := range classIDs {
		if s.coveredClasses[c] {
			out[c] = true
		}
	}
	return out, nil
}

var _ AccessTokenAuthz = (*scriptedAuthz)(nil)

// --- fake Deps ---------------------------------------------------------------

// fakeOpsDeps is ops_test.go's Deps double: a real controller-runtime fake
// K8s client, a scripted AccessTokenAuthz, a real in-memory artifacts.Service
// (Artifacts() returns a concrete *artifacts.Service, never an interface —
// there is no faking it), and test-controlled operator URL/memory token so
// livemirror.ReadHistory / httpclient.Search can be pointed at an httptest
// server.
type fakeOpsDeps struct {
	k8s         client.Client
	authz       *scriptedAuthz
	operatorURL string
	memoryToken string
	artifactSvc *artifacts.Service
	artifactMem memory.Memory
	fetchBytes  []byte
	fetchMIME   string
	fetchErr    error

	lookupSessions spicedb.InteractableSessions
	lookupErr      error
	lookupCalls    int
}

func newOpsFakeDeps(t *testing.T, c client.Client) *fakeOpsDeps {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	return &fakeOpsDeps{
		k8s:         c,
		authz:       newScriptedAuthz(),
		artifactSvc: artifacts.NewService(mem, nil),
		artifactMem: mem,
	}
}

func (f *fakeOpsDeps) K8s() client.Client                 { return f.k8s }
func (f *fakeOpsDeps) AccessTokenAuthz() AccessTokenAuthz { return f.authz }
func (f *fakeOpsDeps) AccessTokenNamespace() string       { return "agentprimitives-system" }
func (f *fakeOpsDeps) OperatorURL() string                { return f.operatorURL }
func (f *fakeOpsDeps) MemoryToken() string                { return f.memoryToken }
func (f *fakeOpsDeps) Artifacts() *artifacts.Service       { return f.artifactSvc }
func (f *fakeOpsDeps) LookupReadableSessions(_ context.Context, _ identity.CanonicalUserID) (spicedb.InteractableSessions, error) {
	f.lookupCalls++
	return f.lookupSessions, f.lookupErr
}
func (f *fakeOpsDeps) FetchArtifact(_ context.Context, _, _, _ string) ([]byte, string, error) {
	return f.fetchBytes, f.fetchMIME, f.fetchErr
}
func (f *fakeOpsDeps) ExternalBaseURL() string { return "https://example.test" }
func (f *fakeOpsDeps) Logger() logr.Logger     { return logr.Discard() }

var _ Deps = (*fakeOpsDeps)(nil)

// --- fixtures ----------------------------------------------------------------

func opsTestOwner() identity.CanonicalUserID {
	return identity.CanonicalFromTrusted("alice", "test fixture")
}

func opsTestActing() acting {
	return acting{TokenID: "at-demo", Owner: opsTestOwner()}
}

func newOpsTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newMintScheme(t)).WithObjects(objects...).Build()
}

// sessionFixture builds a minimal, non-delegated AgentSession CR: the
// "demo-agent"/"demo-session" shaped fixture the brief calls for, extended
// with a namespace/name/class so tests can build several at once.
func sessionFixture(ns, name, class string) *spiceboxv1alpha1.AgentSession {
	startedAt := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:     spiceboxv1alpha1.AgentSessionPhaseRunning,
			StartedAt: &startedAt,
		},
	}
}

// opsTurnEntry builds a memory Entry in the wire shape turn.EntryToTurn
// decodes — the same shape pkg/web/webui/livemirror's own tests use
// (historyTestEntry), copied locally since that helper is unexported in
// another package.
func opsTurnEntry(t *testing.T, ns, name string, idx int, role, text string, at time.Time) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   content,
	}
}

// --- tests ---------------------------------------------------------------

func TestAuthorizeSessionOpDeniesWithoutLeakingExistence(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()

	t.Run("session absent: denied with the standard text, authz never consulted", func(t *testing.T) {
		d := newOpsFakeDeps(t, newOpsTestClient(t))
		_, err := authorizeSessionOp(ctx, d, act, permReadTranscript, "demo-ns", "demo-session")
		require.ErrorIs(t, err, errSessionNotAccessible)
		assert.Empty(t, d.authz.opCalls, "a missing CR must never reach the three-legged check")
	})

	t.Run("session present but a leg denies: same text as absent", func(t *testing.T) {
		sess := sessionFixture("demo-ns", "demo-session", "demo-agent")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		d.authz.opDecision = func(spicedb.AccessTokenCheck) (spicedb.AccessTokenDecision, error) {
			return spicedb.AccessTokenDecision{TokenGrants: true, ScopeCovers: false, OwnerHas: true}, nil
		}
		_, err := authorizeSessionOp(ctx, d, act, permReadTranscript, "demo-ns", "demo-session")
		require.ErrorIs(t, err, errSessionNotAccessible)
	})

	t.Run("all three legs true: session returned", func(t *testing.T) {
		sess := sessionFixture("demo-ns", "demo-session", "demo-agent")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		got, err := authorizeSessionOp(ctx, d, act, permReadTranscript, "demo-ns", "demo-session")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "demo-session", got.Name)
		require.Len(t, d.authz.opCalls, 1)
		assert.Equal(t, permReadTranscript, d.authz.opCalls[0].Permission)
		assert.Equal(t, "agentsession", d.authz.opCalls[0].ResourceType)
		assert.Equal(t, "demo-ns/demo-session", d.authz.opCalls[0].ResourceID)
		assert.Equal(t, "demo-ns/demo-agent", d.authz.opCalls[0].ClassID)
	})

	t.Run("absent vs denied are byte-identical", func(t *testing.T) {
		_, errAbsent := authorizeSessionOp(ctx, newOpsFakeDeps(t, newOpsTestClient(t)), act, permReadTranscript, "demo-ns", "demo-session")

		sess := sessionFixture("demo-ns", "demo-session", "demo-agent")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		d.authz.opDecision = func(spicedb.AccessTokenCheck) (spicedb.AccessTokenDecision, error) {
			return spicedb.AccessTokenDecision{}, nil
		}
		_, errDenied := authorizeSessionOp(ctx, d, act, permReadTranscript, "demo-ns", "demo-session")

		require.Error(t, errAbsent)
		require.Error(t, errDenied)
		assert.Equal(t, errAbsent.Error(), errDenied.Error())
	})
}

func TestListSessionsFiltersByCoverageAndClass(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()

	t.Run("covered-class sessions only", func(t *testing.T) {
		sessA1 := sessionFixture("demo-ns", "sess-a1", "class-a")
		sessA2 := sessionFixture("demo-ns", "sess-a2", "class-a")
		sessB1 := sessionFixture("demo-ns", "sess-b1", "class-b")

		d := newOpsFakeDeps(t, newOpsTestClient(t, sessA1, sessA2, sessB1))
		d.lookupSessions = spicedb.InteractableSessions{Refs: []spicedb.SessionRef{
			{Namespace: "demo-ns", Name: "sess-a1"},
			{Namespace: "demo-ns", Name: "sess-a2"},
			{Namespace: "demo-ns", Name: "sess-b1"},
		}}
		d.authz.coveredClasses = map[string]bool{"demo-ns/class-a": true}

		out, err := opListSessions(ctx, d, act, ListSessionsIn{})
		require.NoError(t, err)
		require.Len(t, out.Sessions, 2, "only the two class-a sessions are covered")
		for _, s := range out.Sessions {
			assert.Equal(t, "class-a", s.Class)
		}
		assert.Equal(t, 1, d.lookupCalls)
	})

	t.Run("mirror check false: empty result, no lookup, no K8s reads", func(t *testing.T) {
		sess := sessionFixture("demo-ns", "sess-a1", "class-a")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		d.authz.mirrorDecision = func(string) (bool, error) { return false, nil }
		d.lookupSessions = spicedb.InteractableSessions{Refs: []spicedb.SessionRef{{Namespace: "demo-ns", Name: "sess-a1"}}}

		out, err := opListSessions(ctx, d, act, ListSessionsIn{})
		require.NoError(t, err)
		assert.Empty(t, out.Sessions)
		assert.Equal(t, 0, d.lookupCalls, "a false mirror check must never consult the lookup")
	})
}

func TestGetTranscriptPaginates(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()
	sess := sessionFixture("demo-ns", "demo-session", "demo-agent")

	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		opsTurnEntry(t, "demo-ns", "demo-session", 0, "user", "m0", at),
		opsTurnEntry(t, "demo-ns", "demo-session", 1, "assistant", "m1", at.Add(time.Second)),
		opsTurnEntry(t, "demo-ns", "demo-session", 2, "user", "m2", at.Add(2*time.Second)),
		opsTurnEntry(t, "demo-ns", "demo-session", 3, "assistant", "m3", at.Add(3*time.Second)),
		opsTurnEntry(t, "demo-ns", "demo-session", 4, "user", "m4", at.Add(4*time.Second)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
	d.operatorURL = srv.URL
	d.memoryToken = "webd-token"

	t.Run("middle page", func(t *testing.T) {
		out, err := opGetTranscript(ctx, d, act, GetTranscriptIn{Namespace: "demo-ns", Name: "demo-session", Offset: 2, Limit: 2})
		require.NoError(t, err)
		assert.Equal(t, 5, out.Total)
		require.Len(t, out.Entries, 2, "entries 3 and 4 of 5")
		assert.Equal(t, "m2", out.Entries[0].Text)
		assert.Equal(t, "m3", out.Entries[1].Text)
	})

	t.Run("offset past end: empty page, Total still correct", func(t *testing.T) {
		out, err := opGetTranscript(ctx, d, act, GetTranscriptIn{Namespace: "demo-ns", Name: "demo-session", Offset: 50, Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, 5, out.Total)
		assert.Empty(t, out.Entries)
	})

	t.Run("default limit when unset", func(t *testing.T) {
		out, err := opGetTranscript(ctx, d, act, GetTranscriptIn{Namespace: "demo-ns", Name: "demo-session"})
		require.NoError(t, err)
		assert.Equal(t, 5, out.Total)
		assert.Len(t, out.Entries, 5, "fewer entries than the default limit returns them all")
	})
}

func TestSearchMemoryScopesToAuthorizedSessionsOnly(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()

	sessA := sessionFixture("demo-ns", "sess-a", "class-a")
	sessB := sessionFixture("demo-ns", "sess-b", "class-b")

	t.Run("derived scope list is exactly the covered sessions", func(t *testing.T) {
		var gotScopes []memory.Scope
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req memory.SearchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotScopes = req.Scopes
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(memory.MergedSearchResult{}))
		}))
		defer srv.Close()

		d := newOpsFakeDeps(t, newOpsTestClient(t, sessA, sessB))
		d.operatorURL = srv.URL
		d.memoryToken = "webd-token"
		d.lookupSessions = spicedb.InteractableSessions{Refs: []spicedb.SessionRef{
			{Namespace: "demo-ns", Name: "sess-a"},
			{Namespace: "demo-ns", Name: "sess-b"},
		}}
		d.authz.coveredClasses = map[string]bool{"demo-ns/class-a": true}

		out, err := opSearchMemory(ctx, d, act, SearchMemoryIn{Query: "hello"})
		require.NoError(t, err)
		assert.False(t, out.SearchUnavailable)
		require.Len(t, gotScopes, 1, "only the covered session's scope is ever sent")
		assert.Equal(t, memory.Scope{Kind: "session", ID: "demo-ns/sess-a"}, gotScopes[0])
		assert.Contains(t, d.authz.mirrorCalls, permRead, "the derived branch mirrors on \"read\", not \"read_transcript\"")
	})

	t.Run("no search providers configured: SearchUnavailable, no error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no search providers configured", http.StatusNotFound)
		}))
		defer srv.Close()

		sess := sessionFixture("demo-ns", "sess-a", "class-a")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		d.operatorURL = srv.URL
		d.memoryToken = "webd-token"
		d.lookupSessions = spicedb.InteractableSessions{Refs: []spicedb.SessionRef{{Namespace: "demo-ns", Name: "sess-a"}}}
		d.authz.coveredClasses = map[string]bool{"demo-ns/class-a": true}

		out, err := opSearchMemory(ctx, d, act, SearchMemoryIn{Query: "x"})
		require.NoError(t, err)
		assert.True(t, out.SearchUnavailable)
		assert.Empty(t, out.Hits)
	})

	t.Run("explicit session uses authorizeSessionOp with perm read, single scope", func(t *testing.T) {
		var gotScopes []memory.Scope
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req memory.SearchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			gotScopes = req.Scopes
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(memory.MergedSearchResult{}))
		}))
		defer srv.Close()

		sess := sessionFixture("demo-ns", "demo-session", "demo-agent")
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		d.operatorURL = srv.URL
		d.memoryToken = "webd-token"

		_, err := opSearchMemory(ctx, d, act, SearchMemoryIn{Query: "x", Namespace: "demo-ns", Name: "demo-session"})
		require.NoError(t, err)
		require.Len(t, gotScopes, 1)
		assert.Equal(t, memory.Scope{Kind: "session", ID: "demo-ns/demo-session"}, gotScopes[0])
		require.Len(t, d.authz.opCalls, 1)
		assert.Equal(t, permRead, d.authz.opCalls[0].Permission)
	})
}

// TestRoleMatrixOnTools pins BOTH permission names each tool uses: the token
// mirror name leg 1 checks ($sameperm contract) and the resource permission
// leg 3 checks on the agentsession (the view/read → read_transcript mapping;
// "" means leg 3 defaults to the mirror name). A drift on either side of
// sessionResourcePermission's map is a regression this table catches by name.
func TestRoleMatrixOnTools(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()
	sess := sessionFixture("demo-ns", "demo-session", "demo-agent")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{}))
	}))
	defer srv.Close()

	type permPair struct{ mirror, resource string }

	cases := []struct {
		tool       string
		run        func(t *testing.T, d *fakeOpsDeps) error
		wantOp     *permPair // the CheckAccessTokenOp (mirror, ResourcePermission) pair; nil = no three-leg check
		wantMirror string    // the CheckAccessTokenMirror permission; "" = no mirror-only check
	}{
		{
			tool: "get_session",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opGetSession(ctx, d, act, GetSessionIn{Namespace: "demo-ns", Name: "demo-session"})
				return err
			},
			wantOp: &permPair{mirror: permReadTranscript, resource: ""},
		},
		{
			tool: "get_transcript",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opGetTranscript(ctx, d, act, GetTranscriptIn{Namespace: "demo-ns", Name: "demo-session"})
				return err
			},
			wantOp: &permPair{mirror: permReadTranscript, resource: ""},
		},
		{
			tool: "search_memory (explicit session)",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opSearchMemory(ctx, d, act, SearchMemoryIn{Query: "x", Namespace: "demo-ns", Name: "demo-session"})
				return err
			},
			wantOp: &permPair{mirror: permRead, resource: permReadTranscript},
		},
		{
			tool: "search_memory (derived)",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opSearchMemory(ctx, d, act, SearchMemoryIn{Query: "x"})
				return err
			},
			wantMirror: permRead,
		},
		{
			tool: "list_artifacts",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opListArtifacts(ctx, d, act, ListArtifactsIn{Namespace: "demo-ns", Name: "demo-session"})
				return err
			},
			wantOp: &permPair{mirror: permView, resource: permReadTranscript},
		},
		{
			tool: "get_artifact",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opGetArtifact(ctx, d, act, GetArtifactIn{Namespace: "demo-ns", Name: "demo-session", ArtifactID: "artifact-1"})
				return err
			},
			wantOp: &permPair{mirror: permView, resource: permReadTranscript},
		},
		{
			tool: "list_sessions",
			run: func(t *testing.T, d *fakeOpsDeps) error {
				_, err := opListSessions(ctx, d, act, ListSessionsIn{})
				return err
			},
			wantMirror: permReadTranscript,
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool+": pins mirror + resource permission names", func(t *testing.T) {
			d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
			d.operatorURL = srv.URL
			d.memoryToken = "webd-token"
			seedArtifact(t, d.artifactMem, "demo-ns", "demo-session", "artifact-1")
			// A read-role token's mirror grants every read-surface permission
			// but denies "interact" (a send-class permission never checked by
			// this read-only tool set) — the scripted decision table the brief
			// asks for.
			d.authz.mirrorDecision = func(permission string) (bool, error) { return permission != "interact", nil }

			require.NoError(t, tc.run(t, d))

			if tc.wantOp != nil {
				require.Len(t, d.authz.opCalls, 1, "exactly one three-leg check per authorized op")
				assert.Equal(t, tc.wantOp.mirror, d.authz.opCalls[0].Permission, "leg-1 mirror name")
				assert.Equal(t, tc.wantOp.resource, d.authz.opCalls[0].ResourcePermission, "leg-3 resource permission")
			}
			if tc.wantMirror != "" {
				require.Len(t, d.authz.mirrorCalls, 1, "exactly one mirror-only check per enumeration op")
				assert.Equal(t, tc.wantMirror, d.authz.mirrorCalls[0])
			}
			for _, c := range d.authz.opCalls {
				assert.NotEqual(t, "interact", c.Permission, "interact is a send-class permission; no read-role tool may check it")
				assert.NotEqual(t, "interact", c.ResourcePermission)
			}
			for _, p := range d.authz.mirrorCalls {
				assert.NotEqual(t, "interact", p)
			}
		})
	}
}

func TestGetArtifactContentTruncatesAndRejectsNonUTF8(t *testing.T) {
	ctx := context.Background()
	act := opsTestActing()
	sess := sessionFixture("demo-ns", "demo-session", "demo-agent")

	newSeededDeps := func(t *testing.T) *fakeOpsDeps {
		t.Helper()
		d := newOpsFakeDeps(t, newOpsTestClient(t, sess))
		seedArtifact(t, d.artifactMem, "demo-ns", "demo-session", "artifact-1")
		return d
	}

	t.Run("content over the cap is truncated at a rune boundary", func(t *testing.T) {
		d := newSeededDeps(t)
		d.fetchBytes = make([]byte, maxArtifactContentBytes+10)
		for i := range d.fetchBytes {
			d.fetchBytes[i] = 'a'
		}

		out, err := opGetArtifact(ctx, d, act, GetArtifactIn{Namespace: "demo-ns", Name: "demo-session", ArtifactID: "artifact-1"})
		require.NoError(t, err)
		assert.True(t, out.ContentTruncated)
		assert.LessOrEqual(t, len(out.Content), maxArtifactContentBytes)
		assert.Equal(t, "artifact-1", out.Artifact.ArtifactID)
	})

	t.Run("non-UTF8 content returns no inline content", func(t *testing.T) {
		d := newSeededDeps(t)
		d.fetchBytes = []byte{0xff, 0xfe, 0xfd}

		out, err := opGetArtifact(ctx, d, act, GetArtifactIn{Namespace: "demo-ns", Name: "demo-session", ArtifactID: "artifact-1"})
		require.NoError(t, err)
		assert.Empty(t, out.Content)
		assert.False(t, out.ContentTruncated)
	})

	t.Run("small content round-trips untruncated", func(t *testing.T) {
		d := newSeededDeps(t)
		d.fetchBytes = []byte("hello artifact")

		out, err := opGetArtifact(ctx, d, act, GetArtifactIn{Namespace: "demo-ns", Name: "demo-session", ArtifactID: "artifact-1"})
		require.NoError(t, err)
		assert.Equal(t, "hello artifact", out.Content)
		assert.False(t, out.ContentTruncated)
		assert.Equal(t, "html", out.Artifact.RendererKind)
	})

	t.Run("unknown artifact id errors", func(t *testing.T) {
		d := newSeededDeps(t)
		_, err := opGetArtifact(ctx, d, act, GetArtifactIn{Namespace: "demo-ns", Name: "demo-session", ArtifactID: "nope"})
		require.Error(t, err)
	})
}

// seedArtifact writes a single artifact head directly into mem, bypassing the
// full FinalizeRevision flow (CRs, renders, revisions) this test has no need
// for — only ListArtifacts/get_artifact's metadata read is exercised here;
// content always comes from the (faked) FetchArtifact, never from this entry.
func seedArtifact(t *testing.T, mem memory.Memory, ns, name, artifactID string) {
	t.Helper()
	content, err := json.Marshal(memartifact.Artifact{Name: "Report", Description: "a test artifact", RendererKind: "html"})
	require.NoError(t, err)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	_, err = mem.Put(ctx, memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: ns + "/" + name},
		Kind:      memartifact.KindName,
		ID:        artifactID,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Content:   content,
	})
	require.NoError(t, err)
}

func TestSessionIsEnded(t *testing.T) {
	assert.False(t, sessionIsEnded(spiceboxv1alpha1.AgentSessionPhaseRunning))
	assert.False(t, sessionIsEnded(""))
	assert.True(t, sessionIsEnded(spiceboxv1alpha1.AgentSessionPhaseSucceeded))
	assert.True(t, sessionIsEnded(spiceboxv1alpha1.AgentSessionPhaseFailed))
}
