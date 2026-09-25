package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

func TestRegistered(t *testing.T) {
	k, ok := registry.Get("fake")
	require.True(t, ok, "fake kind not registered")
	assert.Equal(t, "fake", k.Name(), "kind name")
}

// TestWizardRun drives the whole flow the way the CLI does — ask what is
// needed, then build the manifests from the answers — and pins the fixture
// those manifests are.
//
// It asks nothing, so the answer map is empty and every value below comes from
// the WizardInput or from the fixture itself; that is the property the fake
// kind exists for, and the reason this is a manifest assertion rather than a
// prompt one.
func TestWizardRun(t *testing.T) {
	w := Kind{}.Wizard()

	in := channelkinds.WizardInput{Namespace: "default"}
	qs, err := w.Inputs(context.Background(), in)
	require.NoError(t, err, "wizard inputs")
	require.Empty(t, qs, "the fake flow asks nothing")

	out, err := w.Result(in, map[string]string{})
	require.NoError(t, err, "wizard result")
	require.NotNil(t, out.ChannelManifest, "channel manifest")
	assert.Equal(t, "fake", out.ChannelManifest.Spec.Kind, "channel manifest kind")
	assert.Equal(t, "fake-channel", out.ChannelManifest.Name, "channel manifest name")
	assert.Equal(t, "default", out.ChannelManifest.Namespace, "channel manifest namespace")
	require.NotNil(t, out.SecretManifest, "secret manifest")
	assert.Equal(t, "fake-creds", out.SecretManifest.Name, "secret manifest name")
	assert.Equal(t, "default", out.SecretManifest.Namespace, "secret manifest namespace")
}

// TestWizardRefusesWithoutANamespace: every manifest the fake wizard builds is
// namespaced, so a missing namespace must fail rather than produce
// cluster-scoped nonsense.
func TestWizardRefusesWithoutANamespace(t *testing.T) {
	_, err := Kind{}.Wizard().Result(channelkinds.WizardInput{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

func TestKind_UserAttributable(t *testing.T) {
	assert.True(t, (Kind{}).UserAttributable(), "fake.UserAttributable() must be true")
}

func TestFakePermReqSender_RecordsSessionJoinRequest(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-join"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	s := Kind{}.SubChannelSender("permission_request", channelkinds.Deps{Channel: ch})
	require.NotNil(t, s, `SubChannelSender("permission_request") should return a sender`)

	pl := channelevents.PermissionRequestPayload{Preview: "wants to join"}
	env, err := channelevents.BuildEnvelope("ns", "sess-join",
		channelevents.KindPermissionRequest, pl)
	require.NoError(t, err)
	_, err = s.Send(context.Background(), channelkinds.SessionInfo{
		Namespace: "ns", Name: "sess-join",
	}, env)
	require.NoError(t, err, "Send")

	got := DriverFor("ns", "ch-join").SessionJoinPrompts()
	require.Len(t, got, 1, "one session-join prompt recorded")
	assert.Equal(t, "wants to join", got[0].Payload.Preview)
	assert.Equal(t, "sess-join", got[0].SessionRef.Name)
}

// TestFakeSubChannelSender_TypedApprovalNamesRoutedOff asserts that tool
// approval and info leakage have no bespoke sub-channel of their own: both
// render through the generic "interaction" sub-channel, so their typed names
// must resolve to nil.
func TestFakeSubChannelSender_TypedApprovalNamesRoutedOff(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-routed-off"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	for _, name := range []string{"tool_approval", "info_leakage_approval"} {
		assert.Nil(t, Kind{}.SubChannelSender(name, channelkinds.Deps{Channel: ch}),
			"SubChannelSender(%q) must resolve to nil after the generic-interaction cutover", name)
	}
}

func TestFakePermReqSender_RejectsUnknownEnvelopeKind(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-approval-bad"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	s := Kind{}.SubChannelSender("permission_request", channelkinds.Deps{Channel: ch})
	require.NotNil(t, s)

	env, err := channelevents.BuildEnvelope("ns", "sess",
		channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "x"})
	require.NoError(t, err)
	_, err = s.Send(context.Background(), channelkinds.SessionInfo{
		Namespace: "ns", Name: "sess",
	}, env)
	require.Error(t, err, "permission_request sender must reject non-approval envelope kinds")
}

func TestKind_RelayedByChannelsd(t *testing.T) {
	assert.True(t, (&Kind{}).RelayedByChannelsd(),
		"fake kind is hosted by channelsd")
}

func TestKind_SpawnsSessionOnInbound(t *testing.T) {
	assert.True(t, (&Kind{}).SpawnsSessionOnInbound(),
		"fake kind models a durable channel; an inbound spawns a new session")
}

func TestFakeSender_RecordsPlanUpdateEnvelopes(t *testing.T) {
	// Build a Channel + Driver per existing helper pattern in this file.
	// (See the existing tests for how to wire ch + sender; reuse that.)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch1"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	k := Kind{}
	s := k.NewSender(channelkinds.Deps{Channel: ch})

	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "A", Status: "pending"},
		},
	}
	env, err := channelevents.BuildEnvelope("ns", "sess1", channelevents.KindPlanUpdate, pl)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "ns", Name: "sess1"}, env)
	require.NoError(t, err)

	got := DriverFor("ns", "ch1").Plans()
	require.Len(t, got, 1)
	require.Equal(t, "main", got[0].PlanName)
}

func TestFakeMonitoringSender_RecordsEvents(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon-ch"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake"},
	}
	sender := Kind{}.NewMonitoringSender(channelkinds.Deps{Channel: ch})
	require.NotNil(t, sender, "fake kind must return a MonitoringSender")

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh",
	}
	require.NoError(t, sender.SendMonitoring(context.Background(), ev))

	got := DriverFor("default", "mon-ch").MonitoringEvents()
	require.Len(t, got, 1)
	assert.Equal(t, "github-bot", got[0].Source.Name)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, got[0].Transition)
}

func TestFakeSender_ToolProgress_Ignored(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-tool-progress"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	k := Kind{}
	s := k.NewSender(channelkinds.Deps{Channel: ch})

	pl := channelevents.ToolProgressPayload{
		CallID:         "tc1",
		Name:           "example_tool",
		BudgetSeconds:  10,
		ElapsedSeconds: 5,
	}
	env, err := channelevents.BuildEnvelope("ns", "sess1", channelevents.KindToolProgress, pl)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "ns", Name: "sess1"}, env)
	require.NoError(t, err, "fake sender must not error on tool_progress")
}
