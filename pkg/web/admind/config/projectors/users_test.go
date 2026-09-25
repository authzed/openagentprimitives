package projectors

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

func TestUsersProjector(t *testing.T) {
	withDisplay := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "hashabc"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:YWxpY2U=",
			DisplayName: "alice@example.com",
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "gh", Type: "static"}},
		},
		Status: spiceboxv1alpha1.UserIdentityStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.UserIdentityConditionValid, metav1.ConditionTrue, "AllReferencesResolve")},
		},
	}
	noDisplay := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "hashdef"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:Ym9i"},
	}

	c := newClient(t, withDisplay, noDisplay)
	rows, err := usersProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// Name is the human display. No ManageCmd: UserIdentity is managed by the
	// identity setup flow, not kubectl edit.
	a := rowByName(t, rows, "alice@example.com", "")
	assert.Equal(t, "cluster", a.Scope)
	assert.Equal(t, "Valid", a.Status)
	assert.Equal(t, 1, countVal(a, "availableCredentials"))
	assert.Equal(t, "user:YWxpY2U=", badgeVal(a, "subject"))
	assert.Empty(t, a.ManageCmd, "UserIdentity is managed by the setup flow, not kubectl edit")

	// No displayName → Name falls back to subject; no condition → Unknown.
	b := rowByName(t, rows, "user:Ym9i", "")
	assert.Equal(t, "Unknown", b.Status)
}

// TestUsersDetail_ResolvesBySubjectOrDisplayNotOnlyMetadataName proves the user
// detail resolves the id (which the LIST emits as displayName||subject, NOT the
// opaque metadata.name hash) by listing + matching subject / displayName /
// metadata.name — so a link keyed on any of those opens the page instead of 404.
func TestUsersDetail_ResolvesBySubjectOrDisplayNotOnlyMetadataName(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "hashabc"}, // metadata.name is a hash
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:YWxpY2U=",
			DisplayName: "alice@example.com",
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "gh", Type: "static"}},
		},
		Status: spiceboxv1alpha1.UserIdentityStatus{
			ResolvedCredentials: []string{"gh"},
			Conditions:          []metav1.Condition{cond(spiceboxv1alpha1.UserIdentityConditionValid, metav1.ConditionTrue, "AllReferencesResolve")},
		},
	}
	c := newClient(t, ui)

	// Every id the list could have emitted (subject, displayName) AND the real
	// metadata.name must all resolve to the same detail.
	for _, id := range []string{"user:YWxpY2U=", "alice@example.com", "hashabc"} {
		t.Run("id="+id+" resolves", func(t *testing.T) {
			d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", id)
			require.NoError(t, err)
			require.NotNil(t, d, "id %q must resolve, not 404", id)
			assert.Equal(t, "alice@example.com", d.Name)
			assert.Equal(t, "user:YWxpY2U=", d.Description)
			over := sectionByID(t, d, "overview")
			assert.Equal(t, "user:YWxpY2U=", fieldVal(over, "Subject"))
		})
	}

	// A genuinely-unknown id is the only 404.
	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:ghost")
	require.NoError(t, err)
	assert.Nil(t, d, "no matching UserIdentity → nil detail (404)")
}

// TestUsersDetail_ChannelIdentitiesSection proves the user detail page surfaces
// the channel-identity directory (Task 7's status.channelIdentities) as a
// dedicated list section — one row per (kind, domain, externalID), even when
// the same human has two entries across unrelated Slack workspaces.
func TestUsersDetail_ChannelIdentitiesSection(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-abc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U", DisplayName: "alice@example.com"},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{
			{Kind: "slack", Domain: "T0COMPANY", ExternalID: "U0ALICE", DisplayName: "Alice"},
			{Kind: "slack", Domain: "T0PARTNER", ExternalID: "U0GUEST", DisplayName: "Alice (Guest)"},
		}},
	}
	c := newClient(t, ui)

	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:YWxpY2U")
	require.NoError(t, err)
	require.NotNil(t, d)

	sec := sectionByID(t, d, "channelidentities")
	assert.Equal(t, config.SectionList, sec.Kind)
	require.Len(t, sec.Items, 2)
	assert.Equal(t, "Alice", sec.Items[0].Title)
	assert.Contains(t, sec.Items[0].Subtitle, "T0COMPANY")
	assert.Contains(t, sec.Items[0].Subtitle, "U0ALICE")
	assert.Equal(t, "slack", badgeValItem(sec.Items[0], "kind"))

	assert.Equal(t, "Alice (Guest)", sec.Items[1].Title)
	assert.Contains(t, sec.Items[1].Subtitle, "T0PARTNER")
}

