package pipeline

// Session-to-session delivery: agent_message_send, published by the SENDING
// session on its own subject and naming its destination in the payload.
//
// There is one arriving kind and one handler, and both directions of a
// conversation use them. Which end the SUBJECT names never varies — it is
// always the sender, so the sender is authenticated by the bus rather than
// claimed in the payload — while which end is the Channel's BOUND end does,
// and that is what resolvePairChannel's two arms exist for.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// agentIntegrityTestPriv/Pub is the single Ed25519 key every AgentSession
// fixture in this file anchors as its audit key, and every
// agentMessageSendEnvelope signature is produced with. One key for the whole
// file is sufficient: the verifier authenticates the SESSION (its own
// status.auditKeyID + UID), not the signer's identity, so a shared key just
// needs each session's own publisher string and UID to be right — which
// stampAuditKey and testSessionUID below hold constant.
var (
	agentIntegrityTestPriv = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x39}, ed25519.SeedSize))
	agentIntegrityTestPub  = agentIntegrityTestPriv.Public().(ed25519.PublicKey)
)

// testSessionUID derives the fixed K8s UID this file's fixtures give a
// session, from its name alone, so a fixture (boundSession, bareSession) and
// agentMessageSendEnvelope's signer can agree on it without threading a UID
// through every call site.
func testSessionUID(name string) string { return "uid-" + name }

// stampAuditKey anchors sess's status.auditPublicKey/auditKeyID to the
// shared test key and sets its UID from testSessionUID, so any envelope
// agentMessageSendEnvelope signs for this session's name verifies against
// it. HandleAgentMessageSend's envelope verifier now requires this on every
// AgentSession that acts as an agent_message_send SENDER — a fixture that
// skips it is refused as "no anchored audit key" before anything else in
// the handler runs.
func stampAuditKey(sess *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
	sess.UID = types.UID(testSessionUID(sess.Name))
	sess.Status.AuditPublicKey = base64.StdEncoding.EncodeToString(agentIntegrityTestPub)
	sess.Status.AuditKeyID = keyid.For(agentIntegrityTestPub)
	return sess
}

// bareSession returns a Running AgentSession with no channel binding and no
// pair Channel of its own — a sender whose only job in a test is to EXIST
// (with an anchored audit key), so the verifier's fresh K8s Get succeeds
// before pair resolution runs exactly as if the session had never been
// bound to anything, because it never was.
func bareSession(name string) *spiceboxv1alpha1.AgentSession {
	return stampAuditKey(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	})
}

// agentSubject renders the "agentsession:<ns>/<name>" SpiceDB subject that
// names one end of a conversation — the value Channel.spec.authzSubject
// carries. Every fixture in this file is in the "default" namespace.
func agentSubject(name string) string { return "agentsession:default/" + name }

// newAgentChannel builds a kind=agent Channel CR whose counterparty is the
// given "agentsession:" subject.
//
// That value is LOAD-BEARING, and the tests below turn on it: an agent Channel
// is the conversation between a PAIR, and resolvePairChannel accepts it only
// when its counterparty is the other end of the (target, sender) pair the
// message names. It is still never the acting subject — that is the session
// the arriving subject authorized — so a test that passes only because these
// two happen to be equal is asserting the wrong thing; assert on which SESSION
// was delivered into instead.
func newAgentChannel(name, counterparty string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "agent",
			AgentClass:     "ac1",
			AuthzSubject:   counterparty,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// newSlackChannel builds a kind=slack Channel CR — a real registered kind
// that, unlike newAgentChannel's "agent", does NOT implement
// channelkinds.SessionCounterparty. It takes an authzSubject because the
// security gate this file exercises is precisely "a Channel that names a
// session as its counterparty may still not CARRY that session's traffic
// unless its kind says so": pass "" for an ordinary human surface, and an
// "agentsession:" subject for the fixture that has to reach the gate.
func newSlackChannel(name, authzSubject string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			AgentClass:     "ac1",
			AuthzSubject:   authzSubject,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// boundSession returns a Running AgentSession bound, via both the correlation
// labels and Spec.InputChannel, to the named Channel + key.
//
// It carries the started-by annotations a delegated child inherits verbatim
// from its parent (pkg/controllers/subagentrequest's startedByAnnotations).
// They are load-bearing for the refusal tests, not decoration: they are what
// make approverIdentityFor yield an ADDRESSABLE approver, so the human
// join-request flow would genuinely publish a card and stamp a pending
// requester if a refused agent message were routed into it. Drop them and
// that flow bails out on its own for an unrelated reason, and the assertion
// that it was skipped stops discriminating.
func boundSession(t *testing.T, name, channelKind, channelName, key string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return stampAuditKey(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: channelName,
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest(key),
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U-LEAD",
				spiceboxv1alpha1.AnnotationStartedByEmail:      "lead@example.test",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: channelName, Kind: channelKind, Key: key,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	})
}

// delegationPair builds what ONE conversational delegation provisions
// (pkg/controllers/subagentrequest): a single `agent` Channel named
// "<child>-inbox", bound to the CHILD as its input channel and naming the
// PARENT on spec.authzSubject.
//
// The parent is deliberately NOT returned. That asymmetry is the whole point
// of these tests — the parent may be a Slack-bound root, a headless session
// with no binding at all, or a conversational child itself — so each test
// supplies the parent end it means to exercise.
func delegationPair(t *testing.T, parent, child string) (*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	name := child + "-inbox"
	return newAgentChannel(name, agentSubject(parent)),
		boundSession(t, child, "agent", name, "agent:"+child)
}

// agentMessageSendEnvelope builds what a sender publishes: an envelope whose
// SESSION is the sender (channelsd cross-checks that against the NATS subject
// before the handler runs) and whose payload names the destination.
//
// It is SIGNED, with the shared agentIntegrityTestPriv key and the sender's
// testSessionUID, because HandleAgentMessageSend's envelope verifier now runs
// fail-closed ahead of everything else in the handler: every test using this
// helper needs its sender session fixture to carry the matching audit key
// (stampAuditKey — boundSession and bareSession both do this) or the
// envelope is refused before any of the behavior a test means to exercise
// ever runs.
func agentMessageSendEnvelope(t *testing.T, senderNS, senderName, toNS, toName, text string) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(senderNS, senderName, channelevents.KindAgentMessageSend,
		channelevents.AgentMessageSendPayload{
			To:   channelevents.SessionRef{Namespace: toNS, Name: toName},
			Text: text,
		})
	require.NoError(t, err)
	signAgentEnvelope(t, senderNS, senderName, &env)
	return env
}

