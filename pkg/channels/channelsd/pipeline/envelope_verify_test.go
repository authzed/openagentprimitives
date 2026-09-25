package pipeline

// Verification + anti-replay for agent_message_send envelopes: the fail-closed
// choke point at the top of HandleAgentMessageSend. These tests exercise
// envelopeVerifier directly (white-box, same package) so the replay/GC cases
// can inspect the HWM map without threading it through the whole handler.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// newScheme builds the minimal runtime.Scheme the fake client needs to store
// AgentSession objects — no corev1 involved, this file never seeds a Secret.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "add scheme")
	return scheme
}

// verifierFixture builds an envelopeVerifier over a fake client seeding one
// AgentSession ("demo-ns/root-1") whose status carries the anchored audit
// key, plus a signer already bound to that exact session identity (publisher
// + UID). Tests that need a MISMATCHED signer (wrong publisher, wrong UID,
// wrong key) construct their own signer instead of the one returned here.
func verifierFixture(t *testing.T, now time.Time) (*envelopeVerifier, *channelevents.EnvelopeSigner, ed25519.PublicKey) {
	t.Helper()
	seed := bytes.Repeat([]byte{0x44}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "root-1", UID: types.UID("uid-1")},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			AuditPublicKey: base64.StdEncoding.EncodeToString(pub),
			AuditKeyID:     keyid.For(pub),
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	v := newEnvelopeVerifier(c)
	v.now = func() time.Time { return now }
	signer, err := channelevents.NewEnvelopeSigner(priv, "session:demo-ns/root-1", "uid-1")
	require.NoError(t, err)
	return v, signer, pub
}

// signedAgentMsg builds and signs an agent_message_send envelope from
// "demo-ns/root-1" to "demo-ns/child-1" — the shape verifierFixture's session
// authenticates.
func signedAgentMsg(t *testing.T, signer *channelevents.EnvelopeSigner) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope("demo-ns", "root-1", channelevents.KindAgentMessageSend,
		channelevents.AgentMessageSendPayload{To: channelevents.SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
	require.NoError(t, err)
	subject := channelevents.SubjectIn(channelevents.SubjectPrefix("demo-ns", "root-1"), channelevents.KindAgentMessageSend)
	require.NoError(t, signer.Sign(subject, &env))
	return env
}

// testSeedKey reconstructs the exact private key verifierFixture anchors, for
// tests that need a SECOND signer over the same key material (a fresh epoch,
// a wrong sessionUID) rather than the one instance verifierFixture returns.
func testSeedKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x44}, ed25519.SeedSize))
}

func TestVerifyAgentMessage_HappyPath(t *testing.T) {
	v, signer, _ := verifierFixture(t, time.Now())
	env := signedAgentMsg(t, signer)
	assert.NoError(t, v.verifyAgentMessage(context.Background(), env))
}

func TestVerifyAgentMessage_Unsigned(t *testing.T) {
	v, _, _ := verifierFixture(t, time.Now())
	env, err := channelevents.BuildEnvelope("demo-ns", "root-1", channelevents.KindAgentMessageSend,
		channelevents.AgentMessageSendPayload{To: channelevents.SessionRef{Namespace: "demo-ns", Name: "child-1"}, Text: "hi"})
	require.NoError(t, err)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.ErrorIs(t, err, channelevents.ErrEnvelopeUnsigned)
}

func TestVerifyAgentMessage_PublisherNotSubjectSession(t *testing.T) {
	v, _, _ := verifierFixture(t, time.Now())
	// A cryptographically VALID signature, over the same key material the
	// session anchors — but minted for a different publisher identity, so the
	// structural check must catch it before signature verification even runs.
	signer, err := channelevents.NewEnvelopeSigner(testSeedKey(), "session:demo-ns/other", "uid-1")
	require.NoError(t, err)
	env := signedAgentMsg(t, signer)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not the sending session")
}

func TestVerifyAgentMessage_SigKeyIDMismatch(t *testing.T) {
	v, _, _ := verifierFixture(t, time.Now())
	otherPriv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x55}, ed25519.SeedSize))
	signer, err := channelevents.NewEnvelopeSigner(otherPriv, "session:demo-ns/root-1", "uid-1")
	require.NoError(t, err)
	env := signedAgentMsg(t, signer)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sigKeyId")
}

func TestVerifyAgentMessage_AuditPublicKeyEmpty(t *testing.T) {
	priv := testSeedKey()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "root-1", UID: types.UID("uid-1")},
		// Status left zero: the key has not been anchored yet.
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	v := newEnvelopeVerifier(c)
	v.now = func() time.Time { return time.Now() }
	signer, err := channelevents.NewEnvelopeSigner(priv, "session:demo-ns/root-1", "uid-1")
	require.NoError(t, err)
	env := signedAgentMsg(t, signer)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no anchored audit key")
}

func TestVerifyAgentMessage_SessionUIDMismatch(t *testing.T) {
	v, _, _ := verifierFixture(t, time.Now())
	// Same key, same publisher — minted for a DIFFERENT session instance.
	signer, err := channelevents.NewEnvelopeSigner(testSeedKey(), "session:demo-ns/root-1", "uid-9")
	require.NoError(t, err)
	env := signedAgentMsg(t, signer)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessionUID")
}

func TestVerifyAgentMessage_AgentSessionAbsent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build() // no session seeded
	v := newEnvelopeVerifier(c)
	v.now = func() time.Time { return time.Now() }
	signer, err := channelevents.NewEnvelopeSigner(testSeedKey(), "session:demo-ns/root-1", "uid-1")
	require.NoError(t, err)
	env := signedAgentMsg(t, signer)

	err = v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load sending session")
}

