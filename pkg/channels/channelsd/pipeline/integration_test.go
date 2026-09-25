//go:build integration

// End-to-end pipeline tests against a real SpiceDB. The unit tests in
// pipeline_test.go use a fake authz to exercise control flow; these
// tests use the real authz.Client + a containerized SpiceDB (started
// via dockertest, see test/testspicedb) so the whole Deliver→
// CheckInteract→outcome path is exercised. They lock the contract
// that broke in production: "approve writes a SpiceDB tuple, next
// inbound from the approved user gets allowed."
//
// Run with:
//
//	go test -tags=integration ./pkg/channels/channelsd/pipeline/
//
// Requires a running docker daemon. Each test gets its own isolated
// SpiceDB datastore via a unique bearer token.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// integrationFixture wires a real authz.Client against a SpiceDB
// instance + a fake k8s client containing a pre-seeded Channel and
// AgentSession. Returns the Pipeline + the canonical id we'll use
// for the requester so the test can prep SpiceDB state under the
// same identity Deliver will check against.
type integrationFixture struct {
	pipeline    *Pipeline
	authz       *spicedb.Client
	memory      *fakeMemory
	nats        *fakeNATS
	channel     *spiceboxv1alpha1.Channel
	session     *spiceboxv1alpha1.AgentSession
	canonical   identity.CanonicalUserID
	requester   channelkinds.ExternalIdentity
	sessionNS   string
	sessionName string
	channelKey  string
}

func newIntegrationFixture(t *testing.T, requesterEmail, requesterSlackID string) *integrationFixture {
	t.Helper()
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	az, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = az.Close() })

	requester := channelkinds.ExternalIdentity{
		Kind:       "slack",
		ExternalID: identity.RawExternalID(requesterSlackID),
		Email:      identity.Email(requesterEmail),
	}
	canonical, err := identity.FromExternal(requester.Kind, requester.TeamScope, requester.ExternalID, requester.Email).Canonical()
	require.NoError(t, err, "identity Canonical")

	// Unique channel and session per test so reruns don't collide.
	suffix := uniqHex(t, 8)
	chName := "ch-" + suffix
	sessNS := "integration-test"
	sessName := "sess-" + suffix
	channelKey := "thread:" + chName + ":1.0"
	keyHash := sha256HexTest(channelKey)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: chName, Namespace: sessNS},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "fake", AgentClass: "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: sessName, Namespace: sessNS,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: chName,
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U_owner",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: chName, Kind: "fake", Key: channelKey,
				NATSSubjectPrefix: "ap.session." + sessNS + "." + sessName,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}

	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ch, sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	mem := &fakeMemory{seeded: map[string][]MemTurn{}}
	natsRec := &fakeNATS{}
	caps := func(_ string) []string { return []string{"text", "markdown"} }
	p := NewPipeline(cli, az, mem, natsRec, caps)
	p.Engine = engine.New(engine.Deps{
		SessionInteractChecker: az,
		Granter:                az,
		Lookuper:               az,
		ApproverChecker:        az,
	})
	p.Now = func() time.Time { return time.Date(2026, 5, 1, 22, 0, 0, 0, time.UTC) }
	p.MarkerSigner = testMarkerSigner

	t.Cleanup(func() {
		_ = az.DeleteAgentSessionRelationships(context.Background(), sessNS, sessName)
	})

	return &integrationFixture{
		pipeline: p, authz: az, memory: mem, nats: natsRec,
		channel: ch, session: sess,
		canonical: canonical, requester: requester,
		sessionNS: sessNS, sessionName: sessName, channelKey: channelKey,
	}
}

func (f *integrationFixture) deliver(t *testing.T, text string) channelkinds.InboundDecision {
	t.Helper()
	dec, err := f.pipeline.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     f.channel,
		ExternalIDs: f.requester,
		ChannelKey:  f.channelKey,
		MessageText: text,
	})
	require.NoError(t, err, "Deliver")
	return dec
}

func uniqHex(t *testing.T, nbytes int) string {
	t.Helper()
	buf := make([]byte, nbytes)
	_, err := rand.Read(buf)
	require.NoError(t, err, "rand.Read")
	return hex.EncodeToString(buf)
}

// TestIntegration_Pipeline_NoGrant_DeniesAndPublishesPermissionRequest
// verifies the negative path through the full pipeline. With NO
// SpiceDB tuples written, an inbound from a non-started_by user must
// be denied AND publish a permission_request envelope so the original
// requester gets a DM.
func TestIntegration_Pipeline_NoGrant_DeniesAndPublishesPermissionRequest(t *testing.T) {
	f := newIntegrationFixture(t, "sam@example.com", "U_SAM")

	dec := f.deliver(t, "tell me a joke")

	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome,
		"no SpiceDB grant exists")
	assert.Empty(t, f.memory.appends, "Memory.Append called on a denial")

	requestPublishes := 0
	for i, s := range f.nats.subjects {
		if !strings.HasSuffix(s, ".out."+string(channelevents.KindInteractionRequest)) {
			continue
		}
		requestPublishes++
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(f.nats.payloads[i], &env), "unmarshal envelope")
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal InteractionRequestPayload")
		assert.Equal(t, categories.PermissionRequest, pl.Category,
			"published interaction_request must carry Category=permission_request (a deny->DM, not some other interaction)")
	}
	assert.Equal(t, 1, requestPublishes,
		"deny path must DM the original requester (publish 1 interaction_request(permission_request))")
}

