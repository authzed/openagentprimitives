// Package restartmarker signs and verifies AgentSession.status.pendingRestart:
// the fork/takeover marker channelsd writes and the operator's restart
// reconciler acts on.
//
// Why the marker needs a signature: the per-session runner Role grants `patch`
// on `agentsessions/status` and Kubernetes RBAC has no field-level granularity,
// so a compromised runner can write status.pendingRestart on its own session.
// Takeover mode deliberately skips the SpiceDB agentsession#fork gate (the
// parent's #fork relation neither holds nor should for a different user), and
// BuildChildSession stamps pendingRestart.triggeredBy onto the child's
// started-by-canonical-id — which the AgentSession reconciler turns into a Role
// granting the runner SA get+update on the NAMED user's OAuth master Secrets. A
// forged marker is therefore direct privilege escalation to any user the
// attacker can name.
//
// So the marker is made unforgeable rather than merely well-formed: channelsd
// signs it with the same Ed25519 publisher key it registers with the operator
// (pkg/memory/publisherkeys), and the operator verifies before acting — same
// key, same registry, same content-addressed key IDs, no parallel trust root.
//
// A signature proves who WROTE a marker; it cannot detect a REMOVAL. A runner
// patching its own status.pendingRestart to null deletes another user's queued
// continuation and leaves nothing to verify. The admission webhook
// (pkg/controllers/webhooks/agentsession) refuses that write; the two are
// complementary.
package restartmarker

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// Publisher is the provenance identity channelsd signs restart markers as — the
// same string it registers its key under at startup and signs append-only
// memory entries with, so the operator's publisher-key registry resolves it
// with no additional wiring.
const Publisher = "system:channelsd"

// digestVersion is the canonical-serialization format version, folded into the
// digest. A future field addition bumps it, so an old signature can never be
// reinterpreted under new semantics.
const digestVersion = 1

// Errors returned by Verify. Every one is terminal and refuses the restart;
// they are distinguished so a caller can tell an UPGRADE WINDOW (ErrUnsigned)
// from a FORGERY (everything else) — in its logs, and in what it tells the
// user. See the operator's deny path.
var (
	// ErrUnsigned is returned for a marker carrying no attestation envelope at
	// all — the shape an operator upgraded ahead of channelsd sees (`oap install`
	// rolls the two as independent Deployments with no ordering). The marker is
	// still refused; this only separates "nobody signed it yet" from "someone
	// signed it wrong".
	//
	// The boundary is a MISSING envelope, never empty signature bytes: an
	// envelope with an empty Sig is nothing any writer produces, so it falls
	// through to the ordinary checks and comes back ErrBadSignature. Drawing the
	// line at len(Sig)==0 would let a forger elect the upgrade-shaped treatment
	// by leaving one field empty.
	ErrUnsigned = errors.New("restart marker carries no signature")
	// ErrWrongPublisher is returned when the marker is attributed to any
	// publisher other than Publisher. The operator passes the whole component
	// registry, so "the signer is someone we trust" is strictly weaker than "the
	// signer is channelsd"; this is the difference.
	ErrWrongPublisher = errors.New("restart marker is attributed to a publisher that may not author one")
	// ErrUnknownKey is returned when no trusted key is registered for the
	// marker's (publisher, keyID) pair.
	ErrUnknownKey = errors.New("restart marker names an unregistered signing key")
	// ErrBadSignature is returned when the signature does not verify over the
	// recomputed digest — the marker was fabricated, or a field was mutated
	// after signing.
	ErrBadSignature = errors.New("restart marker signature does not verify")
)

// KeyLookup resolves a (publisher, keyID) pair to its trusted Ed25519 public
// key. *publisherkeys.Registry and *tokens.Registry both satisfy it
// structurally, which is why the operator can pass the registry it already
// builds for provenance verify-on-write without an adapter.
type KeyLookup interface {
	PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool)
}

