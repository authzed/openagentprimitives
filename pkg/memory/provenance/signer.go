package provenance

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// chainState is the per-(scope) chain head for a single publisher: the
// last signed sequence number and its EntryDigest. The next Sign emits
// Seq = seq+1 with PrevHash = lastHash.
type chainState struct {
	seq      uint64
	lastHash string
}

// Signer attests append-only entries for one publisher with one
// Ed25519 key. It maintains per-scope hash-chain state under a mutex so
// concurrent Puts across distinct scopes don't interleave, and a single
// scope's chain advances atomically. A Signer is safe for concurrent
// use.
type Signer struct {
	priv      ed25519.PrivateKey
	publisher string
	keyID     string

	mu     sync.Mutex
	chains map[memory.Scope]chainState
	seeded map[memory.Scope]bool
}

// NewSigner builds a Signer for publisher over priv. The key ID is
// derived from the public half via KeyID.
func NewSigner(priv ed25519.PrivateKey, publisher string) *Signer {
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{
		priv:      priv,
		publisher: publisher,
		keyID:     KeyID(pub),
		chains:    map[memory.Scope]chainState{},
		seeded:    map[memory.Scope]bool{},
	}
}

// Publisher returns the publisher identity this Signer attests as.
func (s *Signer) Publisher() string { return s.publisher }

// KeyID returns the hex key identifier of this Signer's public key.
func (s *Signer) KeyID() string { return s.keyID }

// Sign fills e.Provenance with this Signer's publisher/keyID, the next
// sequence number for e.Scope, and the prior entry's digest as
// PrevHash, then signs the canonical digest and advances the chain. The
// signature covers the digest, which already includes the provenance
// header (publisher/keyID/seq/prevHash) — so it commits to chain
// position as well as payload.
func (s *Signer) Sign(e *memory.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.chains[e.Scope]
	e.Provenance = &memory.Provenance{
		Publisher: s.publisher,
		KeyID:     s.keyID,
		Seq:       st.seq + 1,
		PrevHash:  st.lastHash,
	}
	digest := EntryDigest(*e)
	raw, err := hex.DecodeString(digest)
	if err != nil {
		// EntryDigest always returns valid hex; a failure here is a
		// programmer error, surfaced rather than silently swallowed.
		return fmt.Errorf("decode digest %q: %w", digest, err)
	}
	e.Provenance.Sig = ed25519.Sign(s.priv, raw)
	s.chains[e.Scope] = chainState{seq: e.Provenance.Seq, lastHash: digest}
	return nil
}

// SeedFromMemory rebuilds this Signer's chain head for scope from
// persisted entries, so a freshly constructed Signer (after a process
// restart or session resume) continues the chain rather than restarting
// it at Seq 1. It scans every registered append-only Kind for entries
// this publisher signed and adopts the one with the highest Seq as the
// new chain head.
func (s *Signer) SeedFromMemory(ctx context.Context, mem memory.Memory, scope memory.Scope) error {
	var (
		found    bool
		maxSeq   uint64
		maxEntry memory.Entry
	)
	for _, k := range memory.RegisteredKinds() {
		if !k.Retention().AppendOnly {
			continue
		}
		res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{k.Name()}})
		if err != nil {
			// A kind THIS credential cannot read holds no chain this publisher can
			// resume — it cannot even see its own entries there. pt_tag_content is
			// SessionReadable()=false by design (the platform-only content-binding
			// ledger), so a SESSION credential's seed scan names it and the read
			// door refuses it; skipping keeps the session bootable. A PLATFORM
			// credential CAN read it, so the door never refuses and it is still
			// scanned + anchored — the skip is keyed on the read-door error, which
			// only a session credential provokes, not on the kind's name.
			if errors.Is(err, memory.ErrKindNotSessionReadable) {
				continue
			}
			// Names what the query is FOR. Bare "query kind %q" reads as
			// though the kind were being demanded of the caller — an
			// `approval`-kind failure here sent one investigation looking for
			// a missing approval flow, when the scan reaches every append-only
			// kind in name order and stops at the first that errors.
			return fmt.Errorf("scanning append-only kind %q for this publisher's chain head: %w", k.Name(), err)
		}
		for _, e := range res.Entries {
			p := e.Provenance
			if p == nil || p.Publisher != s.publisher {
				continue
			}
			if !found || p.Seq > maxSeq {
				found, maxSeq, maxEntry = true, p.Seq, e
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if found {
		s.chains[scope] = chainState{seq: maxSeq, lastHash: EntryDigest(maxEntry)}
	} else {
		s.chains[scope] = chainState{}
	}
	return nil
}

// InvalidateSeed forces the next EnsureSeeded(scope) to re-derive the chain
// head from durable storage. Call it whenever a signed entry did NOT land at
// the position Sign minted for it — the store rejected the write, or accepted
// it without persisting that position (an append-only re-put whose payload
// matches a stored entry is idempotent: the facade returns the stored entry,
// discarding the freshly-minted seq). Sign advances the in-memory head before
// the Put, so either case otherwise strands the head ahead of the durable tail,
// and the next accepted append would skip a seq (gap) and link its prevHash to
// a digest that was never stored (fork). Re-seeding rebuilds the head to the
// highest seq that actually landed — the right answer whether the lost entry
// was the chain tail or an older position being re-put.
func (s *Signer) InvalidateSeed(scope memory.Scope) {
	s.mu.Lock()
	delete(s.seeded, scope)
	s.mu.Unlock()
}

// EnsureSeeded runs SeedFromMemory at most once per scope. Subsequent
// calls for the same scope are no-ops, so the hot Put path pays the
// query cost only on the first append-only write into a scope.
func (s *Signer) EnsureSeeded(ctx context.Context, mem memory.Memory, scope memory.Scope) error {
	s.mu.Lock()
	already := s.seeded[scope]
	s.mu.Unlock()
	if already {
		return nil
	}
	// SeedFromMemory takes the same mutex internally, so it must run
	// outside our lock; we re-take below only to flip the seeded flag.
	if err := s.SeedFromMemory(ctx, mem, scope); err != nil {
		return err
	}
	s.mu.Lock()
	s.seeded[scope] = true
	s.mu.Unlock()
	return nil
}