// signAgentEnvelope signs env as sender (senderNS, senderName), using the
// shared agentIntegrityTestPriv key and that sender's testSessionUID. Split
// out from agentMessageSendEnvelope for tests that need to build (or mutate)
// the envelope by hand BEFORE signing — signing must be the last mutation,
// per EnvelopeSigner.Sign's own contract, so a case that tampers with
// Payload after agentMessageSendEnvelope would invalidate its own signature
// rather than reach the decode step it means to exercise.
func signAgentEnvelope(t *testing.T, senderNS, senderName string, env *channelevents.Envelope) {
	t.Helper()
	signer, err := channelevents.NewEnvelopeSigner(agentIntegrityTestPriv,
		"session:"+senderNS+"/"+senderName, testSessionUID(senderName))
	require.NoError(t, err)
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix(senderNS, senderName), channelevents.KindAgentMessageSend)
	require.NoError(t, signer.Sign(subject, env))
}

// seedDelegation records one parent -> child delegation edge on the fake
// authorizer, standing in for the lineage tuple pair the SubagentRequest
// controller writes (spicedb.Client.TouchLineage). fakeAuthz.CheckConverse
// resolves `converse = parent + child` over these edges, so a test that omits
// this gets a genuine refusal — which is the point: without it, every
// assertion on this path would be answered by the blanket-true checkResult
// that hid the defect this whole gate exists to close.
func seedDelegation(t *testing.T, az *fakeAuthz, parent, child string) {
	t.Helper()
	az.delegations = append(az.delegations, delegationEdge{parent: parent, child: child})
}

// TestHandleAgentMessageSend_ParentToChildDeliversThroughThePairChannel covers
// the direction where the TARGET is the end the pair Channel is bound to: the
// child's own spec.inputChannel is that Channel, and its counterparty names
// the parent that is speaking.
//
// The parent here is a Slack-bound root — the shape that makes this direction
// interesting, since a root's own binding admits no session counterparty and
// so can never carry the reply itself.
//
// Both halves are asserted: the message is delivered, AND the check that
// authorized it was the agent-to-agent one. Before agentsession#converse
// existed this could never succeed — the acting subject reached SpiceDB as
// `user:agentsession:default/parent-1`, rejected as InvalidArgument — and
// every test on this path passed anyway, because the fake authorizer returned
// true for anything.
func TestHandleAgentMessageSend_ParentToChildDeliversThroughThePairChannel(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	rootCh := newSlackChannel("root-slack", "")
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
	p, az, mem, _, _ := newPipeline(t, ch, child, rootCh, root)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "use the staging cluster")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	assert.Equal(t, 1, az.converseCalls, "the agent-to-agent check must be the one consulted")
	assert.Zero(t, az.checkCalls, "the human interact check must not be consulted for a session subject")
	require.Len(t, mem.appends, 1, "the parent's message must be delivered to the child exactly once")
	assert.Equal(t, "child-1", mem.appends[0].name, "delivered into the CHILD, not back into the parent")
	require.NotEmpty(t, mem.appends[0].turn.Content)
	assert.Equal(t, "use the staging cluster", mem.appends[0].turn.Content[0].Text)
}