// canonical is the signed message: the parent's identity plus EVERY marker
// field the restart reconciler acts on. A field omitted here would be mutable
// under an otherwise-valid signature, so this struct and v1alpha1.PendingRestart
// must stay in lockstep — a new marker field is added here too and bumps
// digestVersion.
//
// TestDigest_CoversEveryPendingRestartField enforces that lockstep by
// reflection. It stays a test rather than a digest-from-the-marshalled-marker
// rewrite because re-serializing would change the digest bytes, and every marker
// already on a cluster object would stop verifying at upgrade — a silently
// refused takeover.
type canonical struct {
	// V is digestVersion, folded in so an old signature can never be
	// reinterpreted under new field semantics.
	V int `json:"v"`

	// Parent identity. UID is what stops a marker signed for one session from
	// being replayed onto a delete-and-recreated session of the same name.
	Namespace string `json:"ns"`
	Name      string `json:"name"`
	UID       string `json:"uid"`

	// Claimed author. Folding these in binds the signature to the identity it is
	// attributed to, so it cannot be re-presented under a different publisher.
	Publisher string `json:"publisher"`
	KeyID     string `json:"keyId"`

	// Mode is "" (restart-from-here) or "inherit" (full-transcript continuation).
	Mode string `json:"mode"`
	// TriggeredBy is the canonical subject the child is started by — the
	// escalation-relevant field this signature exists to pin.
	TriggeredBy string `json:"triggeredBy"`
	// TargetSessionName is the deterministic child-session name.
	TargetSessionName string `json:"target"`
	// CutTurnIndex is the last parent turn copied to the child, inclusive.
	CutTurnIndex int32 `json:"cutTurnIndex"`
	// NewUserText is the user's edited message, seeded at CutTurnIndex+1.
	NewUserText string `json:"newUserText"`
	// NewOwnerExternalID is the taking-over user's channel external id; empty
	// outside takeover mode.
	NewOwnerExternalID string `json:"newOwnerExternalId"`
	// InheritHistory is takeover-mode transcript seeding: true copies the full
	// parent transcript, false seeds only the new user's message.
	InheritHistory bool `json:"inheritHistory"`
	// RequestedAt is RFC3339 at SECOND precision — see Digest.
	RequestedAt string `json:"requestedAt"`
}

