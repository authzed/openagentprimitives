package httpsrv_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/preferenceaccess"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// userIdentityFor builds the UserIdentity channelsd would record for addr,
// with a Slack channel identity carrying slackDisplay as the display name and
// realName as the full name.
func userIdentityFor(t *testing.T, addr, slackDisplay, realName string) *v1alpha1.UserIdentity {
	t.Helper()
	subject := identity.Subject("user:" + mustCanonicalEmail(t, addr))
	return &v1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(subject)},
		Spec:       v1alpha1.UserIdentitySpec{Subject: subject.String(), DisplayName: slackDisplay},
		Status: v1alpha1.UserIdentityStatus{ChannelIdentities: []v1alpha1.ChannelIdentity{{
			Kind: "slack", Domain: "T1", ExternalID: "U-" + addr,
			DisplayName: slackDisplay, RealName: realName, Email: addr,
		}}},
	}
}

// guestIdentityNamed builds the UserIdentity channelsd records for a Slack
// guest: a team-scoped synthetic subject and a channel identity with the
// self-set realName but no vouched email.
func guestIdentityNamed(userID, realName string) *v1alpha1.UserIdentity {
	subject := identity.Subject("user:synthetic-T1-" + userID)
	return &v1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(subject)},
		Spec:       v1alpha1.UserIdentitySpec{Subject: subject.String(), DisplayName: realName},
		Status: v1alpha1.UserIdentityStatus{ChannelIdentities: []v1alpha1.ChannelIdentity{{
			Kind: "slack", Domain: "T1", ExternalID: userID, DisplayName: realName, RealName: realName,
		}}},
	}
}

// trustedTriggerSession builds the session a trigger opened for an owner the
// provider vouched for (a pull request by a repository member): the only kind
// of session a "name:" reference is honored in.
func trustedTriggerSession() *v1alpha1.AgentSession {
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	sess.Annotations = map[string]string{
		v1alpha1.AnnotationTriggerOwnerSubject: "github_user:42#user",
		v1alpha1.AnnotationTriggerOwnerTrusted: v1alpha1.AnnotationTriggerOwnerTrustedTrue,
	}
	return sess
}

// TestPreferencesGetUserRef_EmailKnownViaUserIdentity_ResolvesToDefaults pins
// the other half of the existence bar: a subject with a UserIdentity is a real
// platform user even when they never saved a preference, so the read answers
// the class defaults instead of refusing — the case that silenced every
// reviewbot mention for authors who had simply never opened the settings.
func TestPreferencesGetUserRef_EmailKnownViaUserIdentity_ResolvesToDefaults(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := buildSession(prefsNS, prefsSession, prefsClass)
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess,
		userIdentityFor(t, "alice@example.com", "alice", "Alice Moreno"))

	resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mustCanonicalEmail(t, "alice@example.com"), snap.Subject)
	require.Len(t, snap.Snapshot.Keys, 1)
	assert.Equal(t, preferences.SourceDefault, snap.Snapshot.Keys[0].Source)

	entries := listAudit(t, mem)
	require.Len(t, entries, 1)
	assert.Equal(t, preferenceaccess.OutcomeOK, entries[0].Outcome)
}

// TestPreferencesGetUserRef_EmailKnownOnlyWithoutVouchedChannelIdentity_Refused
// pins the narrowed existence bar: a UserIdentity that no channel vouched an
// email on — one an identity link created under whatever address the person
// signed in with — is not evidence that this address holds their preferences,
// so it gets the unknown-subject refusal, which lets the caller fall back.
func TestPreferencesGetUserRef_EmailKnownOnlyWithoutVouchedChannelIdentity_Refused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.UserIdentity)
	}{
		{name: "no channel identities: 422, unknown subject", mutate: func(ui *v1alpha1.UserIdentity) {
			ui.Status.ChannelIdentities = nil
		}},
		{name: "channel identity without a vouched email: 422, unknown subject", mutate: func(ui *v1alpha1.UserIdentity) {
			ui.Status.ChannelIdentities[0].Email = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui := userIdentityFor(t, "alice@example.com", "alice", "Alice Moreno")
			tc.mutate(ui)
			class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
			sess := buildSession(prefsNS, prefsSession, prefsClass)
			srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess, ui)

			resp, _ := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, "email:alice@example.com", prefsToken)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

			entries := listAudit(t, mem)
			require.Len(t, entries, 1)
			assert.Equal(t, preferenceaccess.OutcomeUnknownSubject, entries[0].Outcome)
		})
	}
}

