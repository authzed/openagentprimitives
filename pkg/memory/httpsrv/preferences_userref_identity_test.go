package httpsrv_test

import (
	"encoding/json"
	"io"
	"net/http"
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
			sess := buildSession(prefsNS, prefsSession, prefsClass)
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
