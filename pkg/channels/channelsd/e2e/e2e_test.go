//go:build e2e

// Package e2e provides end-to-end integration tests for the channelsd pipeline.
// Run with `go test -tags=e2e ./pkg/channels/channelsd/e2e/...`.
//
// Real envtest k8s + embedded NATS + stub Authz/Memory + the in-process fake
// channel kind, covering inbound routing, denial paths, idle wake-up, and
// authz-failure handling without external services. There is no operator here,
// so scenarios that hand work to a reconciler assert the trigger, not its
// outcome.
package e2e

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsclient "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// stubAuthz implements pipeline.Authz with configurable behavior.
type stubAuthz struct {
	mu               sync.Mutex
	checkResult      bool
	checkErr         error
	startedByErr     error
	participantUsers []identity.CanonicalUserID // canonical ids passed to TouchInteractParticipantUser
}

func (s *stubAuthz) TouchSlackUser(_ context.Context, _, _ string) error { return nil }
func (s *stubAuthz) TouchStartedBy(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedByErr
}
func (s *stubAuthz) TouchOwner(_ context.Context, _, _, _ string) error { return nil }
func (s *stubAuthz) CheckInteract(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkResult, s.checkErr
}
func (s *stubAuthz) CheckDenied(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return false, nil
}

// CheckConverse denies unconditionally: every scenario in this file drives a
// HUMAN inbound, which resolves through CheckInteract above. Deliberately not
// wired to checkResult — an agent-to-agent message reaching this stub would
// mean a scenario changed shape, and it should be refused and noticed rather
// than silently admitted by a flag that was set for a different question.
func (s *stubAuthz) CheckConverse(_ context.Context, _, _, _, _ string, _ bool) (bool, error) {
	return false, nil
}
func (s *stubAuthz) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkResult, s.checkErr
}
func (s *stubAuthz) CheckOwnerOnResource(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkResult, s.checkErr
}

// CheckOnResource satisfies the approver-gate interface; this stub models an
// owner-only resource type, so every permission delegates to the owner answer.
func (s *stubAuthz) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return s.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

func (s *stubAuthz) GrantSlots(_ context.Context, _, _ string, _ []authz.SlotBinding, _ time.Time) error {
	return nil
}

// DeleteSlotGrants satisfies pipeline.Authz: the mint admission sweep. A fresh
// e2e session has no stale predecessor tuples, so a successful no-op is faithful.
func (s *stubAuthz) DeleteSlotGrants(_ context.Context, _, _ string) error { return nil }