// getUserRefRaw reads the user-ref endpoint without decoding, so a test can
// assert on everything the response carries.
func getUserRefRaw(t *testing.T, srvURL, ref string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		srvURL+"/memory/_preferences/"+prefsNS+"/"+prefsSession+"?user-ref="+url.QueryEscape(ref), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+prefsToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestPreferencesGetUserRef_NameRefDisclosesNoIdentity pins that a caller who
// knew only a name learns the snapshot and nothing else: the 200 carries
// neither the matched person's email nor their email-derived subject (the
// audit entry still records the subject), and "nobody" and "several" refuse
// with the same words.
func TestPreferencesGetUserRef_NameRefDisclosesNoIdentity(t *testing.T) {
	class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
	sess := trustedTriggerSession()
	srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess,
		userIdentityFor(t, "alice@example.com", "amoreno", "Alice Moreno"),
		userIdentityFor(t, "jo@example.com", "jo", "Jo Park"),
		userIdentityFor(t, "jpark@example.com", "jpark", "Jo Park"),
	)
	putUserPreference(t, mem, mustCanonicalEmail(t, "alice@example.com"), prefsNS, prefsClass, "language", json.RawMessage(`"de"`))
	aliceSubject := mustCanonicalEmail(t, "alice@example.com")

	status, body := getUserRefRaw(t, srv.URL, "name:Alice Moreno")
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body, "alice@example.com")
	assert.NotContains(t, body, aliceSubject)
	assert.Contains(t, body, `"de"`, "the snapshot itself is still returned")

	entries := listAudit(t, mem)
	require.Len(t, entries, 1)
	assert.Equal(t, aliceSubject, entries[0].ResolvedSubject, "the audit keeps the full detail")

	_, several := getUserRefRaw(t, srv.URL, "name:Jo Park")
	_, nobody := getUserRefRaw(t, srv.URL, "name:Robin Hale")
	assert.Equal(t, strings.ReplaceAll(nobody, "Robin Hale", "Jo Park"), several)
}

// TestPreferencesGetUserRef_NameRef covers a "name:" reference end to end: it
// reaches the value a person saved under their work address even though the
// caller only knew their name, and refuses when the name is shared.
func TestPreferencesGetUserRef_NameRef(t *testing.T) {
	cases := []struct {
		name        string
		ref         string
		wantStatus  int
		wantValue   string
		wantOutcome string
	}{
		{name: "unique full name reaches the saved value", ref: "name:Alice Moreno", wantStatus: http.StatusOK, wantValue: `"de"`, wantOutcome: preferenceaccess.OutcomeOK},
		{name: "name shared by two users: 422, unresolved", ref: "name:Jo Park", wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved},
		{name: "unknown name: 422, unresolved", ref: "name:Robin Hale", wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved},
		{name: "name only an unvouched guest set on themselves: 422, unresolved", ref: "name:Dana Whitfield", wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
			sess := trustedTriggerSession()
			srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess,
				userIdentityFor(t, "alice@example.com", "amoreno", "Alice Moreno"),
				userIdentityFor(t, "jo@example.com", "jo", "Jo Park"),
				userIdentityFor(t, "jpark@example.com", "jpark", "Jo Park"),
				guestIdentityNamed("UGUEST", "Dana Whitfield"),
			)
			putUserPreference(t, mem, mustCanonicalEmail(t, "alice@example.com"), prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

			resp, snap := getPreferencesForUserRef(t, srv, prefsNS, prefsSession, tc.ref, prefsToken)
			defer resp.Body.Close()
			require.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantValue != "" {
				require.Len(t, snap.Snapshot.Keys, 1)
				assert.JSONEq(t, tc.wantValue, string(snap.Snapshot.Keys[0].Value.Raw))
			} else {
				body, _ := io.ReadAll(resp.Body)
				assert.NotEmpty(t, body, "a refusal must say why")
			}

			entries := listAudit(t, mem)
			require.Len(t, entries, 1)
			assert.Equal(t, tc.ref, entries[0].Ref)
			assert.Equal(t, tc.wantOutcome, entries[0].Outcome)
		})
	}
}

