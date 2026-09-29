package main

// The preferences routes (GET /memory/_preferences and POST
// /memory/_preferences_commit) resolve the AgentSession/AgentClass/
// AgentSettings CRs their schema, admin globals, and lock policy come from
// through h.prefsReader — wired only by httpsrv.WithPreferences. Like the
// resource-pool route (pools_wiring_test.go) and the pt-tag mint route
// (pttag_minter_test.go), the risk this guards against is not "does the
// route work" (pkg/memory/httpsrv's own preferences_test.go already proves
// that in detail) but "does run()'s ACTUAL option assembly still call the
// option that turns it on". A handler built by hand-picking
// httpsrv.WithPreferences(c) directly would pass every test in the tree
// while newMemHandlerOpts itself never wired it, and the routes would 404 on
// every real deployment. Going through newMemHandlerOpts — the exact
// function run() calls — closes that gap: deleting the WithPreferences line
// there reddens this test by name.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // register user_preference Kind
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferencewrite"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/userpreference"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

const (
	prefsWiringNS      = "ns-prefs-wiring"
	prefsWiringSession = "sess-prefs-wiring"
	prefsWiringClass   = "reviewbot"
	prefsWiringToken   = "tok-prefs-wiring"
	prefsWiringChanTok = "tok-channelsd-prefs-wiring"
	prefsWiringSubject = "d2lyaW5n"
)

func prefsWiringScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

// TestPreferencesRoutesAreServedWhenWiredTheWayTheOperatorWiresIt is the pin
// for this task's operator-wiring requirement: newMemHandlerOpts must
// actually call httpsrv.WithPreferences, or both preferences routes are
// unreachable in production while every other test — including httpsrv's own
// — stays green.
func TestPreferencesRoutesAreServedWhenWiredTheWayTheOperatorWiresIt(t *testing.T) {
	scheme := prefsWiringScheme(t)
	class := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: prefsWiringNS, Name: prefsWiringClass},
		Spec: v1alpha1.AgentClassSpec{UserPreferences: []v1alpha1.UserPreferenceSchema{
			{Name: "language", Type: "enum", Enum: []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}}},
		}},
	}
	sess := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: prefsWiringNS, Name: prefsWiringSession},
		Spec:       v1alpha1.AgentSessionSpec{Class: prefsWiringClass},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess).Build()

	// ml plays both roles run() uses it for: h.mem (the handler's own facade)
	// and d.MemLocal (EntryDeleter/EntryReindexer) — the same single instance,
	// exactly like main.go's httpsrv.NewHandler(memLocal, memTokens, ...) call.
	ml := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: prefsWiringNS, Name: prefsWiringSession}, prefsWiringToken, "")
	reg.SetChannelsdToken(prefsWiringChanTok)

	opts := newMemHandlerOpts(memHandlerDeps{K8sClient: fakeClient, MemLocal: ml})
	h := httpsrv.NewHandler(ml, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// GET: the session's own token must reach a resolved snapshot, not the
	// handler's "preferences not supported" 405 (the un-wired answer).
	getReq, err := http.NewRequest(http.MethodGet,
		srv.URL+"/memory/_preferences/"+prefsWiringNS+"/"+prefsWiringSession, nil)
	require.NoError(t, err)
	getReq.Header.Set("Authorization", "Bearer "+prefsWiringToken)
	getResp, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err)
	defer getResp.Body.Close()
	assert.Equal(t, http.StatusOK, getResp.StatusCode,
		"GET _preferences must be SERVED (not 405) when built the way run() builds it")

	// POST commit: the channelsd token must actually write, not 405.
	reqBody, err := json.Marshal(preferences.CommitRequest{
		Key:     "language",
		Value:   &apiextv1.JSON{Raw: []byte(`"de"`)},
		Subject: prefsWiringSubject,
	})
	require.NoError(t, err)
	commitReq, err := http.NewRequest(http.MethodPost,
		srv.URL+"/memory/_preferences_commit/"+prefsWiringNS+"/"+prefsWiringSession, bytes.NewReader(reqBody))
	require.NoError(t, err)
	commitReq.Header.Set("Authorization", "Bearer "+prefsWiringChanTok)
	commitReq.Header.Set("Content-Type", "application/json")
	commitResp, err := http.DefaultClient.Do(commitReq)
	require.NoError(t, err)
	defer commitResp.Body.Close()
	assert.Equal(t, http.StatusNoContent, commitResp.StatusCode,
		"POST _preferences_commit must be SERVED (not 405) when built the way run() builds it")
}

