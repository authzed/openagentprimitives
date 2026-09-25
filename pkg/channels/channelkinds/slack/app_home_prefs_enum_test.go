// pkg/channels/channelkinds/slack/app_home_prefs_enum_test.go
//
// buildHomeInputForUser-level tests for the preferences-section seam: the one
// agent this Home tab is about gets a preferences section only when the clicker
// has INTERACTED with it (LookupPersonalizableClassRefs) AND it declares at
// least one userPreferences key. Each case binds the listener to a class
// exercising a distinct branch of that intersection, plus the
// degrade-on-fetch-error path, against one shared cluster fixture.
package slack

import (
	"context"
	"fmt"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// fakePersonalizableClasses is a stub channelkinds.PersonalizableClassLookup:
// it returns a fixed "<ns>/<class>" ref set regardless of the canonical it's
// asked about, so tests can pin exactly which classes the clicker is treated
// as having interacted with, without a live SpiceDB.
type fakePersonalizableClasses struct {
	refs []string
	err  error
}

func (f *fakePersonalizableClasses) LookupPersonalizableClassRefs(_ context.Context, _ identity.CanonicalUserID, _ uint32, _ bool) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.refs, nil
}

// fakePreferencesClient is a stub channelkinds.PreferencesClient:
// GetPreferencesFirstParty answers from a canned map keyed by "ns/class",
// erroring for any key also present in errs. CommitPreferenceFirstParty is
// Task 12's concern and is not exercised here.
type fakePreferencesClient struct {
	snapshots map[string]preferences.SnapshotResponse
	errs      map[string]error
}

func (f *fakePreferencesClient) GetPreferencesFirstParty(_ context.Context, ns, className, _ string) (preferences.SnapshotResponse, error) {
	key := ns + "/" + className
	if err, ok := f.errs[key]; ok {
		return preferences.SnapshotResponse{}, err
	}
	return f.snapshots[key], nil
}

func (f *fakePreferencesClient) CommitPreferenceFirstParty(_ context.Context, _, _ string, _ preferences.CommitRequest) error {
	return fmt.Errorf("fakePreferencesClient: CommitPreferenceFirstParty not exercised by these tests")
}

// buildPrefsEnumFixture wires four AgentClasses in one namespace, each
// exercising a distinct branch of the enumeration ∩ declares-prefs
// intersection:
//
//   - "reviewbot":     LookupPersonalizableClassRefs includes it, declares
//     "notifications"; the fake preferences client answers successfully.
//   - "silent-agent":  included in LR, but declares NO userPreferences.
//   - "stranger-agent": declares userPreferences, but LR does NOT include it.
//   - "broken-agent":  included in LR, declares a preference, but the fake
//     preferences client errors for it.
func buildPrefsEnumFixture(t *testing.T) (*slackListener, *recordingHomeClient) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	const ns = "demo"
	classes := []struct {
		name  string
		prefs []spiceboxv1alpha1.UserPreferenceSchema
	}{
		{name: "reviewbot", prefs: []spiceboxv1alpha1.UserPreferenceSchema{
			{Name: "notifications", Type: "bool", Visibility: "class"},
		}},
		{name: "silent-agent", prefs: nil},
		{name: "stranger-agent", prefs: []spiceboxv1alpha1.UserPreferenceSchema{
			{Name: "digest", Type: "bool", Visibility: "class"},
		}},
		{name: "broken-agent", prefs: []spiceboxv1alpha1.UserPreferenceSchema{
			{Name: "tone", Type: "string"},
		}},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, c := range classes {
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: ns},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				DisplayName:     c.name,
				UserPreferences: c.prefs,
			},
		}
		builder = builder.WithObjects(ac)
	}
	cli := builder.Build()

	api := &recordingHomeClient{Client: fakeslack.New()}
	api.SeedUser(&slackapi.User{
		ID: "U_CLICKER", TeamID: "T_HOME",
		Profile: slackapi.UserProfile{Email: "clicker@example.com"},
	})

	l := &slackListener{
		api:             api,
		installedTeamID: "T_HOME",
		deps: channelkinds.Deps{
			K8sClient: cli,
		},
		personalizableClasses: &fakePersonalizableClasses{
			refs: []string{ns + "/reviewbot", ns + "/silent-agent", ns + "/broken-agent"},
		},
		preferences: &fakePreferencesClient{
			snapshots: map[string]preferences.SnapshotResponse{
				ns + "/reviewbot": {
					Snapshot: preferences.Snapshot{Keys: []preferences.Resolved{
						{Name: "notifications", Type: "bool", Value: jsp(`false`), Source: preferences.SourceUser},
					}},
				},
			},
			errs: map[string]error{
				ns + "/broken-agent": fmt.Errorf("operator memory unreachable"),
			},
		},
	}
	return l, api
}

