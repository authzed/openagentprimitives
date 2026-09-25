package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register turn/user_preference Kinds
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

const (
	prefsNS        = "ns"
	prefsSession   = "sess"
	prefsClass     = "reviewbot"
	prefsToken     = "tok-prefs"
	prefsChannelsd = "tok-channelsd"
	prefsSubject   = "YWxpY2U"
)

func newPreferencesScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

// languagePreference is one enum preference with a default, reused by every
// fixture class below.
func languagePreference(def string) v1alpha1.UserPreferenceSchema {
	raw, _ := json.Marshal(def)
	return v1alpha1.UserPreferenceSchema{
		Name:    "language",
		Type:    "enum",
		Enum:    []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}},
		Default: &apiextv1.JSON{Raw: raw},
	}
}

func buildClass(ns, name string, prefs []v1alpha1.UserPreferenceSchema) *v1alpha1.AgentClass {
	return &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.AgentClassSpec{UserPreferences: prefs},
	}
}

func buildSession(ns, name, class string) *v1alpha1.AgentSession {
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.AgentSessionSpec{Class: class},
	}
}

func buildSettings(ns string, classPrefs map[string]map[string]v1alpha1.PreferenceGlobal) *v1alpha1.AgentSettings {
	return &v1alpha1.AgentSettings{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.AgentSettingsName},
		Spec:       v1alpha1.SettingsSpec{ClassUserPreferences: classPrefs},
	}
}

// newPreferencesServer wires an httptest.Server whose handler carries the
// standard inmem Local, a session token authorized for ns/sess, and
// WithPreferences over a fake client seeded with objs.
func newPreferencesServer(t *testing.T, objs ...client.Object) (*httptest.Server, *memory.Local) {
	t.Helper()
	scheme := newPreferencesScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()

	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: prefsNS, Name: prefsSession}, prefsToken, "")
	reg.SetChannelsdToken(prefsChannelsd)

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithPreferences(c))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mem
}

// seedUserTurn appends a Role="user" turn at idx, attributed to author, into
// the ns/sess scope — mirroring how the turn accessor tests seed transcripts.
func seedUserTurn(t *testing.T, mem *memory.Local, idx int, author identity.Subject) {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsSession}
	appender := turn.NewAppender(mem, scope)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, appender.Append(ctx, memory.Turn{
		Index:     idx,
		Role:      "user",
		Content:   []memory.ContentBlock{{Type: "text", Text: "hello"}},
		CreatedAt: time.Unix(int64(idx), 0).UTC(),
		Author:    author,
	}))
}

// putUserPreference writes a user_preference entry directly into the user's
// own scope, as the operator's commit path would (ComponentWritten).
func putUserPreference(t *testing.T, mem *memory.Local, canonical, classNS, className, key string, value json.RawMessage) {
	t.Helper()
	scope, err := memory.UserScope(canonical)
	require.NoError(t, err)
	content, err := json.Marshal(userpreference.Preference{
		ClassNamespace: classNS,
		ClassName:      className,
		Key:            key,
		Value:          value,
	})
	require.NoError(t, err)
	ctx := memory.SystemContext(context.Background(), "test")
	_, err = mem.Put(ctx, memory.Entry{
		Scope:   scope,
		Kind:    userpreference.KindName,
		ID:      userpreference.EntryID(classNS, className, key),
		Content: content,
	})
	require.NoError(t, err)
}

func getPreferences(t *testing.T, srv *httptest.Server, path, token string) (*http.Response, preferences.SnapshotResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var snap preferences.SnapshotResponse
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	}
	return resp, snap
}

// postCommit POSTs a CommitRequest to /memory/_preferences_commit/{ns}/{name}
// as token, returning the raw response so callers can assert on status and
// body text (error bodies carry the "locked by admin policy" etc. substrings
// tests need to pin).
func postCommit(t *testing.T, srv *httptest.Server, ns, name, token string, req preferences.CommitRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/_preferences_commit/"+ns+"/"+name, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

// readBody reads and returns resp's full body as a string, for tests
// asserting on an error message's exact substring.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func jsonRaw(t *testing.T, v string) *apiextv1.JSON {
	t.Helper()
	return &apiextv1.JSON{Raw: []byte(v)}
}

