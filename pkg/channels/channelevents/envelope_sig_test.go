package channelevents

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSigner(t *testing.T) (*EnvelopeSigner, ed25519.PublicKey) {
	t.Helper()
	seed := bytes.Repeat([]byte{0x11}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	s, err := NewEnvelopeSigner(priv, "session:demo-ns/root-1", "uid-123")
	require.NoError(t, err)
	return s, priv.Public().(ed25519.PublicKey)
}

func TestEnvelopeSign_RoundTripVerifies(t *testing.T) {
	s, pub := testSigner(t)
	env, err := BuildEnvelope("demo-ns", "root-1", KindAgentMessageSend,
		AgentMessageSendPayload{To: SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
	require.NoError(t, err)
	subject := SubjectIn(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend)
	require.NoError(t, s.Sign(subject, &env))

	assert.Equal(t, "session:demo-ns/root-1", env.Publisher)
	assert.Equal(t, s.KeyID(), env.SigKeyID)
	assert.NotEmpty(t, env.SigEpoch)
	assert.Equal(t, uint64(1), env.SigSeq)
	assert.Equal(t, "uid-123", env.SessionUID, "signer stamps SessionUID when empty")

	// Survives a wire round-trip (RawMessage payload bytes preserved verbatim).
	wire, err := json.Marshal(env)
	require.NoError(t, err)
	var back Envelope
	require.NoError(t, json.Unmarshal(wire, &back))
	assert.NoError(t, VerifyEnvelopeSig(subject, back, pub))
}

func TestEnvelopeSign_SeqIncrementsPerSign(t *testing.T) {
	s, _ := testSigner(t)
	for i := uint64(1); i <= 3; i++ {
		env, err := BuildEnvelope("demo-ns", "root-1", KindNotification, NotificationPayload{Text: "x"})
		require.NoError(t, err)
		require.NoError(t, s.Sign("subj", &env))
		assert.Equal(t, i, env.SigSeq)
	}
}

func TestEnvelopeSign_NilSignerIsNoOp(t *testing.T) {
	var s *EnvelopeSigner
	env, err := BuildEnvelope("demo-ns", "root-1", KindNotification, NotificationPayload{Text: "x"})
	require.NoError(t, err)
	require.NoError(t, s.Sign("subj", &env))
	assert.Empty(t, env.Sig)
	assert.Empty(t, env.Publisher)
}

func TestVerifyEnvelopeSig_TamperCases(t *testing.T) {
	subject := SubjectIn(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend)
	cases := []struct {
		name   string
		mutate func(env *Envelope, subj *string)
	}{
		{"unsigned envelope: ErrEnvelopeUnsigned", func(env *Envelope, _ *string) { env.Sig, env.Publisher, env.SigKeyID = "", "", "" }},
		{"subject swapped to out-direction fails", func(_ *Envelope, subj *string) {
			*subj = SubjectOut(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend)
		}},
		{"payload byte tampered fails", func(env *Envelope, _ *string) {
			env.Payload = json.RawMessage(`{"to":{"namespace":"demo-ns","name":"evil"},"text":"hi"}`)
		}},
		{"kind tampered fails", func(env *Envelope, _ *string) { env.Kind = KindNotification }},
		{"session tampered fails", func(env *Envelope, _ *string) { env.Session.Name = "other" }},
		{"sessionUID tampered fails", func(env *Envelope, _ *string) { env.SessionUID = "uid-999" }},
		{"publishedAt tampered fails", func(env *Envelope, _ *string) { env.PublishedAt = env.PublishedAt.Add(time.Hour) }},
		{"seq tampered fails", func(env *Envelope, _ *string) { env.Seq = 99 }},
		{"sigSeq tampered fails", func(env *Envelope, _ *string) { env.SigSeq = 99 }},
		{"sigEpoch tampered fails", func(env *Envelope, _ *string) { env.SigEpoch = "ffffffffffffffff" }},
		{"publisher tampered fails", func(env *Envelope, _ *string) { env.Publisher = "session:demo-ns/other" }},
		{"sig bytes corrupted fails", func(env *Envelope, _ *string) {
			env.Sig = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, pub := testSigner(t)
			env, err := BuildEnvelope("demo-ns", "root-1", KindAgentMessageSend,
				AgentMessageSendPayload{To: SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
			require.NoError(t, err)
			require.NoError(t, s.Sign(subject, &env))
			subj := subject
			tc.mutate(&env, &subj)
			assert.Error(t, VerifyEnvelopeSig(subj, env, pub))
		})
	}
}

func TestVerifyEnvelopeSig_WrongKeyFails(t *testing.T) {
	s, _ := testSigner(t)
	otherPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x22}, ed25519.SeedSize))
	env, err := BuildEnvelope("demo-ns", "root-1", KindNotification, NotificationPayload{Text: "x"})
	require.NoError(t, err)
	require.NoError(t, s.Sign("subj", &env))
	assert.Error(t, VerifyEnvelopeSig("subj", env, otherPriv.Public().(ed25519.PublicKey)))
}

func TestSignerPublishIn_PublishesVerifiableEnvelope(t *testing.T) {
	s, pub := testSigner(t)
	var gotSubject string
	var got Envelope
	publish := func(subject string, data []byte) error {
		gotSubject = subject
		return json.Unmarshal(data, &got)
	}
	err := s.PublishIn(publish, "demo-ns", "root-1", KindAgentMessageSend,
		AgentMessageSendPayload{To: SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
	require.NoError(t, err)
	assert.Equal(t, SubjectIn(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend), gotSubject)
	assert.NoError(t, VerifyEnvelopeSig(gotSubject, got, pub))
}

func TestSignerPublishOutSeq_SignsAfterSeqAndUIDStamp(t *testing.T) {
	s, pub := testSigner(t)
	var gotSubject string
	var got Envelope
	publish := func(subject string, data []byte) error {
		gotSubject = subject
		return json.Unmarshal(data, &got)
	}
	err := s.PublishOutSeq(publish, "demo-ns", "root-1", KindNotification,
		NotificationPayload{Text: "hi"}, PackSeq(2, 1), "uid-123")
	require.NoError(t, err)
	assert.Equal(t, PackSeq(2, 1), got.Seq)
	assert.Equal(t, "uid-123", got.SessionUID)
	// The signature must cover the stamped Seq: verification passes as-is...
	require.NoError(t, VerifyEnvelopeSig(gotSubject, got, pub))
	// ...and fails if Seq is altered after the fact.
	got.Seq = PackSeq(3, 1)
	assert.Error(t, VerifyEnvelopeSig(gotSubject, got, pub))
}

func TestSignerRequestIn_SignsVerifiableEnvelope(t *testing.T) {
	s, pub := testSigner(t)
	var gotSubject string
	var got Envelope
	req := func(subject string, data []byte, _ time.Duration) ([]byte, error) {
		gotSubject = subject
		if err := json.Unmarshal(data, &got); err != nil {
			return nil, err
		}
		return []byte(`{"ok":true}`), nil
	}
	reply, err := s.RequestIn(req, "demo-ns", "root-1", KindAgentMessageSend,
		AgentMessageSendPayload{To: SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"}, time.Second)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"ok":true}`), reply)
	// The request went out on the session's IN subject...
	assert.Equal(t, SubjectIn(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend), gotSubject)
	// ...and the signature verifies against that same subject: the request
	// func received exactly the subject the envelope was signed for.
	assert.NoError(t, VerifyEnvelopeSig(gotSubject, got, pub))
	// A different subject fails: the signature is bound to the request subject.
	otherSubject := SubjectOut(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend)
	assert.Error(t, VerifyEnvelopeSig(otherSubject, got, pub))
}

// TestSignerPublishIn_ConcurrentCallsStayOrdered drives N goroutines
// publishing on the SAME EnvelopeSigner and subject concurrently, then
// asserts SigSeq is strictly increasing in the order envelopes actually hit
// the wire (arrival order, captured inside the fake publish func).
//
// Sign's seq assignment (s.seq.Add(1)) and the actual publish call are two
// separate steps; without serializing Sign→marshal→publish as a unit, two
// goroutines can be scheduled so the one that got the LOWER seq loses the
// race to publish and arrives second. A verifier enforcing strictly-
// increasing SigSeq per subject (pkg/channels/channelsd/pipeline/envelope_verify.go)
// then refuses that later-arriving, lower-seq envelope as a replay — dropping
// a legitimate message. This is exactly the shape two concurrent
// reply_to_subagent tool calls in one runner turn can produce, since tool
// dispatch is per-goroutine.
//
// Run with -race -count=20: without EnvelopeSigner.publishMu serializing the
// three signer publish methods, this flakes/fails; with it, arrival order and
// seq order can never diverge, so it passes deterministically every time.
func TestSignerPublishIn_ConcurrentCallsStayOrdered(t *testing.T) {
	s, pub := testSigner(t)

	const n = 8
	var mu sync.Mutex
	var arrival []Envelope

	publish := func(_ string, data []byte) error {
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return err
		}
		mu.Lock()
		arrival = append(arrival, env)
		mu.Unlock()
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			err := s.PublishIn(publish, "demo-ns", "root-1", KindAgentMessageSend,
				AgentMessageSendPayload{To: SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	require.Len(t, arrival, n, "every goroutine's publish must have landed")
	subject := SubjectIn(SubjectPrefix("demo-ns", "root-1"), KindAgentMessageSend)
	for i, env := range arrival {
		require.NoError(t, VerifyEnvelopeSig(subject, env, pub), "arrival[%d] must verify", i)
		if i > 0 {
			assert.Greater(t, env.SigSeq, arrival[i-1].SigSeq,
				"arrival order must match strictly-increasing SigSeq order — a subject-scoped "+
					"replay verifier refuses any envelope whose SigSeq did not strictly increase "+
					"in arrival order")
		}
	}
}

func TestPackageLevelPublishOut_RemainsUnsigned(t *testing.T) {
	var got Envelope
	publish := func(_ string, data []byte) error { return json.Unmarshal(data, &got) }
	require.NoError(t, PublishOut(publish, "ns", "n", KindNotification, NotificationPayload{Text: "x"}))
	assert.Empty(t, got.Sig)
	assert.Empty(t, got.Publisher)
}