// TestHandleAgentMessageSend_ChildToHumanBoundRootParentDeliversThroughThePairChannel
// is the case an inbox-shaped resolution could not serve at all, and the
// reason this handler resolves a PAIR.
//
// The parent is a root bound to Slack. Its own inbound surface is therefore a
// kind that admits no "agentsession:" subject, so reading the target's own
// binding — as this used to — resolved the human's Channel and the
// authzSubject type gate refused the delivery, correctly and uselessly: a
// conversational child could never answer the session that delegated to it.
// Resolving the Channel bound to the SENDER, whose counterparty names the
// target, reaches the parent through the one Channel the delegation already
// has.
//
// The assertion that matters is WHICH session the turn landed on: the pair
// Channel's correlation labels are on the child, so a delivery that went
// through Deliver's label lookup instead of the explicit target would append
// to "child-1" — the sender feeding itself its own message.
func TestHandleAgentMessageSend_ChildToHumanBoundRootParentDeliversThroughThePairChannel(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	rootCh := newSlackChannel("root-slack", "")
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
	p, az, mem, _, _ := newPipeline(t, ch, child, rootCh, root)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster should I use?")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	assert.Equal(t, 1, az.converseCalls, "the agent-to-agent check must be the one consulted")
	assert.Zero(t, az.checkCalls, "the human interact check must not be consulted for a session subject")
	require.Len(t, mem.appends, 1, "the child's message must be delivered to the parent exactly once")
	assert.Equal(t, "parent-1", mem.appends[0].name,
		"the turn must land on the PARENT — the pair Channel's correlation labels name the child")
	require.NotEmpty(t, mem.appends[0].turn.Content)
	assert.Equal(t, "which cluster should I use?", mem.appends[0].turn.Content[0].Text)
}

// TestHandleAgentMessageSend_HeadlessParentWithNoBindingReceivesThroughThePairChannel
// proves the sender-bound arm does not depend on the target having a binding
// of its own at all. A kubectl-driven parent — created with no spec.inputChannel
// — is a session nothing could previously message; the pair Channel its own
// delegation created gives its child a way to answer it.
func TestHandleAgentMessageSend_HeadlessParentWithNoBindingReceivesThroughThePairChannel(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	headless := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-1", Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"}, // no InputChannel
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	p, az, mem, _, _ := newPipeline(t, ch, child, headless)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "done, 3 files changed")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	assert.Equal(t, 1, az.converseCalls)
	require.Len(t, mem.appends, 1)
	assert.Equal(t, "parent-1", mem.appends[0].name)
}

// TestHandleAgentMessageSend_AttributesToSendingSessionNotAHuman is the
// monotonicity guard: a session-to-session inbound must carry the SENDING
// session's own subject as the acting identity, and — the property the whole
// delegation design rests on — carry NO per-user identity at all. If a future
// change "improves" attribution by threading a human owner (or any external
// id/email) through this path, these assertions are what catch it: a child
// must never be able to act as its parent's human, and a parent's human must
// never follow its reply into a child.
//
// Both directions are covered, because a human identity is within reach from
// either end when the parent is a Slack-bound root: as the TARGET (the child
// answers upward, and the parent's Channel and started-by annotations are
// right there at the destination), and as the SENDER (the parent replies
// downward, carrying its own human's annotations).
func TestHandleAgentMessageSend_AttributesToSendingSessionNotAHuman(t *testing.T) {
	cases := []struct {
		name               string
		senderName, toName string
		wantAppendOn       string
	}{
		{
			name:       "child -> human-bound parent: no identity is picked up at the destination",
			senderName: "child-1", toName: "parent-1", wantAppendOn: "parent-1",
		},
		{
			name:       "human-bound parent -> child: the parent's own human does not follow the reply",
			senderName: "parent-1", toName: "child-1", wantAppendOn: "child-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, child := delegationPair(t, "parent-1", "child-1")
			rootCh := newSlackChannel("root-slack", "")
			root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
			p, az, mem, _, _ := newPipeline(t, ch, child, rootCh, root)
			seedDelegation(t, az, "default/parent-1", "default/child-1")

			env := agentMessageSendEnvelope(t, "default", tc.senderName, "default", tc.toName, "clarify please")
			require.NoError(t, p.HandleAgentMessageSend(context.Background(), env))

			require.Len(t, mem.appends, 1)
			assert.Equal(t, tc.wantAppendOn, mem.appends[0].name)
			// authorSubject() (pipeline.go) returns empty precisely when
			// ExternalIDs carries no ExternalID — the no-user-identity shape.
			// A non-empty Author here would mean a human (or synthetic-human)
			// identity leaked in.
			assert.Empty(t, string(mem.appends[0].turn.Author),
				"monotonicity guard: a session-to-session inbound must carry NO human identity")
			assert.Equal(t, 1, az.converseCalls,
				"the pair was authorized as a pair; a blanket allow would not have consulted converse at all")
		})
	}
}

