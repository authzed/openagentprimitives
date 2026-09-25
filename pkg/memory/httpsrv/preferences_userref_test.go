package httpsrv_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/subjectresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// fakeUserRelations is a test double for subjectresolve.RelationReader, keyed
// "type:id:relation" — mirroring subjectresolve's own internal test fake,
// which this package cannot import (it is unexported there).
type fakeUserRelations struct {
	subjects map[string][]string
	err      error
}

func (f *fakeUserRelations) UserSubjects(_ context.Context, objType, objID, relation string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.subjects == nil {
		return nil, nil
	}
	return f.subjects[objType+":"+objID+":"+relation], nil
}

// userRefServerOpts configures newPreferencesServerForUserRef's optional
// collaborators, so each test states only what it needs.
type userRefServerOpts struct {
	relations subjectresolve.RelationReader // nil: WithSubjectResolution NOT passed
	noAudit   bool                          // true: WithPreferenceAudit NOT passed
}

// newPreferencesServerForUserRef is newPreferencesServer's ?user-ref= sibling:
// same token/registry shape, but builds its HandlerOption list explicitly so
// a test can omit WithSubjectResolution or WithPreferenceAudit to prove the
// degrade paths those options' own docs describe. The audit writer, when
// enabled, is the SAME *memory.Local as h.mem — valid here because this
// fixture configures no provenance verifier (see facade.Put), exactly like
// every other test in this package that writes append-only entries directly.
func newPreferencesServerForUserRef(t *testing.T, o userRefServerOpts, objs ...client.Object) (*httptest.Server, *memory.Local) {
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

	opts := []httpsrv.HandlerOption{httpsrv.WithPreferences(c)}
	if o.relations != nil {
		opts = append(opts, httpsrv.WithSubjectResolution(o.relations))
	}
	if !o.noAudit {
		opts = append(opts, httpsrv.WithPreferenceAudit(mem))
	}
	h := httpsrv.NewHandler(mem, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, mem
}

// classVisiblePreference is a "language" schema opted into visibility:
// class, mirroring languagePreference's shape.
func classVisiblePreference(def string) v1alpha1.UserPreferenceSchema {
	raw, _ := json.Marshal(def)
	return v1alpha1.UserPreferenceSchema{
		Name:       "language",
		Type:       "enum",
		Enum:       []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}},
		Default:    &apiextv1.JSON{Raw: raw},
		Visibility: "class",
	}
}

// selfOnlyStringPreference is a self-only (visibility unset) schema, used to
// prove a key that is NOT opted into class-visibility never reaches a
// ?user-ref= response even when a value is saved for the resolved subject.
func selfOnlyStringPreference(name, def string) v1alpha1.UserPreferenceSchema {
	raw, _ := json.Marshal(def)
	return v1alpha1.UserPreferenceSchema{
		Name:    name,
		Type:    "string",
		Default: &apiextv1.JSON{Raw: raw},
	}
}

func mustCanonicalEmail(t *testing.T, addr string) string {
	t.Helper()
	canon, err := identity.EmailReference(identity.Email(addr)).Canonical()
	require.NoError(t, err)
	return canon.String()
}

// getPreferencesForUserRef GETs ?user-ref=ref against srv as token.
func getPreferencesForUserRef(t *testing.T, srv *httptest.Server, ns, name, ref, token string) (*http.Response, preferences.SnapshotResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/memory/_preferences/"+ns+"/"+name+"?user-ref="+url.QueryEscape(ref), nil)
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

// TestPreferencesGetUserRef_EmailForm_Happy proves the core disclosure
// contract: a visibility:class key's saved value is returned (subject
// echoed, source "user"), while a visibility:self key with a seeded value
// for the SAME resolved subject is entirely ABSENT from the response — not
// merely empty-valued, absent as a Snapshot.Keys entry.
func TestPreferencesGetUserRef_EmailForm_Happy(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{
		classVisiblePreference("en"),
		selfOnlyStringPreference("secret-note", "none"),
	})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess)

	subject := mustCanonicalEmail(t, "alice@example.com")
	putUserPreference(t, mem, subject, prefsNS, prefsClass, "language", json.RawMessage(`"de"`))
	putUserPreference(t, mem, subject, prefsNS, prefsClass, "secret-note", json.RawMessage(`"do not disclose"`))

	resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, subject, snap.Subject)
	assert.Empty(t, snap.Note)
	require.Len(t, snap.Snapshot.Keys, 1, "the self-only key must be ABSENT, not merely unset")
	assert.Equal(t, "language", snap.Snapshot.Keys[0].Name)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))
	for _, k := range snap.Snapshot.Keys {
		assert.NotEqual(t, "secret-note", k.Name, "a visibility:self key must never appear in a user-ref response")
	}
}