// Relations satisfies pipeline.Authz. Nil is the documented safe branch:
// authz.BindApproved narrows session_scope and logs rather than granting
// when its writer is nil, and these e2e scenarios don't assert on the
// grant-tuple shape.
func (s *stubAuthz) Relations() authz.RelWriter { return nil }
func (s *stubAuthz) TouchInteractParticipant(_ context.Context, _, _, _ string) error {
	return nil
}
func (s *stubAuthz) TouchInteractParticipantUser(_ context.Context, _, _ string, canonical identity.CanonicalUserID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.participantUsers = append(s.participantUsers, canonical)
	return nil
}
func (s *stubAuthz) TouchDeniedUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}
func (s *stubAuthz) LookupSubjectIncludes(_ context.Context, _ string, _ identity.CanonicalUserID) (bool, error) {
	return false, nil
}
func (s *stubAuthz) LookupSubjects(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *stubAuthz) LookupInteractSubjects(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

// stubMemory implements pipeline.Memory and records appends.
type stubMemory struct {
	mu      sync.Mutex
	appends []memEntry
	seeded  map[string][]pipeline.MemTurn // key: ns/name; preloaded for ReadAll
}

type memEntry struct {
	ns, name string
	turn     pipeline.MemTurn
}

func (m *stubMemory) Append(_ context.Context, ns, name string, t pipeline.MemTurn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appends = append(m.appends, memEntry{ns, name, t})
	return nil
}

func (m *stubMemory) ReadAll(_ context.Context, ns, name string) ([]pipeline.MemTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seeded[ns+"/"+name], nil
}

// RecordChannelMsgRef is a no-op in the e2e stub: these tests assert on
// routing outcomes, not on channel_msg_ref indexing for the restart UI.
func (m *stubMemory) RecordChannelMsgRef(_ context.Context, _, _, _, _ string, _ int) error {
	return nil
}

// RecordTriggerDelivery is a no-op in the e2e stub for the same reason as
// RecordChannelMsgRef above: these tests assert on routing outcomes, not on
// trigger_delivery evidence.
func (m *stubMemory) RecordTriggerDelivery(_ context.Context, _, _, _, _, _ string, _ []byte) error {
	return nil
}

// RecordEnvelopeFacts is a no-op in the e2e stub for the same reason as
// RecordTriggerDelivery above: these tests assert on routing outcomes, not on
// envelope_fact evidence.
func (m *stubMemory) RecordEnvelopeFacts(_ context.Context, _, _, _, _ string, _ []channelkinds.TriggerFact) error {
	return nil
}

// UploadInboundAsset is a no-op: no scenario here exercises inbound attachments
// (test/e2e's in-process harness does). It drains body and returns a zero result
// with a nil error rather than erroring, so a scenario that later gains an
// attachment-bearing InboundEvent doesn't silently change behavior.
func (m *stubMemory) UploadInboundAsset(_ context.Context, _, _, _, _ string, body io.Reader) (pipeline.InboundAssetResult, error) {
	if body != nil {
		_, _ = io.Copy(io.Discard, body)
	}
	return pipeline.InboundAssetResult{}, nil
}

// stubNATSPub implements pipeline.NATS without actually publishing.
type stubNATSPub struct {
	mu       sync.Mutex
	subjects []string
}

func (n *stubNATSPub) Publish(subj string, _ []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.subjects = append(n.subjects, subj)
	return nil
}

func newPipeline(t *testing.T, az pipeline.Authz, mem pipeline.Memory, nc pipeline.NATS, cli client.Client) *pipeline.Pipeline {
	t.Helper()
	caps := func(_ string) []string { return []string{"text", "markdown"} }
	p := pipeline.NewPipeline(cli, az, mem, nc, caps)
	p.MarkerSigner = testMarkerSigner
	p.Now = func() time.Time { return time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC) }
	return p
}

func startEmbeddedNATS(t *testing.T) *server.Server {
	t.Helper()
	srv := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	t.Cleanup(srv.Shutdown)
	return srv
}

// makeChannel creates a fake-kind Channel CR in the testenv.
func makeChannel(t *testing.T, cli client.Client, ns, name string) *spiceboxv1alpha1.Channel {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-creds"},
		Data:       map[string][]byte{"placeholder": []byte("ok")},
	}
	require.NoError(t, cli.Create(context.Background(), sec))
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			AgentClass:     "ac1",
			SessionScope:   "user",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: name + "-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	require.NoError(t, cli.Create(context.Background(), ch))
	return ch
}

func TestE2ENewSessionFromInbound(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-new")

	az := &stubAuthz{checkResult: true}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "user-1", Email: "alice@example.com"},
		ChannelKey:  "dm:user-1",
		MessageText: "hi from alice",
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	// Assert AgentSession was created with the channel binding.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	got := sessions.Items[0]
	require.NotNil(t, got.Spec.InputChannel)
	assert.Equal(t, "c-new", got.Spec.InputChannel.Name)
	assert.Equal(t, "fake", got.Spec.InputChannel.Kind)
	assert.Equal(t, "hi from alice", got.Spec.Prompt.Inline)
	assert.Equal(t, channelkey.LabelValue("dm:user-1"), got.Labels[spiceboxv1alpha1.LabelChannelKey])
}