func TestVerifyAgentMessage_StalePublishedAt(t *testing.T) {
	buildTime := time.Now()
	// The verifier's clock runs 6 minutes AHEAD of the envelope's real
	// PublishedAt (stamped inside BuildEnvelope at call time, below).
	v, signer, _ := verifierFixture(t, buildTime.Add(6*time.Minute))
	env := signedAgentMsg(t, signer)

	err := v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "freshness window")
}

func TestVerifyAgentMessage_FuturePublishedAt(t *testing.T) {
	buildTime := time.Now()
	// The verifier's clock runs 6 minutes BEHIND the envelope's real
	// PublishedAt — the envelope claims to be from the verifier's future.
	v, signer, _ := verifierFixture(t, buildTime.Add(-6*time.Minute))
	env := signedAgentMsg(t, signer)

	err := v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "freshness window")
}

func TestVerifyAgentMessage_ReplayRefused(t *testing.T) {
	now := time.Now()
	v, signer, _ := verifierFixture(t, now)
	env := signedAgentMsg(t, signer)
	require.NoError(t, v.verifyAgentMessage(context.Background(), env))
	err := v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replay")
}

func TestVerifyAgentMessage_DecreasingSigSeqRefused(t *testing.T) {
	now := time.Now()
	v, signer, _ := verifierFixture(t, now)
	msg1 := signedAgentMsg(t, signer) // sigSeq 1
	msg2 := signedAgentMsg(t, signer) // sigSeq 2

	require.NoError(t, v.verifyAgentMessage(context.Background(), msg2), "the later message arrives first")
	err := v.verifyAgentMessage(context.Background(), msg1)
	require.Error(t, err, "an out-of-order earlier sigSeq must be refused, not delivered late")
	assert.Contains(t, err.Error(), "replay")
}

func TestVerifyAgentMessage_NewEpochResets(t *testing.T) {
	now := time.Now()
	v, signer1, _ := verifierFixture(t, now)
	// A second signer over the SAME key + identity, but a freshly minted
	// (random) epoch — the shape of a restarted publisher process. Its
	// counter starts back at 1, and that must not collide with signer1's own
	// seq-1 high-water mark because the HWM key includes the epoch.
	signer2, err := channelevents.NewEnvelopeSigner(testSeedKey(), "session:demo-ns/root-1", "uid-1")
	require.NoError(t, err)

	env1 := signedAgentMsg(t, signer1)
	env2 := signedAgentMsg(t, signer2)
	assert.NoError(t, v.verifyAgentMessage(context.Background(), env1))
	assert.NoError(t, v.verifyAgentMessage(context.Background(), env2))
}

func TestVerifyAgentMessage_HWMGarbageCollected(t *testing.T) {
	now := time.Now()
	v, signer, _ := verifierFixture(t, now)

	// Seed a stale entry directly, as if verified long past the HWM
	// retention (2x the freshness window — see the GC comment in
	// envelope_verify.go for why retention is TWO windows, not one) — old
	// enough that no legitimately fresh envelope could ever refresh it, so
	// the only way it leaves the map is the sweep.
	staleKey := hwmKey{publisher: "session:demo-ns/other", sessionUID: "uid-stale", epoch: "dead", subject: "stale-subject"}
	v.mu.Lock()
	v.hwm[staleKey] = hwmEntry{seq: 1, seen: now.Add(-2*envelopeFreshnessWindow - time.Minute)}
	v.mu.Unlock()

	env := signedAgentMsg(t, signer)
	require.NoError(t, v.verifyAgentMessage(context.Background(), env))

	v.mu.Lock()
	defer v.mu.Unlock()
	assert.Len(t, v.hwm, 1, "the stale entry must be swept, leaving only the fresh one")
	_, stillThere := v.hwm[staleKey]
	assert.False(t, stillThere, "the stale entry specifically must be gone")
}

// TestVerifyAgentMessage_FutureSkewedReplayStillRefusedAfterOneWindow is the
// GC-vs-freshness seam the one-window sweep opened: freshness tolerates a
// PublishedAt up to one window in the FUTURE of the verifier's clock (sender
// clock skew), and such an envelope stays fresh until PublishedAt + window —
// up to TWO windows after the accept. A sweep at one window from the accept
// dropped the HWM entry while the identical signed envelope was still fresh,
// so a captured copy replayed cleanly. It bites exactly the LAST message a
// session sends on a subject before going quiet, the message nothing after
// it would ever bump the high-water mark for.
func TestVerifyAgentMessage_FutureSkewedReplayStillRefusedAfterOneWindow(t *testing.T) {
	// BuildEnvelope (inside signedAgentMsg) stamps PublishedAt from the real
	// wall clock; starting the verifier's fake clock 4m BEHIND it makes the
	// envelope future-skewed by ~4m at accept time — legal, within the
	// ±window the freshness check exists to tolerate.
	current := time.Now().Add(-4 * time.Minute)
	v, signer, _ := verifierFixture(t, current)
	v.now = func() time.Time { return current }

	env := signedAgentMsg(t, signer)
	require.NoError(t, v.verifyAgentMessage(context.Background(), env),
		"a future-skewed envelope within the window must be accepted")

	// Advance past ONE freshness window from the accept — where the old
	// sweep retired the entry — while the envelope itself is still fresh
	// (its freshness life runs to PublishedAt + window, ~9m after the
	// accept; at +6m the delta to PublishedAt is only ~2m).
	current = current.Add(6 * time.Minute)
	err := v.verifyAgentMessage(context.Background(), env)
	require.Error(t, err, "the identical envelope is STILL fresh here; replaying it must be refused")
	assert.Contains(t, err.Error(), "replay")
}
