// pkg/channels/channelkinds/slack/app_home_panic_test.go
//
// The App Home tab is drawn from the listener's dispatch goroutine
// (listener.go's run loop), which has no panic recovery: anything that panics
// inside handleAppHomeOpened takes the whole channelsd process with it. The
// trigger is per-USER and the state behind it is persistent, so a panic here is
// not a one-off — every workspace member who opens the tab re-kills the process.
//
// That makes the card builder's index arithmetic a process-availability
// invariant rather than a rendering detail, and this file pins it end to end:
// through the real cluster reads, not by hand-building the view input.
package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// recordingHomeClient captures the view handleAppHomeOpened publishes so the
// test can assert on what a reader would actually see, not merely that nothing
// blew up.
type recordingHomeClient struct {
	*fakeslack.Client
	published []slackapi.HomeTabViewRequest
}

func (c *recordingHomeClient) PublishViewContext(_ context.Context, req slackapi.PublishViewContextRequest) (*slackapi.ViewResponse, error) {
	c.published = append(c.published, req.View)
	return &slackapi.ViewResponse{}, nil
}

// toolkitOnlyPassthroughListener wires the cluster state for the case that
// matters: a userPassthrough AgentClass whose ONLY credential comes from a
// toolkit's sensitive env var. passthrough.RequiredBestEffort returns it, the
// user has linked it — and by construction no MCPServer declares it, which is
// exactly the shape passthroughcatalog's label resolver drops.
func toolkitOnlyPassthroughListener(t *testing.T, slackUserID string) (*slackListener, *recordingHomeClient) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	toolkit := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "tracker-cli"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "tracker-cli",
			ToolkitRevision: "v1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tracker"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "builtin", Name: "tracker-cli"},
			Env: spiceboxv1alpha1.ToolkitEnv{Allowed: []spiceboxv1alpha1.ToolkitEnvVar{{
				Name: "TRACKER_TOKEN", Credential: "tracker-token", Sensitive: true,
			}}},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{},
		},
	}
	toolspec := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "tracker-spec"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             "tracker-spec",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "tracker-cli", Revision: "v1"},
			AllowSubcommands: []string{},
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName:  "Demo Agent",
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			ToolBundles: []spiceboxv1alpha1.ToolBundle{
				{Name: "tracking", Toolspecs: []string{"tracker-spec"}},
			},
		},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-slack", Namespace: "demo"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", AgentClass: "demo-agent"},
	}

	subj, err := identity.FromExternal("slack", "T_HOME", identity.RawExternalID(slackUserID), "member@example.com").Subject()
	require.NoError(t, err, "canonical subject for the fixture user")
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: useridentity.NameForSubject(subj)},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     subj.String(),
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "tracker-token", Type: "static"}},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(toolkit, toolspec, ac, ch, ui).Build()

	api := &recordingHomeClient{Client: fakeslack.New()}
	api.SeedUser(&slackapi.User{
		ID: slackUserID, TeamID: "T_HOME",
		Profile: slackapi.UserProfile{Email: "member@example.com"},
	})

	// A non-empty ExternalBaseURL is what puts the page's icon slot in play —
	// the branch that reads a linked credential and its label together. The
	// bound Channel names the single agent this Home tab is about.
	l := &slackListener{
		api:             api,
		installedTeamID: "T_HOME",
		deps: channelkinds.Deps{
			K8sClient:       cli,
			Channel:         ch,
			ExternalBaseURL: func() string { return "https://identity.example.invalid" },
		},
	}
	return l, api
}

// TestHandleAppHomeOpened_ToolkitDerivedLinkedCredentialSurvives is the RED
// test for the index-out-of-range that killed channelsd.
//
// The card builder guarded the icon slot on len(LinkedServiceCredNames) and
// then read LinkedServiceLabels[0]. The two were populated from different
// sources — the raw intersection of the class's credentials with the user's,
// and the resolver's OUTPUT, which skips credentials with no backing MCPServer,
// dedups, and sorts. A toolkit-derived credential has no MCPServer by
// construction, so the linked-name slice had one entry and the label slice had
// none.
//
// Nothing about this state is exotic: it is any userPassthrough AgentClass
// whose credentials come from a ToolBundle rather than an MCPServer, with the
// user having linked one. Every workspace member who opened the Home tab from
// that point on panicked the listener's unrecovered dispatch goroutine.
func TestHandleAppHomeOpened_ToolkitDerivedLinkedCredentialSurvives(t *testing.T) {
	l, api := toolkitOnlyPassthroughListener(t, "U_MEMBER")

	// A panic here is the defect: the dispatch goroutine has no recover, so
	// this is a whole-process kill in production.
	require.NotPanics(t, func() {
		l.handleAppHomeOpened(context.Background(), &slackevents.AppHomeOpenedEvent{User: "U_MEMBER", Tab: "home"})
	}, "opening the Home tab must not panic the listener's unrecovered dispatch goroutine")

	require.Len(t, api.published, 1, "the tab must still be published")
	got := appHomeText(api.published[0])
	assert.Contains(t, got, "Demo Agent", "the card renders for the class the user can talk to")
}