func TestE2EContinuationOnExistingSession(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-cont")

	keyHash := channelkey.LabelValue("dm:user-1")
	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "existing",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c-cont",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name:              "c-cont",
				Kind:              "fake",
				Key:               "dm:user-1",
				Capabilities:      []string{"text", "markdown"},
				NATSSubjectPrefix: "ap.session.default.existing",
			},
		},
	}
	require.NoError(t, cli.Create(context.Background(), existing))
	// Set phase=Running via status update.
	existing.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	require.NoError(t, cli.Status().Update(context.Background(), existing))

	az := &stubAuthz{checkResult: true}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "user-1", Email: "alice@example.com"},
		ChannelKey:  "dm:user-1",
		MessageText: "follow-up",
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Len(t, mem.appends, 1)
	// 2 publishes: the wakeup + the mid-turn enqueue ack (this session is
	// phase=Running, so a follow-up message is held and gets a queued ack).
	assert.Len(t, nc.subjects, 2)
}

// TestE2EArchivedSessionResumesInPlace pins, against a real API server, that a
// reply to a session the archive sweep parked routes into that same session.
// No child CR, no fork-trigger: the user keeps their thread.
func TestE2EArchivedSessionResumesInPlace(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-arch")

	keyHash := channelkey.LabelValue("dm:user-1")
	parked := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "parked",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c-arch",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "user-1",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c-arch", Kind: "fake", Key: "dm:user-1",
				Capabilities:      []string{"text", "markdown"},
				NATSSubjectPrefix: "ap.session.default.parked",
			},
		},
	}
	require.NoError(t, cli.Create(context.Background(), parked))
	parked.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	parked.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.AgentSessionConditionIdle, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived, LastTransitionTime: metav1.Now(),
		ObservedGeneration: parked.Generation, Message: "archived after long idle",
	}}
	require.NoError(t, cli.Status().Update(context.Background(), parked))

	az := &stubAuthz{checkResult: true}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{
		"default/parked": {{Index: 0, Role: "user"}},
	}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "user-1", Email: "alice@example.com"},
		ChannelKey:  "dm:user-1",
		MessageText: "still there?",
	})
	require.NoError(t, err, "Deliver")

	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "a swept session is parked, not finished")
	assert.Equal(t, "parked", dec.Session.Name, "routed into the SAME session")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "parked"}, &got))
	assert.Nil(t, got.Status.PendingRestart, "no fork-trigger written")
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"the operator is asked to respawn the runner")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1, "no child session materialized")
}

func TestE2EDenialFromDifferentUser(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-deny")

	keyHash := channelkey.LabelValue("dm:user-1")
	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "existing-deny",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c-deny",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
			// "A DIFFERENT user is denied" presupposes an original starter, and
			// the join-approval path addresses its approver from these
			// annotations. Without them the request has no addressable approver
			// and is refused outright, so the permission_request below would
			// never publish. A real human-started session always records these.
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID:  "user-1",
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:starter",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	require.NoError(t, cli.Create(context.Background(), existing))
	existing.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	require.NoError(t, cli.Status().Update(context.Background(), existing))

	// The interact check denies, so the inbound from a second user takes the
	// permission-deny path.
	az := &stubAuthz{checkResult: false}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	var denials []string
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "user-other"},
		ChannelKey:  "dm:user-1",
		MessageText: "intruder",
		Reply: channelkinds.InboundReplyHooks{
			Ephemeral: func(_ context.Context, msg string) error {
				denials = append(denials, msg)
				return nil
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome)
	assert.Empty(t, mem.appends, "denied path appended memory")
	// On first denial the pipeline publishes a permission_request envelope (its
	// sub-channel sender owns the in-thread rejection) and suppresses the notice,
	// which would otherwise double-post. See TestDeliverExistingSessionDenied.
	assert.Len(t, nc.subjects, 1, "denied path should publish 1 permission_request")
	assert.True(t, dec.Notice.IsSuppressed(), "the notice must be suppressed when a permission_request is published")
	_ = denials // suppress unused if hook isn't exercised by the pipeline
}