// TestPreferencesUserRefRouteIsServedWhenWiredTheWayTheOperatorWiresIt is the
// pin for Slice 2's operator-wiring requirement: newMemHandlerOpts must call
// BOTH httpsrv.WithSubjectResolution and httpsrv.WithPreferenceAudit, or
// ?user-ref= answers 503 ("audit writer not configured") on every real
// deployment even though pkg/memory/httpsrv's own tests prove the route
// itself works. d.SpiceDBClient is deliberately left nil here (an
// email-form reference needs no SpiceDB lookup at all — see
// subjectresolve/email.go) so this test does not need a live SpiceDB
// connection to prove the wiring; d.OpSigned is a real signing facade
// because WithPreferenceAudit's whole point is refusing a nil one.
func TestPreferencesUserRefRouteIsServedWhenWiredTheWayTheOperatorWiresIt(t *testing.T) {
	scheme := prefsWiringScheme(t)
	class := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: prefsWiringNS, Name: prefsWiringClass},
		Spec: v1alpha1.AgentClassSpec{UserPreferences: []v1alpha1.UserPreferenceSchema{
			{Name: "language", Type: "enum",
				Enum:       []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}},
				Visibility: "class"},
		}},
	}
	sess := &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: prefsWiringNS, Name: prefsWiringSession},
		Spec:       v1alpha1.AgentSessionSpec{Class: prefsWiringClass},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, sess).Build()

	ml := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: prefsWiringNS, Name: prefsWiringSession}, prefsWiringToken, "")

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	opSigned := provenance.NewSigningMemory(ml, provenance.NewSigner(priv, "system:operator"))

	// A saved value makes the referenced subject a KNOWN platform user: an
	// email-form reference for a subject with no user_preference record at
	// all is refused (422 unknown-subject — httpsrv's own tests pin that),
	// and this test's claim is about run()'s wiring, not about resolution
	// semantics, so it reads a user the resolution has no reason to refuse.
	canon, err := identity.EmailReference(identity.Email("alice@example.com")).Canonical()
	require.NoError(t, err)
	aliceScope, err := memory.UserScope(canon.String())
	require.NoError(t, err)
	prefContent, err := json.Marshal(userpreference.Preference{
		ClassNamespace: prefsWiringNS, ClassName: prefsWiringClass,
		Key: "language", Value: json.RawMessage(`"de"`),
	})
	require.NoError(t, err)
	_, err = ml.Put(memory.SystemContext(context.Background(), "test"), memory.Entry{
		Scope:   aliceScope,
		Kind:    userpreference.KindName,
		ID:      userpreference.EntryID(prefsWiringNS, prefsWiringClass, "language"),
		Content: prefContent,
	})
	require.NoError(t, err)

	opts := newMemHandlerOpts(memHandlerDeps{K8sClient: fakeClient, MemLocal: ml, OpSigned: opSigned})
	h := httpsrv.NewHandler(ml, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	getReq, err := http.NewRequest(http.MethodGet,
		srv.URL+"/memory/_preferences/"+prefsWiringNS+"/"+prefsWiringSession+"?user-ref=email:alice@example.com", nil)
	require.NoError(t, err)
	getReq.Header.Set("Authorization", "Bearer "+prefsWiringToken)
	getResp, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err)
	defer getResp.Body.Close()
	assert.Equal(t, http.StatusOK, getResp.StatusCode,
		"?user-ref= must be SERVED (not refused) when built the way run() builds it")

	var snap preferences.SnapshotResponse
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&snap))
	assert.NotEmpty(t, snap.Subject, "an email-form reference must resolve even with no SpiceDB client wired")

	// THE SIGNED-WRITER PIN. What must never change silently is WHICH value
	// newMemHandlerOpts hands WithPreferenceAudit: d.OpSigned (the operator's
	// signing facade), not d.MemLocal (the unwrapped store). The option is an
	// opaque closure, so the value itself cannot be compared for identity
	// from here without a test-only seam in httpsrv; the observable that
	// distinguishes the two exactly is the stored entry's provenance — only
	// a write through the signing facade carries an Ed25519 envelope with
	// Publisher "system:operator", while a write through the unwrapped
	// MemLocal (this fixture configures no verifier, mirroring the pre-crash
	// window) stores with Provenance nil. Rewiring main.go to d.MemLocal
	// turns exactly this assertion red.
	sysCtx := memory.SystemContext(context.Background(), "test")
	qres, err := ml.Query(sysCtx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: prefsWiringNS + "/" + prefsWiringSession},
		Kinds: []string{preferenceaccess.KindName},
	})
	require.NoError(t, err)
	require.Len(t, qres.Entries, 1, "the ?user-ref= read must have appended its audit entry")
	prov := qres.Entries[0].Provenance
	require.NotNil(t, prov,
		"the audit entry must be SIGNED — nil provenance means the write bypassed d.OpSigned (rewired to d.MemLocal?)")
	assert.Equal(t, "system:operator", prov.Publisher,
		"the audit trail's publisher must be the operator's own signing identity")
	assert.NotEmpty(t, prov.Sig)
}

