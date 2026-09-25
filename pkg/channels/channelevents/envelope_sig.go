package channelevents

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// envelopeSigDomain domain-separates envelope signatures from every other
// Ed25519 use of the same key (the provenance audit chain signs a different
// canonical shape under no such prefix, so the two can never be confused).
const envelopeSigDomain = "ap.envelope.v1"

// ErrEnvelopeUnsigned is returned by VerifyEnvelopeSig when the envelope
// carries no signature fields at all — distinct from a present-but-invalid
// signature so enforcement points can name which failure they saw.
var ErrEnvelopeUnsigned = errors.New("channelevents: envelope is unsigned")

// EnvelopeSigner signs envelopes with a publisher identity key. One instance
// per process per publisher; safe for concurrent use. A nil *EnvelopeSigner
// is a valid no-op signer, so unsigned paths need no branching.
type EnvelopeSigner struct {
	priv       ed25519.PrivateKey
	publisher  string
	keyID      string
	epoch      string
	sessionUID string
	seq        atomic.Uint64

	// publishMu serializes Sign→marshal→publish end to end across concurrent
	// callers of the signer publish methods (publishEnvelope, PublishOutSeq,
	// RequestIn — see envelope.go). seq.Add(1) alone is atomic, but Sign and
	// the actual wire publish are two separate steps; without holding this
	// lock across both, two goroutines publishing on the SAME subject (e.g.
	// two concurrent reply_to_subagent tool calls in one runner turn — tool
	// dispatch is per-goroutine) can have the goroutine that got the LOWER
	// SigSeq lose the race and land on the wire second. A verifier enforcing
	// strictly-increasing SigSeq per subject then refuses that envelope as a
	// replay, dropping a legitimate message. See TestSignerPublishIn_ConcurrentCallsStayOrdered.
	publishMu sync.Mutex
}

func NewEnvelopeSigner(priv ed25519.PrivateKey, publisher, sessionUID string) (*EnvelopeSigner, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("envelope signer: private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(priv))
	}
	if publisher == "" {
		return nil, errors.New("envelope signer: publisher identity is required")
	}
	var eb [8]byte
	if _, err := rand.Read(eb[:]); err != nil {
		return nil, fmt.Errorf("envelope signer: mint epoch: %w", err)
	}
	return &EnvelopeSigner{
		priv:       priv,
		publisher:  publisher,
		keyID:      keyid.For(priv.Public().(ed25519.PublicKey)),
		epoch:      hex.EncodeToString(eb[:]),
		sessionUID: sessionUID,
	}, nil
}

func (s *EnvelopeSigner) Publisher() string {
	if s == nil {
		return ""
	}
	return s.publisher
}

func (s *EnvelopeSigner) KeyID() string {
	if s == nil {
		return ""
	}
	return s.keyID
}

// Sign stamps the signature fields onto env, binding it to the exact NATS
// subject it will be published on. Call it as the LAST mutation before
// marshal — later field changes invalidate the signature by design.
//
// ORDERING PRECONDITION: SigSeq assignment (below) and the eventual wire
// publish are two separate steps, and Sign itself takes no lock — a caller
// that signs and publishes on a subject where verification is (or will be)
// enforced MUST serialize its own Sign→publish sequence per subject, or two
// concurrent callers can land on the wire out of SigSeq order and have the
// lower-seq (and therefore legitimate) envelope refused as a replay. The
// three signer publish methods (publishEnvelope, PublishOutSeq, RequestIn in
// envelope.go) do this themselves, via EnvelopeSigner.publishMu. The
// runner's four loop hooks (internal/cmd/runner/main.go:
// InteractionRequestPublish, TimeoutAppliedPublish, IdentityChoicePublish,
// UIPublish) call Sign, marshal, and publish directly rather than through
// those methods, and are NOT ordering-safe today — they publish kinds that
// are not per-subject-seq-enforced yet, so this is latent, not live. The
// per-kind-enforcement follow-up must make those paths ordering-safe (or
// route them through a signer publish method) before flipping enforcement on
// for any kind they carry.
func (s *EnvelopeSigner) Sign(subject string, env *Envelope) error {
	if s == nil {
		return nil
	}
	if env.SessionUID == "" {
		env.SessionUID = s.sessionUID
	}
	env.Publisher = s.publisher
	env.SigKeyID = s.keyID
	env.SigEpoch = s.epoch
	env.SigSeq = s.seq.Add(1)
	env.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, EnvelopeSigDigest(subject, *env)))
	return nil
}