// Digest returns the hex SHA-256 of the canonical serialization of pr bound to
// parent, attributed to (publisher, keyID). It is the signed message.
//
// RequestedAt is formatted at SECOND precision on purpose: metav1.Time marshals
// as RFC3339 with no fractional part, so a digest taken over the signer's
// nanosecond clock reading would never recompute after the marker round-trips
// through the API server.
func Digest(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart, publisher, keyID string) string {
	c := canonical{
		V:                  digestVersion,
		Namespace:          parent.Namespace,
		Name:               parent.Name,
		UID:                string(parent.UID),
		Publisher:          publisher,
		KeyID:              keyID,
		Mode:               pr.Mode,
		TriggeredBy:        pr.TriggeredBy.String(),
		TargetSessionName:  pr.TargetSessionName,
		CutTurnIndex:       pr.CutTurnIndex,
		NewUserText:        pr.NewUserText,
		NewOwnerExternalID: pr.NewOwnerExternalID,
		InheritHistory:     pr.InheritHistory,
		RequestedAt:        pr.RequestedAt.UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(c)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Signer attests restart markers for one publisher with one Ed25519 key.
// Unlike provenance.Signer it keeps no chain state: a marker is a single
// short-lived authorization token, not a log entry, so there is nothing to
// order and nothing to seed after a restart.
type Signer struct {
	priv      ed25519.PrivateKey
	publisher string
	keyID     string
}

// NewSigner builds a Signer over priv, attributing markers to publisher. The key
// ID is the content address of the public half, derived with the same pkg/x/keyid
// helper the publisher-key registry validates against, so a Signer can only claim
// a keyID the operator will accept for this key.
//
// publisher is a parameter for symmetry with provenance.NewSigner, but Publisher
// is the only value Verify accepts: any other mints markers every verifier refuses.
func NewSigner(priv ed25519.PrivateKey, publisher string) *Signer {
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{priv: priv, publisher: publisher, keyID: keyid.For(pub)}
}

// KeyID returns the hex key identifier of this Signer's public key.
func (s *Signer) KeyID() string { return s.keyID }

// Sign fills pr.Signature with this Signer's attestation over pr bound to
// parent. Call it after every marker field is final: the digest covers them
// all, so a later mutation invalidates the signature (which is the point).
func (s *Signer) Sign(parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) error {
	if parent == nil || pr == nil {
		return errors.New("restartmarker: Sign requires a parent session and a marker")
	}
	raw, err := hex.DecodeString(Digest(parent, pr, s.publisher, s.keyID))
	if err != nil {
		// Digest always returns valid hex; a failure here is a programmer
		// error, surfaced rather than silently swallowed.
		return fmt.Errorf("restartmarker: decode digest: %w", err)
	}
	pr.Signature = &spiceboxv1alpha1.PendingRestartSignature{
		Publisher: s.publisher,
		KeyID:     s.keyID,
		Sig:       ed25519.Sign(s.priv, raw),
	}
	return nil
}

// Verify checks that pr carries a signature from Publisher, over pr bound to
// parent. It is fail-closed in every direction: a nil lookup, a nil marker, a
// missing signature, a foreign publisher, an unregistered key, a malformed key,
// or a digest mismatch all return an error. There is no path through Verify
// that accepts an unattested marker.
func Verify(keys KeyLookup, parent *spiceboxv1alpha1.AgentSession, pr *spiceboxv1alpha1.PendingRestart) error {
	if parent == nil || pr == nil {
		return errors.New("restartmarker: Verify requires a parent session and a marker")
	}
	if keys == nil {
		// A missing key lookup is a wiring bug, not an attack — but accepting
		// the marker because we cannot check it is precisely the fail-open
		// this package exists to prevent.
		return errors.New("restartmarker: no publisher-key lookup configured; refusing to accept an unverifiable marker")
	}
	sig := pr.Signature
	if sig == nil {
		// No envelope at all: the pre-upgrade writer's shape. An envelope with
		// empty Sig deliberately does NOT land here — see ErrUnsigned — it goes
		// on to the publisher pin, the key lookup, and ed25519.Verify, which
		// returns false (rather than panicking) for a wrong-length signature.
		return ErrUnsigned
	}
	// Pin the author BEFORE the key lookup. The operator passes the whole
	// component publisher registry (operator and authzd as well as channelsd), so
	// resolving a key proves only that SOME trusted component signed this — while
	// a marker is an authorization input only channelsd may produce. Without this
	// check, compromise of any other component publisher yields the privilege
	// escalation this package exists to close, with a valid signature and no signal.
	if sig.Publisher != Publisher {
		return fmt.Errorf("%w: %q signed a marker only %q may author", ErrWrongPublisher, sig.Publisher, Publisher)
	}
	pub, known := keys.PublisherKey(sig.Publisher, sig.KeyID)
	if !known {
		return fmt.Errorf("%w: publisher %q keyID %q", ErrUnknownKey, sig.Publisher, sig.KeyID)
	}
	if len(pub) != ed25519.PublicKeySize {
		// ed25519.Verify panics on a wrong-sized key, and the registry is
		// ConfigMap-sourced. A corrupt entry must yield a clean refusal, not a
		// controller panic.
		return fmt.Errorf("%w: trusted key for publisher %q keyID %q is malformed (%d bytes)",
			ErrUnknownKey, sig.Publisher, sig.KeyID, len(pub))
	}
	raw, err := hex.DecodeString(Digest(parent, pr, sig.Publisher, sig.KeyID))
	if err != nil {
		return fmt.Errorf("restartmarker: decode digest: %w", err)
	}
	if !ed25519.Verify(pub, raw, sig.Sig) {
		return ErrBadSignature
	}
	return nil
}
