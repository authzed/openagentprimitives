package provenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// ComputeChainHeads scans every append-only kind in scope and returns, per
// publisher, the highest-seq entry's chain head {Seq, EntryDigest(entry)}. It
// anchors tail-truncation detection for ended sessions.
//
// Entries with nil Provenance (unsigned writers) attribute to no publisher and
// are skipped — they anchor nothing. The per-publisher max is taken ACROSS all
// append-only kinds, matching the Signer's per-scope (not per-kind) chain.
func ComputeChainHeads(ctx context.Context, mem memory.Memory, scope memory.Scope) (map[string]ChainHead, error) {
	type best struct {
		seq   uint64
		entry memory.Entry
		set   bool
	}
	byPublisher := map[string]*best{}

	for _, k := range memory.RegisteredKinds() {
		if !k.Retention().AppendOnly {
			continue
		}
		res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{k.Name()}})
		if err != nil {
			// A kind this credential cannot read anchors no chain it could compute
			// a head for — pt_tag_content (SessionReadable()=false) refuses a
			// session credential's read. Skip it, matching Signer.SeedFromMemory;
			// a platform credential still reads and anchors it.
			if errors.Is(err, memory.ErrKindNotSessionReadable) {
				continue
			}
			return nil, fmt.Errorf("compute chain heads: query kind %q in %s: %w", k.Name(), scope.ID, err)
		}
		for _, e := range res.Entries {
			p := e.Provenance
			if p == nil {
				continue // unsigned/legacy entry attributes to no publisher
			}
			b := byPublisher[p.Publisher]
			if b == nil {
				b = &best{}
				byPublisher[p.Publisher] = b
			}
			if !b.set || p.Seq > b.seq {
				b.seq, b.entry, b.set = p.Seq, e, true
			}
		}
	}

	heads := make(map[string]ChainHead, len(byPublisher))
	for publisher, b := range byPublisher {
		heads[publisher] = ChainHead{Seq: b.seq, LastHash: EntryDigest(b.entry)}
	}
	return heads, nil
}
