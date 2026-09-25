package provenance

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// PublisherKeyLookup resolves a publisher's registered public keys for
// verify-on-write. The operator composes the per-session key registry
// (tokens) and the component key registry behind this.
type PublisherKeyLookup interface {
	PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool)
}

// WriteVerifier implements memory.ProvenanceVerifier: it requires a
// provenance envelope on append-only writes and verifies the signature
// under a registered key for the entry's publisher.
type WriteVerifier struct{ keys PublisherKeyLookup }

// WriteVerifier satisfies the facade's verify-on-write hook.
var _ memory.ProvenanceVerifier = (*WriteVerifier)(nil)

// NewWriteVerifier builds a verify-on-write enforcer over keys.
func NewWriteVerifier(keys PublisherKeyLookup) *WriteVerifier { return &WriteVerifier{keys: keys} }

// VerifyEntry rejects an append-only write that lacks provenance, whose
// publisher is not one the authenticated writer may author as, whose key is not
// registered, or whose signature does not verify over the entry digest.
//
// The publisher check (checkAuthor) and the key/signature check are two
// different guarantees and BOTH are load-bearing. The signature proves the entry
// was produced by whoever holds the publisher's key; the publisher check proves
// the writer on this connection IS that publisher. Drop the second and forged
// attribution is refused only for as long as no key ever leaks or is
// mis-registered — precisely the containment an audit log exists to provide.
func (w *WriteVerifier) VerifyEntry(ctx context.Context, caller string, e memory.Entry) error {
	if e.Provenance == nil {
		return fmt.Errorf("%w: %s/%s", memory.ErrProvenanceRequired, e.Kind, e.ID)
	}
	if err := w.checkAuthor(ctx, caller, e); err != nil {
		return err
	}
	return VerifyEntrySignature(w.keys, e)
}

// VerifyEntrySignature checks one entry's provenance envelope on its own: the
// envelope is present, a key is registered for the (publisher, keyID) it claims,
// and the signature verifies over the entry digest. It says NOTHING about chain
// position — no seq, no prevHash — so it answers "did this publisher sign this
// payload?" and not "does this entry belong where it sits". Use VerifyChain for
// the latter.
//
// It is the check a reader owes any entry whose CONTENT it is about to act on
// (a witnessed key binding, for one) and the same check WriteVerifier applies at
// the door, so the two cannot drift apart.
func VerifyEntrySignature(keys PublisherKeyLookup, e memory.Entry) error {
	if e.Provenance == nil {
		return fmt.Errorf("%w: %s/%s", memory.ErrProvenanceRequired, e.Kind, e.ID)
	}
	pub, ok := keys.PublisherKey(e.Provenance.Publisher, e.Provenance.KeyID)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: no usable key %s for %s", memory.ErrBadProvenance, e.Provenance.KeyID, e.Provenance.Publisher)
	}
	raw, err := hex.DecodeString(EntryDigest(e))
	if err != nil || !ed25519.Verify(pub, raw, e.Provenance.Sig) {
		return fmt.Errorf("%w: signature invalid for %s/%s", memory.ErrBadProvenance, e.Kind, e.ID)
	}
	return nil
}

// checkAuthor binds the entry's claimed publisher to the authenticated writer.
// There are exactly three writer classes reaching the facade, each answered on
// its own terms:
//
//   - A PER-SESSION bearer (the runner — the least-trusted class, and the only
//     one holding a credential an agent's own process can leak). Its publisher
//     MUST be its own session's. Nothing else may be authored on that token, in
//     any scope it reaches. This branch is keyed on the token session being
//     PRESENT, never on a string being non-empty: a runner's token registers
//     with an EMPTY callerID, so a `caller != ""` test binds nothing for exactly
//     the caller that most needs binding.
//   - A SYSTEM bearer (channelsd/authzd). Its caller is a package constant that
//     is also its publisher, so publisher must equal caller.
//   - An IN-PROCESS operator write. It reaches the facade with neither, holds
//     the operator's own signing key, and is bound by the signature alone.
//
// The classes are exhaustive by construction: httpsrv answers 401 before
// dispatch to anything that is neither a system nor a registered per-session
// token, and it attaches the token session for every per-session bearer.
func (w *WriteVerifier) checkAuthor(ctx context.Context, caller string, e memory.Entry) error {
	if sess, viaSessionToken := memory.TokenSessionFrom(ctx); viaSessionToken {
		if sess.Namespace == "" || sess.Name == "" {
			// Present but incomplete is a wiring bug, not a caller class. Refuse
			// rather than fall through to a weaker check — an unnamed session
			// would otherwise be able to author as any publisher it can sign for.
			return fmt.Errorf("%w: request carries an incomplete token session identity (%q/%q); refusing to bind the publisher",
				memory.ErrBadProvenance, sess.Namespace, sess.Name)
		}
		if want := SessionPublisher(sess.Namespace, sess.Name); e.Provenance.Publisher != want {
			return fmt.Errorf("%w: publisher %q may not be authored on session %s/%s's token (may author %q only)",
				memory.ErrBadProvenance, e.Provenance.Publisher, sess.Namespace, sess.Name, want)
		}
		return nil
	}
	if caller != "" && caller != e.Provenance.Publisher {
		return fmt.Errorf("%w: publisher %q != caller %q", memory.ErrBadProvenance, e.Provenance.Publisher, caller)
	}
	return nil
}