// bindHomeChannel points the listener's Deps.Channel at one AgentClass, the way
// a per-agent Slack listener is wired in production — its Channel names the
// single class whose Home tab it serves, and buildHomeInputForUser reads
// exactly this to decide which agent the page is about.
func bindHomeChannel(l *slackListener, ns, class string) {
	l.deps.Channel = &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: class + "-slack", Namespace: ns},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: class},
	}
}

func TestBuildHomeInputForUser_PreferencesEnumeration(t *testing.T) {
	const ns = "demo"
	cases := []struct {
		name       string
		class      string
		wantRef    string
		wantPrefs  int
		wantFailed bool
	}{
		{
			name:      "interacted class that declares prefs: resolved snapshot, ClassRef set",
			class:     "reviewbot",
			wantRef:   "demo/reviewbot",
			wantPrefs: 1,
		},
		{
			name:  "interacted class with no declared prefs: no section, no ClassRef",
			class: "silent-agent",
		},
		{
			name:  "declared-prefs class the user never interacted with: no section, no ClassRef",
			class: "stranger-agent",
		},
		{
			name:       "class whose fetch errors: couldn't-load, ClassRef set, tab survives",
			class:      "broken-agent",
			wantRef:    "demo/broken-agent",
			wantFailed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := buildPrefsEnumFixture(t)
			bindHomeChannel(l, ns, tc.class)

			in, err := l.buildHomeInputForUser(context.Background(), "U_CLICKER")
			require.NoError(t, err, "buildHomeInputForUser must succeed even when the preferences fetch errors")
			require.NotNil(t, in.Agent, "the bound agent's page must render")
			require.Equal(t, tc.class, in.Agent.Name)

			assert.Equal(t, tc.wantRef, in.Agent.ClassRef, "ClassRef is set only for a class in preferences scope")
			assert.Len(t, in.Agent.Preferences, tc.wantPrefs)
			assert.Equal(t, tc.wantFailed, in.Agent.PreferencesLoadFailed)
			if tc.wantPrefs > 0 {
				assert.Equal(t, "notifications", in.Agent.Preferences[0].Name)
				assert.Equal(t, preferences.SourceUser, in.Agent.Preferences[0].Source)
			}
		})
	}
}

// TestBuildHomeInputForUser_NoPreferencesClientDegrades pins the nil-client
// branch: LookupPersonalizableClassRefs is wired but l.preferences is nil
// (channelsd started without one configured). A class in scope for a section
// must show the couldn't-load state, not panic and not silently omit.
func TestBuildHomeInputForUser_NoPreferencesClientDegrades(t *testing.T) {
	l, _ := buildPrefsEnumFixture(t)
	bindHomeChannel(l, "demo", "reviewbot")
	l.preferences = nil

	in, err := l.buildHomeInputForUser(context.Background(), "U_CLICKER")
	require.NoError(t, err)
	require.NotNil(t, in.Agent)
	assert.Empty(t, in.Agent.Preferences)
	assert.True(t, in.Agent.PreferencesLoadFailed)
}

// TestBuildHomeInputForUser_LookupPersonalizableClassRefsErrorDegrades pins the
// enumeration-failure branch: LookupPersonalizableClassRefs itself errors. The
// page must still render (as manage-connections only, no preferences section)
// rather than failing the whole tab.
func TestBuildHomeInputForUser_LookupPersonalizableClassRefsErrorDegrades(t *testing.T) {
	l, _ := buildPrefsEnumFixture(t)
	bindHomeChannel(l, "demo", "reviewbot")
	l.personalizableClasses = &fakePersonalizableClasses{err: fmt.Errorf("spicedb unavailable")}

	in, err := l.buildHomeInputForUser(context.Background(), "U_CLICKER")
	require.NoError(t, err)
	require.NotNil(t, in.Agent)
	assert.Empty(t, in.Agent.Preferences, "no preferences section when enumeration failed")
	assert.False(t, in.Agent.PreferencesLoadFailed, "no couldn't-load when the class was never in scope")
}