// EnvelopeSigDigest is the canonical digest a signature covers.
//
// WIRE FORMAT. encoding/json emits struct fields in DECLARATION order, so the
// field order and every json tag below are part of the signed message — the
// same discipline as provenance.EntryDigest, which this deliberately mirrors.
func EnvelopeSigDigest(subject string, env Envelope) []byte {
	payloadSum := sha256.Sum256(env.Payload)
	type canonical struct {
		Domain      string `json:"domain"`
		Subject     string `json:"subject"`
		Version     int    `json:"v"`
		Kind        string `json:"kind"`
		SessionNS   string `json:"sessionNs"`
		SessionName string `json:"sessionName"`
		SessionUID  string `json:"sessionUID"`
		// PublishedAt is µs-truncated for the same reason provenance
		// truncates: sub-µs precision does not survive every store/transport.
		PublishedAt string `json:"publishedAt"`
		Seq         uint64 `json:"seq"`
		Resurface   string `json:"resurfaceInterruptRequestID,omitempty"`
		PayloadSHA  string `json:"payloadSha256"`
		Publisher   string `json:"publisher"`
		SigKeyID    string `json:"sigKeyId"`
		SigEpoch    string `json:"sigEpoch"`
		SigSeq      uint64 `json:"sigSeq"`
	}
	c := canonical{
		Domain:      envelopeSigDomain,
		Subject:     subject,
		Version:     env.Version,
		Kind:        string(env.Kind),
		SessionNS:   env.Session.Namespace,
		SessionName: env.Session.Name,
		SessionUID:  env.SessionUID,
		PublishedAt: env.PublishedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		Seq:         env.Seq,
		Resurface:   env.ResurfaceInterruptRequestID,
		PayloadSHA:  hex.EncodeToString(payloadSum[:]),
		Publisher:   env.Publisher,
		SigKeyID:    env.SigKeyID,
		SigEpoch:    env.SigEpoch,
		SigSeq:      env.SigSeq,
	}
	body, err := json.Marshal(c)
	if err != nil {
		// Unreachable for this all-scalar struct; mirror provenance's
		// poisoned-digest fallback rather than panicking in a signer.
		sum := sha256.Sum256([]byte("channelevents: undigestable envelope: " + err.Error()))
		return sum[:]
	}
	sum := sha256.Sum256(body)
	return sum[:]
}

// VerifyEnvelopeSig checks env's signature against pub for the given subject.
// It verifies CRYPTOGRAPHY only — publisher identity, key selection,
// session-window binding, freshness and replay are the enforcement point's
// checks, layered on top.
func VerifyEnvelopeSig(subject string, env Envelope, pub ed25519.PublicKey) error {
	if env.Sig == "" && env.Publisher == "" && env.SigKeyID == "" {
		return ErrEnvelopeUnsigned
	}
	if env.Sig == "" {
		return errors.New("channelevents: envelope has signature fields but no sig")
	}
	sig, err := base64.StdEncoding.DecodeString(env.Sig)
	if err != nil {
		return fmt.Errorf("channelevents: decode envelope sig: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("channelevents: verify key must be %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}
	if !ed25519.Verify(pub, EnvelopeSigDigest(subject, env), sig) {
		return errors.New("channelevents: envelope signature invalid")
	}
	return nil
}
