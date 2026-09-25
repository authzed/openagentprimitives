// pkg/channels/channelsd/pipeline/workshop_handoff_test.go
package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// --- fixtures --------------------------------------------------------------

const (
	whNS          = "builder-h"
	whSession     = "builder-y"
	whWorkshop    = whSession + "-workshop"
	whSidecar     = "builder-toolbox"
	whSuggested   = "weather-ai"
	whArtifactRef = "artifacts/weather-ai-skill"
)

// whFixtureSession returns a builder AgentSession in whNS -- everything
// ReconcileOne needs to resolve the builder session before it will deliver
// either card.
func whFixtureSession(mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: whSession, Namespace: whNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{},
	}
	for _, m := range mutate {
		m(sess)
	}
	return sess
}

// whFixtureWorkshop returns a Workshop with both an install and a capability
// request pending, and no delivery status recorded. Tests narrow to one kind
// of request via mutate.
func whFixtureWorkshop(mutate ...func(*spiceboxv1alpha1.Workshop)) *spiceboxv1alpha1.Workshop {
	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Name: whWorkshop, Namespace: whNS},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:        spiceboxv1alpha1.NamespacedRef{Namespace: whNS, Name: whSession},
			SidecarToolbox: whSidecar,
			InstallRequest: &spiceboxv1alpha1.WorkshopInstallRequest{
				SuggestedName: whSuggested,
				BundleDigest:  "sha256:deadbeef",
			},
			CapabilityRequest: &spiceboxv1alpha1.WorkshopCapabilityRequest{
				Summary:     "adds a weather lookup tool",
				ArtifactRef: whArtifactRef,
			},
		},
	}
	for _, m := range mutate {
		m(ws)
	}
	return ws
}

// newWorkshopHandoffWatcher wires a WorkshopHandoffWatcher against a fake k8s
// client + a fresh capturingPublisher. now is fixed so timestamps are
// deterministic.
func newWorkshopHandoffWatcher(t *testing.T, objs ...client.Object) (*WorkshopHandoffWatcher, client.Client, *capturingPublisher) {
	t.Helper()
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		Build()
	pub := &capturingPublisher{}
	w := &WorkshopHandoffWatcher{
		K8s:             cli,
		ExternalBaseURL: func() string { return "https://admin.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}
	return w, cli, pub
}

func getWorkshopHandoffFresh(t *testing.T, cli client.Client, ns, name string) *spiceboxv1alpha1.Workshop {
	t.Helper()
	var ws spiceboxv1alpha1.Workshop
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ws))
	return &ws
}

// --- (a) install request ----------------------------------------------------

// TestWorkshopHandoffWatcher_InstallRequestPublishesAndStamps proves the
// install_request MonitoringEvent shape and the status.install stamp on the
// happy path.
func TestWorkshopHandoffWatcher_InstallRequestPublishesAndStamps(t *testing.T) {
	sess := whFixtureSession()
	ws := whFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) { ws.Spec.CapabilityRequest = nil })
	w, cli, pub := newWorkshopHandoffWatcher(t, sess, ws, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), ws))

	require.Equal(t, 1, pub.countMonitoring(), "exactly one MonitoringEvent published")
	ev := pub.findMonitoring(t)
	assert.Equal(t, "install_request", ev.Category)
	assert.Equal(t, channelevents.MonitoringLevelWarning, ev.Level)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
	assert.Equal(t, "Workshop", ev.Source.Kind)
	assert.Equal(t, whNS, ev.Source.Namespace)
	assert.Equal(t, whWorkshop, ev.Source.Name)
	assert.Equal(t, "WorkshopInstallRequested", ev.Condition)
	assert.Contains(t, ev.Summary, whSuggested)
	assert.Contains(t, ev.Summary, whNS+"/"+whSession)
	assert.Contains(t, ev.Hint, "https://admin.example.test/admin", "the hint must carry the admin console link")

	got := getWorkshopHandoffFresh(t, cli, whNS, whWorkshop)
	require.NotNil(t, got.Status.Install)
	assert.Equal(t, "Requested", got.Status.Install.Phase)
	assert.NotNil(t, got.Status.Install.RequestedAt)
	assert.NotNil(t, got.Status.Install.DeliveredAt)
}

// --- (b) capability request -------------------------------------------------

// TestWorkshopHandoffWatcher_CapabilityRequestPublishesAndStamps mirrors (a)
// for the capability_request card.
func TestWorkshopHandoffWatcher_CapabilityRequestPublishesAndStamps(t *testing.T) {
	sess := whFixtureSession()
	ws := whFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) { ws.Spec.InstallRequest = nil })
	w, cli, pub := newWorkshopHandoffWatcher(t, sess, ws, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), ws))

	require.Equal(t, 1, pub.countMonitoring(), "exactly one MonitoringEvent published")
	ev := pub.findMonitoring(t)
	assert.Equal(t, "capability_request", ev.Category)
	assert.Equal(t, "WorkshopCapabilityRecommended", ev.Condition)
	assert.Contains(t, ev.Summary, "adds a weather lookup tool")
	assert.Contains(t, ev.Summary, whArtifactRef)

	got := getWorkshopHandoffFresh(t, cli, whNS, whWorkshop)
	require.NotNil(t, got.Status.CapabilityRequest)
	assert.NotNil(t, got.Status.CapabilityRequest.DeliveredAt)
	assert.NotEmpty(t, got.Status.CapabilityRequest.NoticeRef)
}