// TestPreferencesGetUserRef_NameRefGatedOnTriggerOwnerTrust pins the session
// wiring of the trust gate end to end: a name reference reads the matched
// person's value only in a session a trigger opened for a trusted owner. It is
// refused (audited unresolved, value not disclosed) for an outside
// contributor's pull request, and equally in a session a person started in
// chat — a requested review — where nothing verified says whose name it is.
func TestPreferencesGetUserRef_NameRefGatedOnTriggerOwnerTrust(t *testing.T) {
	const owner = "github_user:42#user"
	cases := []struct {
		name        string
		annotations map[string]string
		wantStatus  int
		wantOutcome string
		wantReason  string
	}{
		{name: "trusted author: 200, value read", annotations: map[string]string{
			v1alpha1.AnnotationTriggerOwnerSubject: owner,
			v1alpha1.AnnotationTriggerOwnerTrusted: v1alpha1.AnnotationTriggerOwnerTrustedTrue,
		}, wantStatus: http.StatusOK, wantOutcome: preferenceaccess.OutcomeOK},
		{name: "outside contributor: 422, unresolved", annotations: map[string]string{
			v1alpha1.AnnotationTriggerOwnerSubject: owner,
			v1alpha1.AnnotationTriggerOwnerTrusted: v1alpha1.AnnotationTriggerOwnerTrustedFalse,
		}, wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved,
			wantReason: "whose trigger owner the provider does not vouch for"},
		{name: "session a person started, no trigger owner: 422, unresolved", annotations: nil,
			wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved,
			wantReason: "this session has no such owner"},
		{name: "trust bit with no trigger owner: 422, unresolved", annotations: map[string]string{
			v1alpha1.AnnotationTriggerOwnerTrusted: v1alpha1.AnnotationTriggerOwnerTrustedTrue,
		}, wantStatus: http.StatusUnprocessableEntity, wantOutcome: preferenceaccess.OutcomeUnresolved,
			wantReason: "this session has no such owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := buildClass(prefsNS, prefsClass, []v1alpha1.UserPreferenceSchema{classVisiblePreference("en")})
			sess := buildSession(prefsNS, prefsSession, prefsClass)
			sess.Annotations = tc.annotations
			srv, mem := newPreferencesServerForUserRef(t, userRefServerOpts{}, class, sess,
				userIdentityFor(t, "alice@example.com", "amoreno", "Alice Moreno"))
			putUserPreference(t, mem, mustCanonicalEmail(t, "alice@example.com"), prefsNS, prefsClass, "language", json.RawMessage(`"de"`))

			status, body := getUserRefRaw(t, srv.URL, "name:Alice Moreno")
			assert.Equal(t, tc.wantStatus, status, body)
			if tc.wantStatus != http.StatusOK {
				assert.Contains(t, body, tc.wantReason)
				assert.NotContains(t, body, `"de"`)
			}

			entries := listAudit(t, mem)
			require.Len(t, entries, 1)
			assert.Equal(t, tc.wantOutcome, entries[0].Outcome)
			if tc.wantStatus != http.StatusOK {
				assert.Empty(t, entries[0].ResolvedSubject, "a refused name is never matched against the directory")
			}
		})
	}
}