func TestE2EIdleWakeAnnotation(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-idle")

	keyHash := channelkey.LabelValue("dm:user-1")
	existing := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "existing-idle",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c-idle",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	require.NoError(t, cli.Create(context.Background(), existing))
	existing.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
	require.NoError(t, cli.Status().Update(context.Background(), existing))

	az := &stubAuthz{checkResult: true}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "user-1", Email: "alice@example.com"},
		ChannelKey:  "dm:user-1",
		MessageText: "wake up",
	})
	require.NoError(t, err)

	got := &spiceboxv1alpha1.AgentSession{}
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: "existing-idle",
	}, got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt], "wake annotation not patched")
}

func TestE2EAuthzWriteFailure(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-authzfail")

	az := &stubAuthz{startedByErr: errors.New("spicedb down")}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "user-1", Email: "alice@example.com"},
		ChannelKey:  "dm:user-1",
		MessageText: "hi",
	})
	require.Error(t, err, "expected error from authz write failure")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	// channelsd raises its own StartFailure signal rather than writing the
	// operator-owned failureReason; the operator (absent from this harness) is
	// what converts the signal into the Failed state.
	require.NotNil(t, sessions.Items[0].Status.StartFailure, "channelsd must raise the StartFailure signal on authz-write failure")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionAuthzWriteFail, sessions.Items[0].Status.StartFailure.Reason)

	// Keep startEmbeddedNATS and the NATS packages referenced: they are here for
	// tests that exercise the real relay path, and Go rejects unused imports.
	_ = fakekind.DriverFor
	_ = startEmbeddedNATS
	_ = natsclient.Connect
	_ = server.Options{}
}

// TestE2EInheritsFromArchivedSession pins channelsd's half of the
// continuation-inherit flow. A new inbound for an archived (Succeeded) session
// is a DispNewInheriting continuation: channelsd writes an inherit-mode
// PendingRestart trigger on the terminal parent and returns OutcomeForkPending
// rather than creating the child itself. The operator's fork reconciler
// materializes the inheriting child (owner-gated, denied-copying,
// full-transcript copy) — covered by
// TestReconcileRestart_InheritCopiesFullTranscriptAndDenied and
// TestReconcileRestart_InheritForkDenied_NonOwner in the agentsession package.
func TestE2EInheritsFromArchivedSession(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-inherit")

	keyHash := channelkey.LabelValue("dm:user-inherit")

	// Pre-existing archived session (Succeeded) for the same channelKey.
	archivedSess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "c1-old",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: "c-inherit",
				spiceboxv1alpha1.LabelChannelKey:  keyHash,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
	}
	require.NoError(t, cli.Create(context.Background(), archivedSess))
	archivedSess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	require.NoError(t, cli.Status().Update(context.Background(), archivedSess))

	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	az := &stubAuthz{checkResult: true}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	const newText = "new message after archived session"
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "user-inherit", Email: "inherit@example.com"},
		ChannelKey:  "dm:user-inherit",
		MessageText: newText,
	})
	require.NoError(t, err)

	// The continuation is fork-pending, not routed: channelsd hands the child
	// creation to the operator rather than creating a session itself.
	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)

	// No new AgentSession was created — only the archived parent still exists.
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1, "channelsd must not synchronously create the inheriting child")
	assert.Equal(t, "c1-old", sessions.Items[0].Name)

	// The parent carries an inherit-mode PendingRestart trigger the operator's
	// fork reconciler will consume — this is the whole contract of channelsd's
	// half; if this trigger stops being written, the continuation strands.
	var parent spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: "c1-old",
	}, &parent))
	require.NotNil(t, parent.Status.PendingRestart, "inherit fork-trigger not written on the terminal parent")
	assert.Equal(t, spiceboxv1alpha1.PendingRestartModeInherit, parent.Status.PendingRestart.Mode)
	assert.Equal(t, newText, parent.Status.PendingRestart.NewUserText,
		"the new message must ride into the fork trigger so the operator seeds it as the child's first turn")
	assert.NotEmpty(t, parent.Status.PendingRestart.TargetSessionName, "child session name must be stamped for crash-safe idempotency")

	// channelsd does not copy the transcript — the operator's fork reconciler
	// owns the copy. So the pipeline appends nothing on this path.
	assert.Empty(t, mem.appends, "channelsd must not copy the transcript on the inherit path")
}