// --- (c) dedup ---------------------------------------------------------------

// TestWorkshopHandoffWatcher_DedupSuppressesASecondTick: once delivered,
// stamped status.install/.capabilityRequest suppress a re-publish.
func TestWorkshopHandoffWatcher_DedupSuppressesASecondTick(t *testing.T) {
	sess := whFixtureSession()
	ws := whFixtureWorkshop()
	w, cli, pub := newWorkshopHandoffWatcher(t, sess, ws, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), ws))
	require.Equal(t, 2, pub.countMonitoring(), "one install_request + one capability_request card")

	got := getWorkshopHandoffFresh(t, cli, whNS, whWorkshop)
	require.NoError(t, w.ReconcileOne(context.Background(), got))
	assert.Equal(t, 2, pub.countMonitoring(), "a second tick after both are stamped delivered must publish nothing")
}

// --- (d) fail-closed: no role=monitoring recipient --------------------------

// TestWorkshopHandoffWatcher_NoRecipientFailsClosed: no role=monitoring
// Channel means no publish AND no stamp, so the request retries next tick.
func TestWorkshopHandoffWatcher_NoRecipientFailsClosed(t *testing.T) {
	sess := whFixtureSession()
	ws := whFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) { ws.Spec.CapabilityRequest = nil })
	w, cli, pub := newWorkshopHandoffWatcher(t, sess, ws) // no monitoring Channel

	require.NoError(t, w.ReconcileOne(context.Background(), ws),
		"a per-request delivery failure must not fail the whole workshop reconcile")

	assert.Zero(t, pub.countMonitoring(), "no recipient means no publish")
	got := getWorkshopHandoffFresh(t, cli, whNS, whWorkshop)
	assert.Nil(t, got.Status.Install, "no recipient means no stamp -- the request must retry next tick")
}

// --- (e) missing builder session --------------------------------------------

// TestWorkshopHandoffWatcher_MissingBuilderSessionSkipsWithoutPanic: a
// Workshop whose builder AgentSession does not exist (yet, or ever) must be
// skipped quietly -- never a panic, never a publish.
func TestWorkshopHandoffWatcher_MissingBuilderSessionSkipsWithoutPanic(t *testing.T) {
	ws := whFixtureWorkshop()
	w, cli, pub := newWorkshopHandoffWatcher(t, ws, fixtureMonitoringChannel(), fixtureMonitoringSecret())
	// No AgentSession object created.

	require.NotPanics(t, func() {
		err := w.ReconcileOne(context.Background(), ws)
		assert.NoError(t, err, "a missing builder session is a skip, not a structural failure")
	})

	assert.Zero(t, pub.countMonitoring())
	got := getWorkshopHandoffFresh(t, cli, whNS, whWorkshop)
	assert.Nil(t, got.Status.Install)
	assert.Nil(t, got.Status.CapabilityRequest)
}

// --- (f) card-injection: builder-supplied text is sanitized -----------------

// TestWorkshopHandoffWatcher_BuilderTextIsSanitized proves every
// builder-supplied string (suggestedName, capability summary, artifact ref)
// is run through sanitizeCardText before it reaches a card: a newline or
// control character must not survive into the published event.
func TestWorkshopHandoffWatcher_BuilderTextIsSanitized(t *testing.T) {
	const dirtyName = "weather-ai\nX-Injected: true"
	const dirtySummary = "adds weather\x1b[31mFATAL\x1b[0mlookup"
	const dirtyArtifact = "artifacts/weather\nsecond-line"

	sess := whFixtureSession()
	ws := whFixtureWorkshop(func(ws *spiceboxv1alpha1.Workshop) {
		ws.Spec.InstallRequest.SuggestedName = dirtyName
		ws.Spec.CapabilityRequest.Summary = dirtySummary
		ws.Spec.CapabilityRequest.ArtifactRef = dirtyArtifact
	})
	w, _, pub := newWorkshopHandoffWatcher(t, sess, ws, fixtureMonitoringChannel(), fixtureMonitoringSecret())

	require.NoError(t, w.ReconcileOne(context.Background(), ws))
	require.Equal(t, 2, pub.countMonitoring())

	var install, capability channelevents.MonitoringEvent
	for _, ev := range pub.allMonitoring(t) {
		switch ev.Category {
		case "install_request":
			install = ev
		case "capability_request":
			capability = ev
		}
	}

	assert.NotContains(t, install.Summary, "\n", "a sanitized suggestedName can never introduce a second line")
	assert.Contains(t, install.Summary, sanitizeCardText(dirtyName))

	assert.NotContains(t, capability.Summary, "\n")
	assert.NotContains(t, capability.Summary, "\x1b")
	assert.Contains(t, capability.Summary, sanitizeCardText(dirtySummary))
	assert.Contains(t, capability.Summary, sanitizeCardText(dirtyArtifact))
}

// allMonitoring decodes every MonitoringEvent published so far.
func (p *capturingPublisher) allMonitoring(t *testing.T) []channelevents.MonitoringEvent {
	t.Helper()
	var out []channelevents.MonitoringEvent
	for _, m := range p.messages {
		if m.subject != channelevents.MonitoringEventSubject {
			continue
		}
		var ev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(m.data, &ev))
		out = append(out, ev)
	}
	return out
}
