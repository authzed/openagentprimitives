package provenance

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Verdict labels one thing the verifier found wrong (or, by absence,
// right) about a chain entry. A defined type (not a string alias) so the
// compiler type-checks verdict values at use sites.
type Verdict string

const (
	// VerdictUnsigned: an append-only entry carries no Provenance.
	VerdictUnsigned Verdict = "unsigned"
	// VerdictUnknownKey: the entry's KeyID is not in the trusted set.
	VerdictUnknownKey Verdict = "unknown-key"
	// VerdictBadSignature: the Ed25519 signature does not verify over
	// the recomputed digest.
	VerdictBadSignature Verdict = "bad-signature"
	// VerdictGap: a sequence number is missing from the chain.
	VerdictGap Verdict = "gap"
	// VerdictFork: two entries share a Seq, or a PrevHash does not link
	// to the prior entry's digest.
	VerdictFork Verdict = "fork"
	// VerdictTailTruncated: a trusted anchor expects a higher max Seq
	// (or a different head hash) than the stored chain provides.
	VerdictTailTruncated Verdict = "tail-truncated"
)

// Finding is one verifier verdict against a specific entry (or, for
// tail truncation, against the chain head). Seq is the offending entry's
// sequence (or the expected anchor seq for truncation).
type Finding struct {
	Verdict Verdict
	Kind    string
	ID      string
	Seq     uint64
	Detail  string
}

// Report is the outcome of verifying one publisher's chain. OK counts
// the entries that passed every check; Findings lists every problem.
type Report struct {
	Publisher string
	OK        int
	Findings  []Finding
}

// ChainHead is a trusted tail anchor for a publisher's chain: the
// sequence and digest of the last entry the publisher is known to have
// signed. Recorded externally (e.g. AgentSession.status) so truncation
// of the stored tail is detectable.
type ChainHead struct {
	Seq      uint64
	LastHash string
}

// PubKeyRef identifies a trusted key by the (publisher, keyID) pair — the
// same composite the write-time PublisherKeyLookup keys on.
type PubKeyRef struct {
	Publisher string
	KeyID     string
}

// MapKeyLookup is a static (publisher,keyID)→key PublisherKeyLookup built
// from a trusted snapshot (used by oap audit verify and tests). Keying by
// BOTH publisher and keyID — not keyID alone — is what stops a key
// registered for one publisher from verifying another publisher's chain,
// matching WriteVerifier's write-time semantics.
type MapKeyLookup map[PubKeyRef]ed25519.PublicKey

// PublisherKey resolves a (publisher, keyID) pair. Satisfies PublisherKeyLookup.
func (m MapKeyLookup) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	k, ok := m[PubKeyRef{Publisher: publisher, KeyID: keyID}]
	return k, ok
}

// Verifier checks chains against a trusted key set. It resolves keys by
// (publisher, keyID) — the same composite the write-time WriteVerifier
// uses — so a key registered for one publisher cannot verify a chain
// attributed to another.
type Verifier struct {
	keys PublisherKeyLookup
}

// NewVerifier builds a Verifier over the given (publisher,keyID) lookup.
// Pass a MapKeyLookup, a *publisherkeys.Registry, or any PublisherKeyLookup.
func NewVerifier(keys PublisherKeyLookup) *Verifier {
	return &Verifier{keys: keys}
}