// TestHandleAgentMessageSend_MalformedEnvelopeDroppedNotFatal proves a bad
// publish is refused with a descriptive error rather than silently accepted
// or causing a panic — the property that matters for a wildcard subscription
// serving every session in the cluster: one bad publisher must not be able to
// take the whole thing down, and channelsd's envelopeHandler wrapper (which
// this handler is always reached through in production) logs the returned
// error and keeps serving the next message.
//
// There is deliberately no case for a malformed SENDER. The sender is
// env.Session, cross-checked against the NATS subject before the handler is
// reached, so there is no publisher-written sender string left to be
// malformed — which is the whole reason this is the only arriving shape.
func TestHandleAgentMessageSend_MalformedEnvelopeDroppedNotFatal(t *testing.T) {
	cases := []struct {
		name    string
		env     func(t *testing.T) channelevents.Envelope
		wantErr string
	}{
		{
			name: "the wrong kind: refused before anything is decoded",
			env: func(t *testing.T) channelevents.Envelope {
				env, err := channelevents.BuildEnvelope("default", "parent-1",
					channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "hi"})
				require.NoError(t, err)
				return env
			},
			wantErr: "unexpected kind",
		},
		{
			name: "empty text",
			env: func(t *testing.T) channelevents.Envelope {
				return agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "")
			},
			wantErr: "empty text",
		},
		{
			name: "no destination: refused rather than delivered to nobody",
			env: func(t *testing.T) channelevents.Envelope {
				return agentMessageSendEnvelope(t, "default", "parent-1", "", "", "hello")
			},
			wantErr: "no destination",
		},
		{
			name: "a destination with a namespace but no name",
			env: func(t *testing.T) channelevents.Envelope {
				return agentMessageSendEnvelope(t, "default", "parent-1", "default", "", "hello")
			},
			wantErr: "no destination",
		},
		{
			name: "malformed payload JSON",
			env: func(t *testing.T) channelevents.Envelope {
				// Payload must be tampered with BEFORE signing — signing after
				// (as agentMessageSendEnvelope does) would make the mutation
				// invalidate the signature instead of reaching the decode step
				// this case means to exercise.
				env, err := channelevents.BuildEnvelope("default", "parent-1", channelevents.KindAgentMessageSend,
					channelevents.AgentMessageSendPayload{To: channelevents.SessionRef{Namespace: "default", Name: "child-1"}, Text: "hello"})
				require.NoError(t, err)
				env.Payload = []byte(`{"text": not-json}`)
				signAgentEnvelope(t, "default", "parent-1", &env)
				return env
			},
			wantErr: "decode payload",
		},
		{
			// The arrival-side half of the agent Sender's own self-Send guard:
			// a publish that bypassed the Sender must not be able to feed a
			// session its own message.
			name: "a session addressing itself: refused as the self-feeding loop it is",
			env: func(t *testing.T) channelevents.Envelope {
				return agentMessageSendEnvelope(t, "default", "child-1", "default", "child-1", "hello")
			},
			wantErr: "addressed itself",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, child := delegationPair(t, "parent-1", "child-1")
			rootCh := newSlackChannel("root-slack", "")
			root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
			p, _, mem, _, _ := newPipeline(t, ch, child, rootCh, root)
			err := p.HandleAgentMessageSend(context.Background(), tc.env(t))
			require.Error(t, err, "a malformed envelope must be refused, not silently accepted")
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, mem.appends, "a refused envelope must not be delivered")
		})
	}
}

// TestHandleAgentMessageSend_RefusedEnvelopeRaisesIntegrityMonitoring proves
// the envelope verifier's refusal is not JUST an error return: it also
// raises a MonitoringEvent on the "AgentMessageIntegrity" condition, so an
// operator watching cluster monitoring — not just grepping channelsd's own
// logs for the returned error — can see a forged, unsigned, or replayed
// agent_message_send being refused. AGENTS.md's no-silent-errors rule names
// this as one of the three required surfaces alongside the log line and the
// returned error; this pins the monitoring one specifically, since the other
// two are already covered by every other refusal test in this file
// asserting on the returned error.
func TestHandleAgentMessageSend_RefusedEnvelopeRaisesIntegrityMonitoring(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
	p, _, mem, nats, _ := newPipeline(t, ch, child, root)

	// Unsigned — built directly rather than via agentMessageSendEnvelope
	// (which always signs) — is the simplest refusal shape to trigger.
	env, err := channelevents.BuildEnvelope("default", "parent-1", channelevents.KindAgentMessageSend,
		channelevents.AgentMessageSendPayload{To: channelevents.SessionRef{Namespace: "default", Name: "child-1"}, Text: "hello"})
	require.NoError(t, err)

	err = p.HandleAgentMessageSend(context.Background(), env)
	require.Error(t, err, "an unsigned envelope must be refused")
	assert.Contains(t, err.Error(), "envelope refused")
	assert.Empty(t, mem.appends, "a refused envelope must never be delivered")

	require.Contains(t, nats.subjects, channelevents.MonitoringEventSubject,
		"a verification refusal must raise a monitoring event, not just an error return")
	var found bool
	for i, subj := range nats.subjects {
		if subj != channelevents.MonitoringEventSubject {
			continue
		}
		var mev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(nats.payloads[i], &mev))
		if mev.Condition != "AgentMessageIntegrity" || mev.Reason != "EnvelopeVerificationFailed" {
			continue
		}
		found = true
		assert.Equal(t, channelevents.MonitoringLevelWarning, mev.Level)
		assert.Equal(t, "AgentSession", mev.Source.Kind)
		assert.Equal(t, "default", mev.Source.Namespace)
		assert.Equal(t, "parent-1", mev.Source.Name, "must name the SENDING session, not the target")
		assert.Contains(t, mev.Summary, "envelope is unsigned")
	}
	assert.True(t, found, "expected an AgentMessageIntegrity/EnvelopeVerificationFailed monitoring event")
}