// TestPreferencesFirstPartyCommitRouteIsServedWhenWiredTheWayTheOperatorWiresIt
// is Task 7's operator-wiring pin: newMemHandlerOpts must call
// httpsrv.WithPreferenceWriteAudit(d.OpSigned) — NOT d.MemLocal — or the
// class-addressed first-party commit route either 503s on every real
// deployment (unwired entirely) or, worse, appends an UNSIGNED
// preference_write entry that provenance verification would otherwise have
// caught. d.SpiceDBClient is left nil, mirroring the sibling test above:
// this route needs no SpiceDB lookup at all.
func TestPreferencesFirstPartyCommitRouteIsServedWhenWiredTheWayTheOperatorWiresIt(t *testing.T) {
	scheme := prefsWiringScheme(t)
	class := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: prefsWiringNS, Name: prefsWiringClass},
		Spec: v1alpha1.AgentClassSpec{UserPreferences: []v1alpha1.UserPreferenceSchema{
			{Name: "language", Type: "enum", Enum: []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}}},
		}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()

	ml := memory.NewLocal(memoryinmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken(prefsWiringChanTok)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	opSigned := provenance.NewSigningMemory(ml, provenance.NewSigner(priv, "system:operator"))

	opts := newMemHandlerOpts(memHandlerDeps{K8sClient: fakeClient, MemLocal: ml, OpSigned: opSigned})
	h := httpsrv.NewHandler(ml, reg, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	reqBody, err := json.Marshal(preferences.CommitRequest{
		Key:     "language",
		Value:   &apiextv1.JSON{Raw: []byte(`"de"`)},
		Subject: prefsWiringSubject,
	})
	require.NoError(t, err)
	commitReq, err := http.NewRequest(http.MethodPost,
		srv.URL+"/memory/_preferences_firstparty_commit/"+prefsWiringNS+"/"+prefsWiringClass, bytes.NewReader(reqBody))
	require.NoError(t, err)
	commitReq.Header.Set("Authorization", "Bearer "+prefsWiringChanTok)
	commitReq.Header.Set("Content-Type", "application/json")
	commitResp, err := http.DefaultClient.Do(commitReq)
	require.NoError(t, err)
	defer commitResp.Body.Close()
	assert.Equal(t, http.StatusNoContent, commitResp.StatusCode,
		"POST _preferences_firstparty_commit must be SERVED (not 503/405) when built the way run() builds it")

	// THE SIGNED-WRITER PIN — same reasoning as the ?user-ref= test above,
	// applied to preference_write instead of preference_access: only a write
	// through d.OpSigned carries an Ed25519 envelope with Publisher
	// "system:operator". Rewiring main.go's WithPreferenceWriteAudit call to
	// d.MemLocal turns exactly this assertion red.
	userScope, err := memory.UserScope(prefsWiringSubject)
	require.NoError(t, err)
	sysCtx := memory.SystemContext(context.Background(), "test")
	qres, err := ml.Query(sysCtx, memory.Query{
		Scope: userScope,
		Kinds: []string{preferencewrite.KindName},
	})
	require.NoError(t, err)
	require.Len(t, qres.Entries, 1, "the first-party commit must have appended its preference_write audit entry")
	prov := qres.Entries[0].Provenance
	require.NotNil(t, prov,
		"the preference_write entry must be SIGNED — nil provenance means the write bypassed d.OpSigned (rewired to d.MemLocal?)")
	assert.Equal(t, "system:operator", prov.Publisher,
		"the preference_write trail's publisher must be the operator's own signing identity")
	assert.NotEmpty(t, prov.Sig)
}