func TestPreferencesCommit_HappyPath_ChannelsdWrites_ThenGetShowsSourceUser(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, identity.Subject("user:"+prefsSubject))

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	getResp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

func TestPreferencesCommit_SessionBearer_403(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsToken, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPreferencesCommit_NoBearer_401(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp := postCommit(t, srv, prefsNS, prefsSession, "", preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPreferencesCommit_UnknownKey_404(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "does-not-exist", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesCommit_LockedByValidGlobal_409(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	settings := buildSettings(prefsNS, map[string]map[string]v1alpha1.PreferenceGlobal{
		prefsClass: {"language": {Value: apiextv1.JSON{Raw: []byte(`"de"`)}, Lock: true}},
	})
	srv, _ := newPreferencesServer(t, class, sess, settings)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"en"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	respBody := readBody(t, resp)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, respBody, "locked by admin policy")
}

// TestPreferencesCommit_MalformedGlobalLock_DoesNotLock proves a global whose
// Value no longer type-checks against the schema does NOT lock the key —
// same rule Resolve applies at read time — so the commit proceeds.
func TestPreferencesCommit_MalformedGlobalLock_DoesNotLock(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	settings := buildSettings(prefsNS, map[string]map[string]v1alpha1.PreferenceGlobal{
		// "fr" is not a permitted enum value for this schema: malformed.
		prefsClass: {"language": {Value: apiextv1.JSON{Raw: []byte(`"fr"`)}, Lock: true}},
	})
	srv, _ := newPreferencesServer(t, class, sess, settings)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestPreferencesCommit_TypeMismatch_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"not-a-permitted-value"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPreferencesCommit_BadSubject_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: "not/a/bare/id",
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPreferencesCommit_MissingKeyOrSubject_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	cases := []struct {
		name string
		req  preferences.CommitRequest
	}{
		{name: "missing key", req: preferences.CommitRequest{Subject: prefsSubject, Value: jsonRaw(t, `"de"`)}},
		{name: "missing subject", req: preferences.CommitRequest{Key: "language", Value: jsonRaw(t, `"de"`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, tc.req)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPreferencesCommit_NullValue_TombstonesBackToDefault proves a nil/null
// Value clears the user layer: the commit succeeds and a follow-up GET
// resolves the key back to its class default rather than the prior user value.
func TestPreferencesCommit_NullValue_TombstonesBackToDefault(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, identity.Subject("user:"+prefsSubject))
	putUserPreference(t, mem, prefsSubject, prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp := postCommit(t, srv, prefsNS, prefsSession, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: nil, Subject: prefsSubject,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	getResp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceDefault, snap.Snapshot.Keys[0].Source)
}

func TestPreferencesGet_HappyPath_DefaultOnly(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "YWxpY2U", snap.Subject)
	assert.Equal(t, prefsNS, snap.ClassNamespace)
	assert.Equal(t, prefsClass, snap.ClassName)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceDefault, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"en"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

func TestPreferencesGet_UserValuePresent_SourceUser(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

func TestPreferencesGet_LockedGlobal_SourceLocked(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	settings := buildSettings(prefsNS, map[string]map[string]v1alpha1.PreferenceGlobal{
		prefsClass: {"language": {Value: apiextv1.JSON{Raw: []byte(`"de"`)}, Lock: true}},
	})
	srv, mem := newPreferencesServer(t, class, sess, settings)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")
	// A user value exists too, but the lock must win.
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"en"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceLocked, snap.Snapshot.Keys[0].Source)
	assert.True(t, snap.Snapshot.Keys[0].Locked)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

func TestPreferencesGet_EmptyAuthorTurn_SubjectEmptyAndUserLayerSkipped(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	// Agent-authored turn: empty Author.
	seedUserTurn(t, mem, 0, "")
	// Seed a user_preference row for a totally different canonical id: it must
	// never be consulted, since there is no verified author to key off of.
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "", snap.Subject)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceDefault, snap.Snapshot.Keys[0].Source)
}

// TestPreferencesGet_LaterToolResultTurn_StillResolvesHumanAuthor proves the
// no-?turn resolution keys off the latest HUMAN turn, not merely the latest
// user-ROLE turn. Tool-result turns are stored with role "user" too (the LLM
// protocol models a tool result as a user-role message; turn.EntryToTurn
// derives Role from the "-user" ID suffix) and carry an empty Author. During
// an active tool-calling loop — exactly when set_preference's readback and
// get_preferences run — the highest-index user-role turn is a tool result. If
// the resolver took its (empty) author, every preference read would fall
// through to the class default even for a user who just saved a value, which
// is the production bug this guards against.
func TestPreferencesGet_LaterToolResultTurn_StillResolvesHumanAuthor(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U") // the human message
	seedUserTurn(t, mem, 2, "")             // a later tool_result: role "user", empty Author
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "YWxpY2U", snap.Subject)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

// TestPreferencesGet_ExplicitTurnSelectsThatTurnsAuthor proves ?turn=N
// resolves for the AUTHOR OF THE NAMED TURN, not merely "some user turn":
// two turns by two different users, each with their own saved value, and
// each ?turn answer must carry that turn's author and value.
func TestPreferencesGet_ExplicitTurnSelectsThatTurnsAuthor(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")
	seedUserTurn(t, mem, 2, "user:Ym9i")
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))
	putUserPreference(t, mem, "Ym9i", prefsNS, prefsClass, "language", json.RawMessage(`"en"`))

	cases := []struct {
		name        string
		turn        string
		wantSubject string
		wantValue   string
	}{
		{name: "?turn=0 resolves the first author's value", turn: "0", wantSubject: "YWxpY2U", wantValue: `"de"`},
		{name: "?turn=2 resolves the second author's value", turn: "2", wantSubject: "Ym9i", wantValue: `"en"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, snap := getPreferences(t, srv,
				"/memory/_preferences/"+prefsNS+"/"+prefsSession+"?turn="+tc.turn, prefsToken)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			assert.Equal(t, tc.wantSubject, snap.Subject)
			require.Len(t, snap.Snapshot.Keys, 1)
			// Source "user" on both rows is what proves each request consulted
			// the NAMED turn author's own user layer — bob's saved value equals
			// the class default, so only the source separates them.
			assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
			require.NotNil(t, snap.Snapshot.Keys[0].Value)
			assert.JSONEq(t, tc.wantValue, string(snap.Snapshot.Keys[0].Value.Raw))
		})
	}
}

// TestPreferencesGet_NoTurnParam_HighestIndexUserTurnWins proves the
// fallback picks the LATEST user turn's author in a multiplayer transcript,
// not the first or an arbitrary one.
func TestPreferencesGet_NoTurnParam_HighestIndexUserTurnWins(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")
	seedUserTurn(t, mem, 2, "user:Ym9i")
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))
	putUserPreference(t, mem, "Ym9i", prefsNS, prefsClass, "language", json.RawMessage(`"en"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "Ym9i", snap.Subject, "the index-2 turn's author must win over index 0")
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"en"`, string(snap.Snapshot.Keys[0].Value.Raw))
}

// TestPreferencesGet_OtherClassValueDoesNotLeak proves the user-layer
// class filter: the same user's saved value for a DIFFERENT class shares
// the key name but must never reach this session's snapshot.
func TestPreferencesGet_OtherClassValueDoesNotLeak(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")
	// Same user, same key, DIFFERENT class: must be filtered out.
	putUserPreference(t, mem, "YWxpY2U", prefsNS, "other", "language", json.RawMessage(`"en"`))
	// The session class's own entry: this is the one the snapshot must carry.
	putUserPreference(t, mem, "YWxpY2U", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw),
		"the other class's \"en\" must not overwrite the session class's \"de\"")
}

func TestPreferencesGet_UnknownTurn_404(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp, _ := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession+"?turn=99", prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesGet_ClassWithoutPreferences_404(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, nil)
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServer(t, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")

	resp, _ := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesGet_NoBearer_401(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServer(t, class, sess)

	resp, _ := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, "")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