// TestHandleAgentMessageSend_RefusedWhenNoChannelJoinsThePair is the
// assertion that makes the resolution a PAIR resolution rather than "some
// agent Channel involving the target". Each case sends from a session that no
// K8s-witnessed Channel joins to the destination, and each is refused before
// any authorization check runs at all.
//
// The first three cases deliberately SEED a delegation edge that would make
// agentsession#converse answer yes. That is what makes them discriminating:
// drop either counterparty comparison from resolvePairChannel and the message
// resolves a Channel, passes the seeded check, and lands in the target's
// transcript — so `converseCalls` is asserted, not just the refusal. A test
// that only checked "not delivered" would pass for the wrong reason, because
// the permission would have refused these anyway in most shapes.
func TestHandleAgentMessageSend_RefusedWhenNoChannelJoinsThePair(t *testing.T) {
	cases := []struct {
		name string
		// objs are the sender-side fixtures; the destination (child-1) and its
		// pair Channel to parent-1 are always present.
		objs   func(t *testing.T) []client.Object
		sender string
		// seedEdge, when set, is a delegation edge written to the fake
		// authorizer so converse WOULD allow this sender.
		seedEdge bool
	}{
		{
			name: "a stranger bound to its own agent Channel with a different counterparty: refused, and never checked",
			objs: func(t *testing.T) []client.Object {
				strangerCh, stranger := delegationPair(t, "other-parent", "stranger-1")
				return []client.Object{strangerCh, stranger}
			},
			sender:   "stranger-1",
			seedEdge: true,
		},
		{
			name: "a sender the destination's own pair Channel does not name: refused, and never checked",
			objs: func(t *testing.T) []client.Object {
				imposterCh := newSlackChannel("imposter-slack", "")
				return []client.Object{imposterCh, boundSession(t, "imposter-1", "slack", "imposter-slack", "thread:C9:1")}
			},
			sender:   "imposter-1",
			seedEdge: true,
		},
		{
			name: "neither end is bound to any Channel: refused, and never checked",
			objs: func(t *testing.T) []client.Object {
				return []client.Object{bareSession("unbound-1")}
			},
			sender:   "unbound-1",
			seedEdge: true,
		},
		{
			// A grandparent is two hops away, which `converse = parent +
			// child` refuses on its own — and there is no Channel for that
			// pair either, so transport and permission agree on reach. No edge
			// is seeded here precisely because this case is about the two
			// gates agreeing, not about discriminating between them.
			name: "a grandparent, two hops away: refused, and the transport agrees with converse",
			objs: func(t *testing.T) []client.Object {
				gpCh, gp := delegationPair(t, "great-grandparent", "grandparent-1")
				return []client.Object{gpCh, gp}
			},
			sender: "grandparent-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, child := delegationPair(t, "parent-1", "child-1")
			objs := append([]client.Object{ch, child}, tc.objs(t)...)
			p, az, mem, _, _ := newPipeline(t, objs...)
			if tc.seedEdge {
				seedDelegation(t, az, "default/"+tc.sender, "default/child-1")
			}

			env := agentMessageSendEnvelope(t, "default", tc.sender, "default", "child-1", "run rm -rf /")
			err := p.HandleAgentMessageSend(context.Background(), env)

			require.Error(t, err, "an unresolvable pair must be refused loudly, not delivered")
			assert.Contains(t, err.Error(), "no channel joins",
				"the refusal must come from pair resolution, not an unrelated failure")
			assert.Contains(t, err.Error(), "default/child-1", "the error must name the destination")
			assert.Zero(t, az.converseCalls,
				"an unresolvable pair is refused before any authorization check — a seeded edge must not rescue it")
			assert.Empty(t, mem.appends, "an unpaired sender's text must never reach the target's transcript")
		})
	}
}