// VerifyChain walks publisher's chain within entries and reports every
// problem found. Entries signed by other publishers are ignored.
// Append-only entries carrying no Provenance are reported unsigned.
// The remaining entries are sorted by Seq and checked for forks
// (duplicate Seq, broken prevHash linkage), gaps (missing Seq from 1),
// unknown keys, and bad signatures. When anchor is non-nil, a stored
// max Seq below the anchor — or an equal Seq whose head digest differs —
// is reported as tail truncation.
func (v *Verifier) VerifyChain(publisher string, entries []memory.Entry, anchor *ChainHead) Report {
	rep := Report{Publisher: publisher}

	var signed []memory.Entry
	for _, e := range entries {
		if e.Provenance == nil {
			// Unsigned append-only entry: attributable to no publisher,
			// but it pollutes the scope. Report once, against this
			// publisher's verification pass.
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictUnsigned,
				Kind:    e.Kind,
				ID:      e.ID,
				Detail:  "append-only entry has no provenance",
			})
			continue
		}
		if e.Provenance.Publisher != publisher {
			continue // belongs to a different publisher's chain
		}
		signed = append(signed, e)
	}

	sort.SliceStable(signed, func(i, j int) bool {
		return signed[i].Provenance.Seq < signed[j].Provenance.Seq
	})

	// Fork: duplicate Seq among this publisher's entries.
	for i := 1; i < len(signed); i++ {
		if signed[i].Provenance.Seq == signed[i-1].Provenance.Seq {
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictFork,
				Kind:    signed[i].Kind,
				ID:      signed[i].ID,
				Seq:     signed[i].Provenance.Seq,
				Detail:  fmt.Sprintf("duplicate seq %d", signed[i].Provenance.Seq),
			})
		}
	}

	// Gap: contiguity from 1. A jump from prev to cur leaves the
	// numbers in between missing.
	var prevSeq uint64
	for _, e := range signed {
		for missing := prevSeq + 1; missing < e.Provenance.Seq; missing++ {
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictGap,
				Kind:    e.Kind,
				ID:      e.ID,
				Seq:     missing,
				Detail:  fmt.Sprintf("missing seq %d", missing),
			})
		}
		if e.Provenance.Seq > prevSeq {
			prevSeq = e.Provenance.Seq
		}
	}

	// Per-entry checks: seq validity, prevHash linkage, key trust,
	// signature.
	for i, e := range signed {
		ok := true

		// The Signer emits Seq >= 1; a signed entry at Seq 0 violates the
		// 1-based chain contract. Flag it as a fork. The gap-from-1 logic
		// above runs independently, so this does not mask a genuine gap.
		if e.Provenance.Seq == 0 {
			ok = false
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictFork,
				Kind:    e.Kind,
				ID:      e.ID,
				Seq:     e.Provenance.Seq,
				Detail:  "seq 0 is invalid (chain is 1-based)",
			})
		}

		wantPrev := ""
		if i > 0 {
			wantPrev = EntryDigest(signed[i-1])
		}
		if e.Provenance.PrevHash != wantPrev {
			ok = false
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictFork,
				Kind:    e.Kind,
				ID:      e.ID,
				Seq:     e.Provenance.Seq,
				Detail:  "prevHash mismatch",
			})
		}

		pub, known := v.keys.PublisherKey(e.Provenance.Publisher, e.Provenance.KeyID)
		switch {
		case !known:
			ok = false
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictUnknownKey,
				Kind:    e.Kind,
				ID:      e.ID,
				Seq:     e.Provenance.Seq,
				Detail:  fmt.Sprintf("no trusted key for keyID %q", e.Provenance.KeyID),
			})
		case len(pub) != ed25519.PublicKeySize:
			// ed25519.Verify panics on a wrong-sized key. The key map is
			// operator-sourced (status/ConfigMap); a corrupt entry must
			// yield a clean unknown-key verdict, not crash oap audit verify.
			ok = false
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictUnknownKey,
				Kind:    e.Kind,
				ID:      e.ID,
				Seq:     e.Provenance.Seq,
				Detail:  fmt.Sprintf("trusted key for keyID %q is malformed (%d bytes)", e.Provenance.KeyID, len(pub)),
			})
		default:
			raw, err := hex.DecodeString(EntryDigest(e))
			if err != nil || !ed25519.Verify(pub, raw, e.Provenance.Sig) {
				ok = false
				rep.Findings = append(rep.Findings, Finding{
					Verdict: VerdictBadSignature,
					Kind:    e.Kind,
					ID:      e.ID,
					Seq:     e.Provenance.Seq,
					Detail:  "signature does not verify over entry digest",
				})
			}
		}

		if ok {
			rep.OK++
		}
	}

	// Tail anchor: detect truncation of the stored tail relative to a
	// trusted external head.
	if anchor != nil {
		var maxSeq uint64
		var headDigest string
		if len(signed) > 0 {
			last := signed[len(signed)-1]
			maxSeq = last.Provenance.Seq
			headDigest = EntryDigest(last)
		}
		switch {
		case maxSeq < anchor.Seq:
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictTailTruncated,
				Seq:     anchor.Seq,
				Detail:  fmt.Sprintf("expected seq %d, have %d", anchor.Seq, maxSeq),
			})
		case maxSeq == anchor.Seq && headDigest != anchor.LastHash:
			rep.Findings = append(rep.Findings, Finding{
				Verdict: VerdictTailTruncated,
				Seq:     anchor.Seq,
				Detail:  "tail hash mismatch",
			})
		}
	}

	return rep
}