// TestPreferencesGetUserRef_ResourceForm_Resolved proves a "<type>:<id>"
// reference resolves via the injected RelationReader's sole_user relation.
func TestPreferencesGetUserRef_ResourceForm_Resolved(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	rel := &fakeUserRelations{subjects: map[string][]string{
		"github_user:12345:sole_user": {"canon-abc"},
	}}
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: rel}, class, sess)
	putUserPreference(t, mem, "canon-abc", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "github_user:12345", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "canon-abc", snap.Subject)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
}

// TestPreferencesGetUserRef_Unresolved covers two shapes of "did not
// resolve": an unknown reference scheme, and a resource with no sole_user. A
// ref that resolves to NO platform user is an ERROR, never a 200 carrying
// class DEFAULTS: a default snapshot is byte-for-byte indistinguishable from
// "this user saved nothing", so returning it silently would let an agent act
// on the wrong policy for a user it could not identify — the exact failure a
// webhook-triggered reviewbot hit (pinging on the default when the author had
// set `always`). The attempt is still audited (OutcomeUnresolved); only the
// client-visible answer becomes a hard error carrying the reason.
func TestPreferencesGetUserRef_Unresolved(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		rel  subjectresolve.RelationReader
	}{
		{name: "unknown scheme", ref: "totally-unsupported-reference-form", rel: &fakeUserRelations{}},
		{name: "resource with no sole_user", ref: "github_user:99999", rel: &fakeUserRelations{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
			sess := buildSession(prefsNS, prefsSession, prefsClass)
			srv, _ := newPreferencesServerForUserRef(t, userRefServerOpts{relations: tc.rel}, class, sess)

			resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, tc.ref, prefsToken)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
				"an unresolvable user-ref must be an error, not a 200 carrying class defaults")
			body, _ := io.ReadAll(resp.Body)
			assert.Contains(t, strings.ToLower(string(body)), "resolve",
				"the error body must explain the ref could not be resolved to a platform user")
		})
	}
}

// TestPreferencesGetUserRef_NilRelations_ResourceRef_UnresolvedNotAPanic
// proves the WithSubjectResolution-not-passed degrade path: a resource-shaped
// reference on a cluster with no RelationReader errors cleanly — never a
// panic — carrying the fixed "not available" reason (distinct from "this
// resource has no linked user"), never a 200 answering class defaults.
func TestPreferencesGetUserRef_NilRelations_ResourceRef_UnresolvedNotAPanic(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServerForUserRef(t, userRefServerOpts{ /* relations omitted */ }, class, sess)

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "github_user:12345", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "unresolved is an error, not a panic and not a defaulted 200")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "subject resolution is not available on this cluster",
		"the fixed not-available reason is preserved in the error, distinct from a no-linked-user reason")
}

// TestPreferencesGetUserRef_EmailStillWorksWithNilRelations proves the
// email-form resolver needs no RelationReader at all, so the SAME handler
// that cannot resolve a resource reference (previous test) still resolves
// an email one.
func TestPreferencesGetUserRef_EmailStillWorksWithNilRelations(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, _ := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess)

	resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mustCanonicalEmail(t, "alice@example.com"), snap.Subject)
}