// TestIntegration_Pipeline_OwnerGrant_RoutesAndAppends verifies the
// positive path: writing the session owner for the requester makes
// CheckInteract return true → Deliver routes the message, appends
// memory, publishes the wakeup envelope. interact = owner + participant
// - denied, so the owner (written at session creation by the operator's
// owner resolver) works through SpiceDB.
func TestIntegration_Pipeline_OwnerGrant_RoutesAndAppends(t *testing.T) {
	f := newIntegrationFixture(t, "alice@example.com", "U_ALICE")

	require.NoError(t, f.authz.TouchOwner(context.Background(), f.sessionNS, f.sessionName, "user:"+f.canonical.String()),
		"TouchOwner")

	dec := f.deliver(t, "follow-up from owner")

	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "owner tuple exists")
	assert.Len(t, f.memory.appends, 1, "Memory.Append calls")

	wakeup := 0
	for _, s := range f.nats.subjects {
		if strings.HasSuffix(s, ".in.user_message") {
			wakeup++
		}
	}
	assert.Equal(t, 1, wakeup, "wakeup publishes")
}

// TestIntegration_Pipeline_ParticipantGrant_RoutesAndAppends is the
// regression-locking test: the production bug was that approving a
// requester wrote a SpiceDB participant tuple, but the next inbound
// still got denied because of a write/check shape mismatch. With the
// simplified user-only schema this MUST work.
func TestIntegration_Pipeline_ParticipantGrant_RoutesAndAppends(t *testing.T) {
	f := newIntegrationFixture(t, "sam@example.com", "U_SAM")

	require.NoError(t, f.authz.TouchInteractParticipantUser(context.Background(), f.sessionNS, f.sessionName, f.canonical),
		"TouchInteractParticipantUser")

	dec := f.deliver(t, "approved coworker speaks")

	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"participant tuple exists for canonical %s", f.canonical)
	assert.Len(t, f.memory.appends, 1, "Memory.Append calls")
}

// TestIntegration_Pipeline_DeniedTuple_SilentDrops verifies the
// blocklist path: a previously-denied user's inbound is silent-dropped
// (Outcome=DeniedByPermission, notice suppressed, no permission_request
// envelope, no memory append).
func TestIntegration_Pipeline_DeniedTuple_SilentDrops(t *testing.T) {
	f := newIntegrationFixture(t, "sam@example.com", "U_SAM")

	require.NoError(t, f.authz.TouchDeniedUser(context.Background(), f.sessionNS, f.sessionName, f.canonical),
		"TouchDeniedUser")

	dec := f.deliver(t, "should be silently dropped")

	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	assert.True(t, dec.Notice.IsSuppressed(), "denied user must be silent, no in-thread message")
	for _, s := range f.nats.subjects {
		assert.False(t, strings.HasSuffix(s, ".out."+string(channelevents.KindInteractionRequest)),
			"denied user triggered interaction_request publish on %q", s)
	}
	assert.Empty(t, f.memory.appends, "Memory.Append called on a silent-drop")
}

// TestIntegration_Pipeline_GroupGrant_RoutesAndAppends verifies that
// AgentClass.spec.sessionInteractPermission "group:<g>#member" — the
// class-wide grant — actually allows a member of that group through
// the full pipeline. group#member walks server-side to user:<canonical>.
func TestIntegration_Pipeline_GroupGrant_RoutesAndAppends(t *testing.T) {
	f := newIntegrationFixture(t, "alice@example.com", "U_ALICE")
	groupName := "test-group-" + uniqHex(t, 4)

	require.NoError(t, f.authz.AddGroupMember(context.Background(), groupName, f.canonical), "AddGroupMember")
	require.NoError(t, f.authz.TouchInteractParticipant(context.Background(), f.sessionNS, f.sessionName, "group:"+groupName+"#member"),
		"TouchInteractParticipant")
	t.Cleanup(func() {
		_ = f.authz.DeleteGroupMember(context.Background(), groupName, f.canonical)
	})

	dec := f.deliver(t, "alice is in test-group")

	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome,
		"alice is a member of group:%s, granted via class-wide participant", groupName)
	assert.Len(t, f.memory.appends, 1, "Memory.Append calls")
}

// TestIntegration_Pipeline_DeniedOverridesParticipant verifies the
// schema's `interact = owner + participant - denied` actually
// causes a denied tuple to OVERRIDE a participant tuple. Same user,
// both tuples written → silent drop.
func TestIntegration_Pipeline_DeniedOverridesParticipant(t *testing.T) {
	f := newIntegrationFixture(t, "sam@example.com", "U_SAM")

	require.NoError(t, f.authz.TouchInteractParticipantUser(context.Background(), f.sessionNS, f.sessionName, f.canonical),
		"TouchInteractParticipantUser")
	require.NoError(t, f.authz.TouchDeniedUser(context.Background(), f.sessionNS, f.sessionName, f.canonical),
		"TouchDeniedUser")

	dec := f.deliver(t, "should still be denied because of denied tuple")

	require.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome,
		"denied subtracts from participant per schema")
	assert.True(t, dec.Notice.IsSuppressed(), "denied path is silent")
}