// TestUsersDetail_NoChannelIdentitiesSectionWhenEmpty proves an empty
// ChannelIdentities directory omits the tab entirely (appendSection drops
// zero-item sections), matching the pattern used by every other list tab.
func TestUsersDetail_NoChannelIdentitiesSectionWhenEmpty(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-def"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:Ym9i"},
	}
	c := newClient(t, ui)

	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:Ym9i")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.False(t, hasSection(d, "channelidentities"), "no channel identities → no tab")
}

// TestUsersDetail_DirectoryIdentitiesReaderNilOmitsSection proves that with no
// reader installed (the state of every build until admind.New wires one — see
// SetSubjectIdentityReader's doc), the users detail page carries no Directory
// identities tab at all rather than an empty or unavailable one.
func TestUsersDetail_DirectoryIdentitiesReaderNilOmitsSection(t *testing.T) {
	SetSubjectIdentityReader(nil)
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-nil"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	c := newClient(t, ui)

	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:YWxpY2U=")
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.False(t, hasSection(d, "directoryidentities"), "no reader installed → no tab")
}

// TestUsersDetail_DirectoryIdentitiesReaderCalledWithSpecSubject proves the
// users detail projector calls the injected reader with the RAW
// spec.subject value (a "user:<canonical>" SpiceDB reference), and renders
// what the reader returns as the Directory identities tab.
func TestUsersDetail_DirectoryIdentitiesReaderCalledWithSpecSubject(t *testing.T) {
	var gotSubject string
	SetSubjectIdentityReader(func(_ context.Context, subject string) (spicedb.SubjectIdentities, error) {
		gotSubject = subject
		return spicedb.SubjectIdentities{Identities: []spicedb.SubjectIdentity{
			{Definition: "github_user", Relation: "user", ObjectID: "999", Source: "Account attestation"},
		}}, nil
	})
	t.Cleanup(func() { SetSubjectIdentityReader(nil) })

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-abc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	c := newClient(t, ui)

	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:YWxpY2U=")
	require.NoError(t, err)
	require.NotNil(t, d)

	assert.Equal(t, "user:YWxpY2U=", gotSubject,
		"the reader is called with spec.subject verbatim; conversion is admind's job")
	sec := sectionByID(t, d, "directoryidentities")
	require.Len(t, sec.Items, 1)
	assert.Equal(t, "github_user:999", sec.Items[0].Title)
}

// TestUsersDetail_DirectoryIdentitiesReaderErrorRendersUnavailable proves a
// failed read surfaces as the "unavailable: <reason>" text tab, not a silently
// empty or absent one.
func TestUsersDetail_DirectoryIdentitiesReaderErrorRendersUnavailable(t *testing.T) {
	SetSubjectIdentityReader(func(_ context.Context, _ string) (spicedb.SubjectIdentities, error) {
		return spicedb.SubjectIdentities{}, errors.New("spicedb unreachable")
	})
	t.Cleanup(func() { SetSubjectIdentityReader(nil) })

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-abc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	c := newClient(t, ui)

	d, err := (usersDetailProjector{}).Detail(context.Background(), c, "", "user:YWxpY2U=")
	require.NoError(t, err)
	require.NotNil(t, d)

	sec := sectionByID(t, d, "directoryidentities")
	assert.Equal(t, config.SectionText, sec.Kind)
	assert.Contains(t, sec.Text, "unavailable")
	assert.Contains(t, sec.Text, "spicedb unreachable")
}