// TestPreferencesGet_TurnAndUserRef_MutuallyExclusive_400 proves the two
// query parameters refuse together rather than one silently winning.
func TestPreferencesGet_TurnAndUserRef_MutuallyExclusive_400(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/memory/_preferences/"+prefsNS+"/"+prefsSession+"?turn=0&user-ref=email:alice@example.com", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+prefsToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestPreferencesGetUserRef_NoAuditWriter_503 proves an un-audited
// disclosure is refused outright rather than served silently, while the
// SAME handler's turn-derived route is entirely unaffected.
func TestPreferencesGetUserRef_NoAuditWriter_503(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{noAudit: true}, class, sess)
	seedUserTurn(t, mem, 0, "user:YWxpY2U")

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// The turn-derived route on the exact same handler is unaffected.
	turnResp, turnSnap := getPreferences(t, srv, "/memory/_preferences/"+prefsNS+"/"+prefsSession, prefsToken)
	defer turnResp.Body.Close()
	require.Equal(t, http.StatusOK, turnResp.StatusCode)
	require.Len(t, turnSnap.Snapshot.Keys, 1)
}

// TestPreferencesGetUserRef_AuditTrail proves every ?user-ref= read — here,
// two back-to-back identical ones — appends its own preference_access entry
// to the SESSION scope (not the resolved subject's user scope), carrying
// {ref, resolvedSubject, keys}. Append-only: two identical reads append two
// entries, by design — see preferenceaccess.Record's doc — because each is
// a distinct FACT ("this agent looked this reference up at this time"), not
// a value that converges to one row.
func TestPreferencesGetUserRef_AuditTrail(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{
		classVisiblePreference("en"),
		selfOnlyStringPreference("secret-note", "none"),
	})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess)

	resp1, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	resp2, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	scope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsSession}
	entries, err := preferenceaccess.List(memory.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	require.Len(t, entries, 2, "append-only: two reads append two entries, even byte-identical ones")
	for _, e := range entries {
		assert.Equal(t, "email:alice@example.com", e.Ref)
		assert.Equal(t, mustCanonicalEmail(t, "alice@example.com"), e.ResolvedSubject)
		assert.Equal(t, preferenceaccess.OutcomeOK, e.Outcome)
		assert.Equal(t, []string{"language"}, e.Keys, "self-only keys must never appear in the audited key list either")
	}
}

// TestPreferencesGetUserRef_UnresolvedStillAudits proves the audit
// obligation applies to an unresolved read too, not only a successful
// disclosure — an agent's ATTEMPT to look someone up is itself the fact
// worth recording.
func TestPreferencesGetUserRef_UnresolvedStillAudits(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: &fakeUserRelations{}}, class, sess)

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "github_user:99999", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"the unresolved read errors — but the audit obligation still applies, which is what this test proves")

	scope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsSession}
	entries, err := preferenceaccess.List(memory.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "github_user:99999", entries[0].Ref)
	assert.Empty(t, entries[0].ResolvedSubject)
	assert.Equal(t, preferenceaccess.OutcomeUnresolved, entries[0].Outcome)
	assert.NotEmpty(t, entries[0].Reason)
	assert.Equal(t, []string{"language"}, entries[0].Keys,
		"the record still names the class-visible keys the attempt considered, even though it resolved no user and returned no snapshot")
}

// listAudit reads the session's preference_access trail under a system
// approval — the shared tail of every fault-path assertion below.
func listAudit(t *testing.T, mem *memory.Local) []preferenceaccess.Content {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: prefsNS + "/" + prefsSession}
	entries, err := preferenceaccess.List(memory.WithSystemApproval(context.Background(), "test"), mem, scope)
	require.NoError(t, err)
	return entries
}

// TestPreferencesGetUserRef_ResolverError_502_Audited proves a resolver
// HARD FAULT (a RelationReader error — not an unresolved outcome) answers
// 502 AND still appends an audit entry: httpclient retries a 5xx up to
// eight times, so an unaudited fault branch would be eight invisible
// subject-named attempts per caller fault.
func TestPreferencesGetUserRef_ResolverError_502_Audited(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	rel := &fakeUserRelations{err: errors.New("spicedb: connection refused")}
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: rel}, class, sess)

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "github_user:12345", prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	entries := listAudit(t, mem)
	require.Len(t, entries, 1, "a resolver fault is still a subject-named read attempt and must be audited")
	assert.Equal(t, "github_user:12345", entries[0].Ref)
	assert.Equal(t, preferenceaccess.OutcomeResolverError, entries[0].Outcome)
	assert.Empty(t, entries[0].ResolvedSubject)
	assert.Contains(t, entries[0].Reason, "connection refused")
	assert.Empty(t, entries[0].Keys, "a faulted attempt disclosed nothing; Keys stays empty")
}