// TestE2EAdoptsThreadWithPriorHistory exercises thread adoption end-to-end: the
// bot is @-mentioned inside a thread that already has conversation history and
// no prior AgentSession. The pipeline must backfill that history into the new
// session's prompt, auto-grant interact to the thread's participants, and mark
// the session mention_only — against real envtest k8s.
func TestE2EAdoptsThreadWithPriorHistory(t *testing.T) {
	te := testenv.Shared(t)
	cli := te.Client
	ch := makeChannel(t, cli, "default", "c-adopt")

	az := &stubAuthz{checkResult: true}
	mem := &stubMemory{seeded: map[string][]pipeline.MemTurn{}}
	nc := &stubNATSPub{}
	p := newPipeline(t, az, mem, nc, cli)

	// The conversation that existed in the thread BEFORE the bot was summoned.
	// The fake channel kind has no ConversationReader, so ReadHistory is injected
	// directly — the same seam internal/cmd/channelsd wires from the bound kind.
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, channelKey string, opts channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		assert.Equal(t, "thread:C1:100.0", channelKey)
		assert.Equal(t, "101.0", opts.BeforeTS, "backfill window bounded by the triggering message")
		return channelkinds.HistoryPage{
			HasMore: true,
			Messages: []channelkinds.HistoryMessage{
				{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "alice@example.com", Text: "should we use postgres?", TS: "100.1"},
				{AuthorExternalID: "U_B", AuthorDisplayName: "Bob", AuthorEmail: "bob@example.com", Text: "yes, we already run it", TS: "100.2"},
			},
		}, nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U_A", Email: "alice@example.com"},
		ChannelKey:  "thread:C1:100.0",
		MessageText: "@bot summarize this",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "100.0", "message_ts": "101.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.True(t, dec.NewSession, "a reply into a thread with no prior session creates one")
	assert.True(t, dec.Adopted, "a thread with prior history must be adopted")
	assert.ElementsMatch(t, []string{"Alice", "Bob"}, dec.GrantedParticipants)

	// The new AgentSession in envtest carries the adopted-thread shape.
	var newSess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{
		Namespace: "default", Name: dec.Session.Name,
	}, &newSess), "new session not found in k8s")
	require.NotNil(t, newSess.Spec.InputChannel)
	assert.Equal(t, "mention_only", newSess.Spec.InputChannel.RoutingMode)
	assert.Equal(t, "100.1", newSess.Annotations[spiceboxv1alpha1.AnnotationBackfilledFromTS])
	assert.Equal(t, "101.0", newSess.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS])

	// The backfilled transcript is folded into spec.Prompt — history
	// BEFORE the live mention — so the agent reads the thread before the
	// question it was summoned to answer.
	inline := newSess.Spec.Prompt.Inline
	assert.Contains(t, inline, "Alice: should we use postgres?")
	assert.Contains(t, inline, "Bob: yes, we already run it")
	assert.Contains(t, inline, "earlier messages omitted")
	assert.Contains(t, inline, "@bot summarize this")
	assert.Less(t,
		strings.Index(inline, "should we use postgres?"),
		strings.Index(inline, "@bot summarize this"),
		"thread history must precede the live mention in the prompt")

	// New-session adoption seeds the prompt, not memory.
	assert.Empty(t, mem.appends, "adoption must not append to memory on the new-session path")

	// Both thread participants were auto-granted interact.
	assert.Len(t, az.participantUsers, 2, "both thread authors must be granted interact")
}
