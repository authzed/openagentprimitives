package httpsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferencewrite"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// selfVisiblePreference is a "notifications" schema with visibility unset
// (self-only) — the differentiator this route's full-schema contract needs
// to prove: it must appear in a first-party read even though it would never
// appear in a ?user-ref= one.
func selfVisiblePreference(def string) v1alpha1.UserPreferenceSchema {
	raw, _ := json.Marshal(def)
	return v1alpha1.UserPreferenceSchema{
		Name:    "notifications",
		Type:    "enum",
		Enum:    []v1alpha1.PreferenceEnumValue{{Value: "all"}, {Value: "none"}},
		Default: &apiextv1.JSON{Raw: raw},
	}
}

// newFirstPartyServer wires an httptest.Server carrying an inmem Local, a
// channelsd token, and WithPreferences over a fake client seeded with objs —
// this route has no per-session bearer at all (the App Home has no session),
// so the only credential ever exercised is the channelsd system token.
func newFirstPartyServer(t *testing.T, objs ...client.Object) (*httptest.Server, *memory.Local) {
	t.Helper()
	scheme := newPreferencesScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()

	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken(prefsChannelsd)

	h := httpsrv.NewHandler(mem, reg, httpsrv.WithPreferences(c))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mem
}

// getFirstParty GETs /memory/_preferences_firstparty/{ns}/{className} as
// token, with subject appended as ?subject= when non-empty (an empty
// subject exercises the "missing ?subject=" 400 case without hand-building
// the query string per call site).
func getFirstParty(t *testing.T, srv *httptest.Server, ns, className, subject, token string) (*http.Response, preferences.SnapshotResponse) {
	t.Helper()
	path := "/memory/_preferences_firstparty/" + ns + "/" + className
	if subject != "" {
		path += "?subject=" + subject
	}
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

// TestPreferencesFirstPartyGet_HappyPath_FullSchema proves the differentiator
// from ?user-ref=: the UNION of self-only AND class-visible keys appears in
// the SAME response, because the caller is the subject reading their OWN
// data. classVisiblePreference (a visibility:class "language" key, from
// preferences_userref_test.go's shared package scope) and
// selfVisiblePreference (a self-only "notifications" key) are declared
// together; a copy-pasted ?user-ref= visibility filter would drop the
// self-only key entirely, so asserting BOTH by name is what pins the union.
// The user value is seeded on the SELF-ONLY key on purpose: it is precisely
// the value a ?user-ref= read would never disclose, so its presence here is
// the strongest single proof this route does not filter.
func TestPreferencesFirstPartyGet_HappyPath_FullSchema(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{
		classVisiblePreference("en"), // visibility: class, name "language"
		selfVisiblePreference("all"), // self-only (visibility unset), name "notifications"
	})
	srv, mem := newFirstPartyServer(t, class)
	putUserPreference(t, mem, prefsSubject, prefsNS, prefsClass, "notifications", json.RawMessage(`"none"`))

	resp, snap := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, prefsSubject, snap.Subject)
	assert.Equal(t, prefsNS, snap.ClassNamespace)
	assert.Equal(t, prefsClass, snap.ClassName)
	require.Len(t, snap.Snapshot.Keys, 2, "full schema: the self AND the class key must both be present")

	byName := map[string]preferences.Resolved{}
	for _, k := range snap.Snapshot.Keys {
		byName[k.Name] = k
	}

	// The class-visible key is present (a ?user-ref= read would keep this one).
	classKey, ok := byName["language"]
	require.True(t, ok, "the visibility:class 'language' key must be present")
	assert.Equal(t, "class", classKey.Visibility)

	// The self-only key is present AND carries the seeded user value — the half
	// a ?user-ref= read filters out. Asserting both keys by name is the union
	// proof; a Len(2) alone would pass even if the wrong two keys came back.
	selfKey, ok := byName["notifications"]
	require.True(t, ok, "the self-only 'notifications' key must be present too — full schema, not class-visible-only")
	assert.Equal(t, "self", selfKey.Visibility)
	assert.Equal(t, preferences.SourceUser, selfKey.Source)
	require.NotNil(t, selfKey.Value)
	assert.JSONEq(t, `"none"`, string(selfKey.Value.Raw))
}