// TestPreferencesGetUserRef_UserScopeError_500_Audited proves the
// post-resolution fault branch audits too, carrying the subject that WAS
// resolved by then: the fake reader hands back a canonical id containing a
// reserved separator, which memory.UserScope refuses.
func TestPreferencesGetUserRef_UserScopeError_500_Audited(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	rel := &fakeUserRelations{subjects: map[string][]string{
		"github_user:12345:sole_user": {"bad/canonical"},
	}}
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: rel}, class, sess)

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "github_user:12345", prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	entries := listAudit(t, mem)
	require.Len(t, entries, 1)
	assert.Equal(t, preferenceaccess.OutcomeUserScopeError, entries[0].Outcome)
	assert.Equal(t, "bad/canonical", entries[0].ResolvedSubject,
		"the subject WAS resolved before the fault; whom the attempt was about belongs in the record")
	assert.NotEmpty(t, entries[0].Reason)
	assert.Empty(t, entries[0].Keys)
}

// TestPreferencesGetUserRef_TriggerAuthor_Resolved is the end-to-end pin for
// the trigger-author reference THROUGH THE HANDLER: the session CR carries
// the real AnnotationTriggerOwnerSubject value channelsd would have written
// ("<type>:<id>#<relation>"), and resolution strips the relation half and
// recurses into the resource path against the injected RelationReader.
func TestPreferencesGetUserRef_TriggerAuthor_Resolved(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	sess.Annotations = map[string]string{
		v1alpha1.AnnotationTriggerOwnerSubject: "github_user:99#user",
	}
	rel := &fakeUserRelations{subjects: map[string][]string{
		"github_user:99:sole_user": {"canon-trig"},
	}}
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: rel}, class, sess)
	putUserPreference(t, mem, "canon-trig", prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

	resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "trigger-author", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "canon-trig", snap.Subject)
	assert.Empty(t, snap.Note)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceUser, snap.Snapshot.Keys[0].Source)
	require.NotNil(t, snap.Snapshot.Keys[0].Value)
	assert.JSONEq(t, `"de"`, string(snap.Snapshot.Keys[0].Value.Raw))

	entries := listAudit(t, mem)
	require.Len(t, entries, 1)
	assert.Equal(t, "trigger-author", entries[0].Ref)
	assert.Equal(t, preferenceaccess.OutcomeOK, entries[0].Outcome)
	assert.Equal(t, "canon-trig", entries[0].ResolvedSubject)
}

// TestPreferencesGetUserRef_TriggerAuthor_NoAnnotation_Unresolved proves a
// session with no recorded trigger owner is an ERROR carrying the resolver's
// own fixed reason — NOT a 200 answering class defaults (which would look
// exactly like "this author saved nothing"). The attempt is still audited
// (OutcomeUnresolved) with that reason, exactly as before.
func TestPreferencesGetUserRef_TriggerAuthor_NoAnnotation_Unresolved(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{relations: &fakeUserRelations{}}, class, sess)

	resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "trigger-author", prefsToken)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"an unresolvable trigger-author must error, not answer class defaults")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "this session was not opened by a trigger with a recorded owner",
		"the error body surfaces the resolver's reason for the caller to act on")

	entries := listAudit(t, mem)
	require.Len(t, entries, 1)
	assert.Equal(t, "trigger-author", entries[0].Ref)
	assert.Equal(t, preferenceaccess.OutcomeUnresolved, entries[0].Outcome)
	assert.Equal(t, "this session was not opened by a trigger with a recorded owner", entries[0].Reason)
}
