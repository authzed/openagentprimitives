// Package publisherkeys is an in-memory publisher→keyID→Ed25519 key
// store for component publishers (channelsd, authzd, operator). The
// operator loads it from / persists it to a ConfigMap and composes it
// with the per-session tokens registry behind provenance verify-on-write.
package publisherkeys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// Entry is the flat, serializable form of one registered key: the
// standard-base64 public key under a (publisher, keyID) pair. It is the
// shape persisted into the publisher-keys ConfigMap.
type Entry struct {
	// Publisher is the component identity that signs with this key ("system:authzd", …).
	Publisher string `json:"publisher"`
	// KeyID is the key's content address (provenance.KeyID of PubKeyB64), not a free label.
	KeyID string `json:"keyId"`
	// PubKeyB64 is the 32-byte Ed25519 public key in STANDARD base64 (not URL-safe).
	PubKeyB64 string `json:"pubKey"`
}

// Registry is an in-memory publisher→keyID→Ed25519 key store for
// component publishers. It implements provenance.PublisherKeyLookup
// structurally (PublisherKey), so the provenance package need not be
// imported here.
type Registry struct {
	mu   sync.RWMutex
	keys map[string]map[string]ed25519.PublicKey
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{keys: map[string]map[string]ed25519.PublicKey{}}
}

// Add registers a public key for a publisher under keyID, first-write-wins.
// Additive across keyIDs: old keys remain for verifying old entries (rotation
// registers a new keyID). Add returns an error and changes nothing when:
//
//   - pub is not a 32-byte Ed25519 key;
//   - keyID is not the content address of pub (keyID == provenance.KeyID(pub)).
//     This invariant is what makes a keyID un-spoofable — a tampered ConfigMap
//     or a forged registration cannot bind a keyID to a key that does not hash
//     to it;
//   - (publisher, keyID) is already bound to a DIFFERENT key. A byte-identical
//     re-registration is idempotent (returns nil).
func (r *Registry) Add(publisher, keyID string, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("publisherkeys: publisher %q keyID %q: key is %d bytes (want %d)", publisher, keyID, len(pub), ed25519.PublicKeySize)
	}
	if want := provenance.KeyID(pub); keyID != want {
		return fmt.Errorf("publisherkeys: publisher %q keyID %q does not match the key's content address %q (refusing to register a forged/tampered key binding)", publisher, keyID, want)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys[publisher] == nil {
		r.keys[publisher] = map[string]ed25519.PublicKey{}
	}
	if cur, ok := r.keys[publisher][keyID]; ok {
		if !bytes.Equal(cur, pub) {
			return fmt.Errorf("publisherkeys: keyID %q is already bound to a different key for publisher %q (first-write-wins; overwrite refused)", keyID, publisher)
		}
		return nil // idempotent: same key re-registered
	}
	r.keys[publisher][keyID] = pub
	return nil
}

// PublisherKey resolves a (publisher, keyID) to its registered key.
func (r *Registry) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pub, ok := r.keys[publisher][keyID]
	return pub, ok
}

// Entries returns a flat snapshot of every registered key, for
// persistence into the ConfigMap. Order is unspecified.
func (r *Registry) Entries() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Entry
	for publisher, byKey := range r.keys {
		for keyID, pub := range byKey {
			out = append(out, Entry{
				Publisher: publisher,
				KeyID:     keyID,
				PubKeyB64: base64.StdEncoding.EncodeToString(pub),
			})
		}
	}
	return out
}

// Load installs entries decoded from the ConfigMap JSON, additively: existing
// keys are preserved. An entry is REJECTED (not installed) when its PubKeyB64
// fails to decode or it fails Add's invariants. Rejections are returned as a
// slice of errors so the caller can log them — a corrupt or tampered entry must
// neither abort the rest nor be dropped silently.
func (r *Registry) Load(entries []Entry) []error {
	var errs []error
	for _, e := range entries {
		pub, err := provenance.DecodePubKey(e.PubKeyB64)
		if err != nil {
			errs = append(errs, fmt.Errorf("publisherkeys: publisher %q keyID %q: %w", e.Publisher, e.KeyID, err))
			continue
		}
		if err := r.Add(e.Publisher, e.KeyID, pub); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