func TestPreferencesFirstPartyGet_NonChannelsdPrincipal_403(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyServer(t, class)

	// No token registered for this route other than channelsd; a session
	// token (unregistered here) is simply unauthenticated at all, which
	// proves the same thing at the earlier auth layer, but the webd token
	// IS registrable and reaches componentPrincipalFrom's check — use it to
	// prove the 403 is about identity, not merely about being unauthenticated.
	reg := tokens.NewRegistry()
	reg.SetWebdToken("tok-webd")
	scheme := newPreferencesScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()
	mem := memory.NewLocal(memoryinmem.NewBackend())
	h := httpsrv.NewHandler(mem, reg, httpsrv.WithPreferences(c))
	srv2 := httptest.NewServer(h)
	defer srv2.Close()

	resp, _ := getFirstParty(t, srv2, prefsNS, prefsClass, prefsSubject, "tok-webd")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The main fixture's server has no way to authenticate a non-channelsd
	// caller as anything but 401 — assert that shape too on srv, so a
	// completely unknown bearer is refused earlier, not confused with 403.
	resp2, _ := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, "unknown-token")
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestPreferencesFirstPartyGet_ClassWithoutPreferences_404(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, nil)
	srv, _ := newFirstPartyServer(t, class)

	resp, _ := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesFirstPartyGet_UnknownClass_404(t *testing.T) {
	srv, _ := newFirstPartyServer(t) // no AgentClass seeded at all

	resp, _ := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesFirstPartyGet_MissingOrBadSubject_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyServer(t, class)

	cases := []struct {
		name    string
		subject string
	}{
		{name: "missing ?subject=", subject: ""},
		{name: "subject contains a slash", subject: "not/a/bare/id"},
		{name: "subject contains a colon", subject: "bad:canonical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := getFirstParty(t, srv, prefsNS, prefsClass, tc.subject, prefsChannelsd)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPreferencesFirstPartyGet_NotAudited proves this read appends NEITHER a
// preference_access entry (the session-scoped ?user-ref= audit kind) NOR a
// preference_write entry (the user-scoped write audit kind) — the
// differentiator from every other subject-named preferences route in this
// package, which all audit.
func TestPreferencesFirstPartyGet_NotAudited(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, mem := newFirstPartyServer(t, class)
	putUserPreference(t, mem, prefsSubject, prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, _ := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	sysCtx := memory.WithSystemApproval(context.Background(), "test")

	accessScope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsClass}
	accessEntries, err := preferenceaccess.List(sysCtx, mem, accessScope)
	require.NoError(t, err)
	assert.Empty(t, accessEntries, "a class-addressed self-read must not append a preference_access entry")

	userScope, err := memory.UserScope(prefsSubject)
	require.NoError(t, err)
	writeEntries, err := preferencewrite.List(sysCtx, mem, userScope)
	require.NoError(t, err)
	assert.Empty(t, writeEntries, "a class-addressed self-read must not append a preference_write entry")
}

// firstPartyCommitServerOpts configures newFirstPartyCommitServer's optional
// collaborators, so each test states only what it needs.
type firstPartyCommitServerOpts struct {
	noWriteAudit bool // true: WithPreferenceWriteAudit NOT passed
}

// newFirstPartyCommitServer is newFirstPartyServer's commit-route sibling:
// same channelsd-only token shape, but also wires WithPreferenceWriteAudit by
// default (the SAME *memory.Local as h.mem — valid here because this fixture
// configures no provenance verifier, exactly like
// newPreferencesServerForUserRef does for WithPreferenceAudit) so a test can
// omit it via noWriteAudit to prove the 503 degrade path.
func newFirstPartyCommitServer(t *testing.T, o firstPartyCommitServerOpts, objs ...client.Object) (*httptest.Server, *memory.Local) {
	t.Helper()
	scheme := newPreferencesScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()

	mem := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken(prefsChannelsd)
	reg.SetAuthzdToken(prefsFirstPartyCommitAuthzd)

	opts := []httpsrv.HandlerOption{httpsrv.WithPreferences(c)}
	if !o.noWriteAudit {
		opts = append(opts, httpsrv.WithPreferenceWriteAudit(mem))
	}
	h := httpsrv.NewHandler(mem, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mem
}

// prefsFirstPartyCommitAuthzd is a system-caller token registered on every
// newFirstPartyCommitServer fixture, used ONLY by the non-channelsd-403
// test: authzd reaches the handler's own componentPrincipalFrom check
// (unlike webd, which the outer ServeHTTP auth layer already refuses
// read-only on ANY mutating route, before the handler ever runs), so the
// 403 it produces actually pins the handler's own gate rather than the
// read-only-webd gate one layer up.
const prefsFirstPartyCommitAuthzd = "tok-authzd-firstparty-commit"

// postFirstPartyCommit POSTs a CommitRequest to
// /memory/_preferences_firstparty_commit/{ns}/{className} as token.
func postFirstPartyCommit(t *testing.T, srv *httptest.Server, ns, className, token string, req preferences.CommitRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost,
		srv.URL+"/memory/_preferences_firstparty_commit/"+ns+"/"+className, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

// TestPreferencesFirstPartyCommit_HappyPath_ThenGetShowsSourceUser_AndAudited
// is the core round trip: a channelsd, class-addressed commit lands the
// value AND appends a preference_write audit entry into the subject's own
// user scope, and a follow-up first-party GET resolves it with source
// "user".
func TestPreferencesFirstPartyCommit_HappyPath_ThenGetShowsSourceUser_AndAudited(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, mem := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	getResp, snap := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))

	userScope, err := memory.UserScope(prefsSubject)
	require.NoError(t, err)
	sysCtx := memory.WithSystemApproval(context.Background(), "test")
	entries, err := preferencewrite.List(sysCtx, mem, userScope)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the commit must append exactly one preference_write entry")
	assert.Equal(t, prefsSubject, entries[0].Subject)
	assert.Equal(t, prefsNS, entries[0].ClassNamespace)
	assert.Equal(t, prefsClass, entries[0].ClassName)
	assert.Equal(t, "language", entries[0].Key)
	assert.Equal(t, "app-home", entries[0].Via)
	assert.JSONEq(t, `"de"`, string(entries[0].Value))
}

func TestPreferencesFirstPartyCommit_LockedByValidGlobal_409(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	settings := buildSettings(prefsNS, map[string]map[string]v1alpha1.PreferenceGlobal{
		prefsClass: {"language": {Value: apiextv1.JSON{Raw: []byte(`"de"`)}, Lock: true}},
	})
	srv, _ := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class, settings)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"en"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	body := readBody(t, resp)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, "locked by admin policy")
}

func TestPreferencesFirstPartyCommit_NonChannelsdPrincipal_403(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsFirstPartyCommitAuthzd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPreferencesFirstPartyCommit_UnknownKey_404(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "does-not-exist", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPreferencesFirstPartyCommit_TypeMismatch_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"not-a-permitted-value"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestPreferencesFirstPartyCommit_NullValue_TombstonesBackToDefault proves a
// nil Value clears the user layer: the commit succeeds and a follow-up
// first-party GET resolves the key back to its class default.
func TestPreferencesFirstPartyCommit_NullValue_TombstonesBackToDefault(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, mem := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{}, class)
	putUserPreference(t, mem, prefsSubject, prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: nil, Subject: prefsSubject,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	getResp, snap := getFirstParty(t, srv, prefsNS, prefsClass, prefsSubject, prefsChannelsd)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceDefault, snap.Snapshot.Keys[0].Source)
}

// TestPreferencesFirstPartyCommit_NoWriteAuditWriter_503 proves an unaudited
// first-party write is refused outright, mirroring
// TestPreferencesGetUserRef_NoAuditWriter_503's degrade path for the sibling
// audit option.
func TestPreferencesFirstPartyCommit_NoWriteAuditWriter_503(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{languagePreference("en")})
	srv, _ := newFirstPartyCommitServer(t, firstPartyCommitServerOpts{noWriteAudit: true}, class)

	resp := postFirstPartyCommit(t, srv, prefsNS, prefsClass, prefsChannelsd, preferences.CommitRequest{
		Key: "language", Value: jsonRaw(t, `"de"`), Subject: prefsSubject,
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