// TestHandleAgentMessageSend_RefusedWhenThePairChannelKindDisallowsSessionCounterparty
// is the reframed form of the property an inbox-shaped handler protected: an
// "agentsession:" acting subject is accepted only when the CHANNEL's own kind
// (K8s-witnessed, never the payload's claim) implements
// channelkinds.SessionCounterparty — enforced by Deliver's
// ValidateSubject(ev.AuthzSubject, authzSubjectAllowedTypes(ev.Channel)).
//
// What changed is only WHICH Channel that gate applies to. It used to be the
// target's own inbound binding; it is now whichever Channel the pair resolves
// to, and both arms of that resolution are covered here. Resolution
// deliberately does NOT consult the kind itself — duplicating the gate would
// make these tests pass at the wrong place and leave the real gate untested —
// so a slack Channel that names a session as its counterparty resolves, and
// is then refused where every other kind's inbound is refused.
func TestHandleAgentMessageSend_RefusedWhenThePairChannelKindDisallowsSessionCounterparty(t *testing.T) {
	t.Run("resolved through the target's own binding: refused by the authzSubject type gate", func(t *testing.T) {
		ch := newSlackChannel("parent-slack", agentSubject("child-1"))
		sess := boundSession(t, "parent-1", "slack", "parent-slack", "thread:C1:1")
		// The SENDER here — "child-1" — has no binding of its own in this
		// direction (the gate under test is the target's), but the verifier
		// still requires it to exist with an anchored audit key before
		// anything else in the handler runs.
		p, az, mem, _, _ := newPipeline(t, ch, sess, bareSession("child-1"))
		seedDelegation(t, az, "default/parent-1", "default/child-1")

		env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "clarify please")
		err := p.HandleAgentMessageSend(context.Background(), env)

		require.Error(t, err, "an agentsession: acting subject must be refused for a Channel kind that does not allow a session counterparty")
		assert.Contains(t, err.Error(), "not permitted here",
			"the refusal must come from the authzSubject type gate (ValidateSubject), not an unrelated failure")
		assert.Zero(t, az.converseCalls, "the type gate runs before the permission check")
		assert.Empty(t, mem.appends, "a refused agentsession: subject must never be delivered")
	})

	t.Run("resolved through the sender's binding: refused by the same gate", func(t *testing.T) {
		rootCh := newSlackChannel("root-slack", "")
		root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
		// The sender's own binding names the target as its counterparty, so
		// the sender-bound arm resolves it — and its kind still may not carry
		// the traffic.
		senderCh := newSlackChannel("child-slack", agentSubject("parent-1"))
		sender := boundSession(t, "child-1", "slack", "child-slack", "thread:C2:1")
		p, az, mem, _, _ := newPipeline(t, rootCh, root, senderCh, sender)
		seedDelegation(t, az, "default/parent-1", "default/child-1")

		env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster?")
		err := p.HandleAgentMessageSend(context.Background(), env)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "not permitted here",
			"the refusal must come from the authzSubject type gate (ValidateSubject), not an unrelated failure")
		assert.Zero(t, az.converseCalls, "the type gate runs before the permission check")
		assert.Empty(t, mem.appends)
	})
}

// TestHandleAgentMessageSend_PairChannelWithoutLineageIsDeniedAndRaisesNoJoinRequest
// pins that a resolvable pair Channel is NOT authorization. The transport is
// willing here — the Channel joins these two ends exactly as the resolver
// requires — and SpiceDB refuses because no delegation tuple was ever written
// (a rolled-back delegation, or a Channel that outlived its lineage). Without
// this, "the pair resolved" and "the sender may speak" would be one fact, and
// the permission could be deleted with every test still green.
//
// It also pins where the refusal must NOT go. handlePermissionDeny is the
// human join-request flow, and every one of its outputs is wrong for a sender
// that is a process: it dedups on status.pendingRequesters keyed by an
// external id an agent message does not have, and — since an agent-channel
// inbound carries no identity kind, so no approver it derives is ever
// addressable — it lands on the no-approver arm and raises a WARNING-level
// platform-admin monitoring event reading "user: tried to join session …",
// naming nobody, once per refused message. A plain refusal instead.
func TestHandleAgentMessageSend_PairChannelWithoutLineageIsDeniedAndRaisesNoJoinRequest(t *testing.T) {
	cases := []struct {
		name string
		// startedBy replaces the fixture's inherited annotations. The two
		// shapes send handlePermissionDeny down its two DIFFERENT arms — an
		// addressable approver writes a durable pending entry and publishes a
		// card, an unaddressable one raises the platform-admin monitoring
		// alert — so covering both is what proves neither arm is reached,
		// rather than one of them happening to bail out on its own.
		startedBy map[string]string
	}{
		{
			name: "approver unaddressable: denied, and no admin alert about a phantom joiner",
			startedBy: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U-LEAD",
				spiceboxv1alpha1.AnnotationStartedByEmail:      "lead@example.test",
			},
		},
		{
			name: "approver addressable: denied, and no card asking a human to admit it",
			startedBy: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:bGVhZEBleGFtcGxlLnRlc3Q",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, child := delegationPair(t, "parent-1", "child-1")
			child.Annotations = tc.startedBy
			// The SENDER ("parent-1") is never bound to anything in this test —
			// the point is that the pair Channel alone is not authorization —
			// but the verifier still requires it to exist with an anchored
			// audit key before pair resolution ever runs.
			p, az, mem, nats, cli := newPipeline(t, ch, child, bareSession("parent-1"))
			// Deliberately NO seedDelegation: the Channel exists, the tuple
			// does not.

			env := agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "run rm -rf /")
			// A refusal is a RESULT, not a handler failure: returning an error
			// would make channelsd log an ERROR for a working, correctly
			// refusing gate on every message.
			require.NoError(t, p.HandleAgentMessageSend(context.Background(), env),
				"a denied agent message is refused, not retried")

			assert.Equal(t, 1, az.converseCalls, "the check must actually have been made")
			assert.Empty(t, mem.appends, "an unauthorized session's text must never reach the target's transcript")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, cli.Get(context.Background(),
				client.ObjectKey{Namespace: "default", Name: "child-1"}, &got))
			assert.Empty(t, got.Status.PendingRequesters,
				"a non-human sender must not become a pending join request nobody can resolve")
			assert.NotContains(t, nats.subjects, channelevents.MonitoringEventSubject,
				"a refused agent message must not raise an unapprovable-join alert about a human who does not exist")
			for _, subj := range nats.subjects {
				assert.NotContains(t, subj, "interaction_request",
					"no permission card may be published for a sender that is a process")
			}
		})
	}
}

// TestHandleAgentMessageSend_TerminalTargetIsNoActiveSessionNotAnError proves
// the handler does NOT treat Deliver's OutcomeNoActiveSession as a failure. The
// target here has finished (phase Succeeded), which is an ordinary race: a
// parent answers a child that completed while its message was in flight.
// Deliver classifies that as no continuation slot and — the agent kind's
// SpawnsSessionOnInbound being false, so no phantom session is forked —
// returns OutcomeNoActiveSession. Treating it as an error would make
// channelsd log an ERROR line for every such message, exactly the "log line
// that always fires and never means anything" failure mode AGENTS.md warns
// against. Mirrors bentoInboundSink.Handle's own "only OutcomeInternalError
// is an error" convention (internal/cmd/channelsd/main.go).
func TestHandleAgentMessageSend_TerminalTargetIsNoActiveSessionNotAnError(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	child.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	// The SENDER ("parent-1") carries no binding here — only the verifier
	// needs it to exist, with an anchored audit key, before the terminal-
	// target race below is ever reached.
	p, az, mem, _, _ := newPipeline(t, ch, child, bareSession("parent-1"))
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "parent-1", "default", "child-1", "hello")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env),
		"OutcomeNoActiveSession must not be surfaced as a handler error")
	assert.Empty(t, mem.appends, "a finished session is not a runner destination")
}

// TestHandleAgentMessageSend_GarbageCollectedTargetIsQuietWhenTheSenderStillHoldsThePair
// covers the same race as the terminal case above, one step further along: the
// parent has not merely finished, it is GONE, and the child is answering it.
//
// The pair Channel is bound to the CHILD, so the conversation is still fully
// witnessed and the message routes as far as Deliver — which finds no session
// to deliver into and says so once, at INFO. This handler must not have a
// second opinion about that: refusing a missing target outright would make a
// routine condition an ERROR line per message on a path where nothing can be
// done about it. correlateSessions is the one place that decides.
func TestHandleAgentMessageSend_GarbageCollectedTargetIsQuietWhenTheSenderStillHoldsThePair(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	// The parent is deliberately not seeded: it has been collected.
	p, az, mem, _, _ := newPipeline(t, ch, child)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "done, 3 files changed")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env),
		"a child answering a collected parent is ordinary, not a handler failure")
	assert.Empty(t, mem.appends, "there is no session to deliver into")
	// The lineage is seeded, so a quiet outcome here cannot be a refusal
	// wearing a quiet face: nothing was asked, because there was nothing to
	// ask about.
	assert.Zero(t, az.converseCalls, "no session existed to authorize a turn into")
}

// TestHandleAgentMessageSend_NeitherEndWitnessesThePairIsALoudError is the
// other half of the decision above: giving up the refusal for a missing TARGET
// must not give up the refusal for a pair that has no Channel joining it at
// all.
//
// The target ("ghost-1") is absent entirely — nothing in the cluster names
// it. The sender ("nobody-1") does exist, as bareSession's bare minimum: the
// envelope verifier now authenticates every sender before pair resolution
// even runs, so a sender that is not a real, anchored session can never
// reach this refusal at all — it is caught earlier, and more specifically,
// by the verifier itself (see the envelope_verify_test.go table). What this
// test still proves is that an EXISTING sender with no Channel of its own,
// naming a destination that also has no Channel, gets the same loud,
// both-ends-named refusal a delegation whose Channel was rolled back would.
func TestHandleAgentMessageSend_NeitherEndWitnessesThePairIsALoudError(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	p, _, mem, _, _ := newPipeline(t, ch, child, bareSession("nobody-1"))

	env := agentMessageSendEnvelope(t, "default", "nobody-1", "default", "ghost-1", "hello")
	err := p.HandleAgentMessageSend(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no channel joins session default/ghost-1 and claimed sender default/nobody-1",
		"the refusal must name BOTH ends of the pair it could not resolve")
	assert.Empty(t, mem.appends)
}

// TestHandleAgentMessageSend_DanglingBindingOnTheTargetStillReachableThroughTheSender
// covers a target whose spec.inputChannel names a Channel that no longer
// exists — a human Slack Channel deleted while sessions still reference it.
//
// The pair Channel is on the OTHER end and is perfectly healthy, so the child
// can still reach its parent. Ending the search on the target's broken binding
// made that parent unreachable by its own child, which is a strictly worse
// outcome than a log line about the dangling binding.
func TestHandleAgentMessageSend_DanglingBindingOnTheTargetStillReachableThroughTheSender(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	// The parent's binding survives; the Channel it names does not (no
	// "root-slack" object is seeded).
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")
	p, az, mem, _, _ := newPipeline(t, ch, child, root)
	seedDelegation(t, az, "default/parent-1", "default/child-1")

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster should I use?")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env), "HandleAgentMessageSend")

	require.Len(t, mem.appends, 1, "the sender's healthy pair Channel must still carry the message")
	assert.Equal(t, "parent-1", mem.appends[0].name, "delivered into the parent")
	assert.Equal(t, 1, az.converseCalls,
		"and it went through the agent-to-agent gate, not around it")
}

// TestHandleAgentMessageSend_TransientLookupFailureIsNotReportedAsARefusal pins
// the distinction the "no channel joins these two" text used to erase: a
// definitive refusal and a retryable apiserver fault are different findings,
// and reporting the second in the shape of the first sends whoever reads the
// log looking for a broken delegation that is not broken.
//
// The blipping Get is keyed on the SENDER's AgentSession ("child-1"), which
// the envelope verifier now reads first — before pairChannelFrom ever gets a
// chance to fail the same way — so the fault surfaces as the verifier's own
// "load sending session" error rather than pairChannelFrom's "get session
// ... while resolving its conversation with ...". Both are equally NOT the
// refusal shape ("no channel joins"), which is the property this test
// actually pins; which layer names the failing lookup is incidental to which
// one runs first.
func TestHandleAgentMessageSend_TransientLookupFailureIsNotReportedAsARefusal(t *testing.T) {
	ch, child := delegationPair(t, "parent-1", "child-1")
	root := boundSession(t, "parent-1", "slack", "root-slack", "thread:C1:1")

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add scheme")
	require.NoError(t, corev1.AddToScheme(scheme), "add corev1")
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(withHealthyAgentClass(t, []client.Object{ch, child, root})...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// Only the SENDER's read blips — now hit first by the envelope
				// verifier's own Get, before pairChannelFrom's target arm ever
				// runs (it would miss cleanly anyway: its binding names
				// "root-slack", deliberately not seeded).
				if _, isSession := obj.(*spiceboxv1alpha1.AgentSession); isSession && key.Name == "child-1" {
					return apierrors.NewServiceUnavailable("apiserver having a moment")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	p, _, mem, _ := newPipelineOn(t, cli)

	env := agentMessageSendEnvelope(t, "default", "child-1", "default", "parent-1", "which cluster?")
	err := p.HandleAgentMessageSend(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load sending session", "the error must name the lookup that failed")
	assert.NotContains(t, err.Error(), "no channel joins",
		"a retryable fault must not wear the shape of a definitive refusal")
	assert.Empty(t, mem.appends)
}

// TestHandleAgentMessageSend_AnySessionNoHardcodedName proves the same handler
// delivers correctly for an ARBITRARY session name — nothing in
// HandleAgentMessageSend or its wiring may hardcode a session identity, since
// the row it is bound to in internal/cmd/channelsd/main.go subscribes with
// channelevents.AnySessionPrefix(), a wildcard across every session.
func TestHandleAgentMessageSend_AnySessionNoHardcodedName(t *testing.T) {
	ch, child := delegationPair(t, "totally-different-parent-9000", "totally-different-child-9000")
	p, az, mem, _, _ := newPipeline(t, ch, child, bareSession("totally-different-parent-9000"))
	seedDelegation(t, az, "default/totally-different-parent-9000", "default/totally-different-child-9000")

	env := agentMessageSendEnvelope(t, "default", "totally-different-parent-9000",
		"default", "totally-different-child-9000", "hi")
	require.NoError(t, p.HandleAgentMessageSend(context.Background(), env))
	require.Len(t, mem.appends, 1)
	assert.Equal(t, "totally-different-child-9000", mem.appends[0].name)
}
