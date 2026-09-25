package toolcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Digest returns the hex sha256 of a tool set, canonicalizing order itself.
//
// Canonicalizing HERE rather than requiring sorted input is deliberate: a
// caller that had to remember to sort first would eventually forget, and the
// symptom would be a spurious catalog change recorded on a turn where nothing
// actually moved.
func Digest(tools []string) string {
	c := slices.Clone(tools)
	sort.Strings(c)
	h := sha256.New()
	for _, t := range c {
		h.Write([]byte(t))
		h.Write([]byte{0}) // separator: {"ab","c"} must not digest as {"a","bc"}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Record stores one catalog change.
//
// Tools are sorted and Digest recomputed here, so the stored content is
// canonical regardless of what the caller passed. The entry id embeds
// FromTurnIndex — NOT the content digest, unlike systemprompt.Record, whose id
// IS its digest and so a conflict there mathematically proves the content
// matches. Here a conflict proves only that SOMETHING is already recorded at
// that turn index; it could be the same change replayed after a restart (the
// idempotent case this Kind depends on), or it could be a genuinely different
// tool set that landed on the same index (e.g. a mid-loop retry that never
// advanced nextIndex, racing a sidecar tool becoming ready). Record
// distinguishes the two by reading the existing entry back and comparing
// digests before deciding a conflict is a no-op.
//
// The Memory passed in MUST be the caller's provenance-signing facade; the
// append-only facade rejects unsigned writes.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	if len(c.Tools) == 0 {
		// An empty catalog would read as real evidence the agent was offered
		// nothing, which is never true — respond_to_user is always present.
		// Composing an empty set is a bug to surface, not a row to write.
		return fmt.Errorf("toolcatalog.Record: refusing to record an empty catalog for %s at turn %d",
			scope.ID, c.FromTurnIndex)
	}
	c.Tools = slices.Clone(c.Tools)
	sort.Strings(c.Tools)
	c.Digest = Digest(c.Tools)

	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("toolcatalog.Record: marshal: %w", err)
	}
	id := IDPrefix + strconv.Itoa(c.FromTurnIndex)
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		// Do NOT assume idempotent — prove it. See the doc comment above for
		// why a conflict on this Kind's id carries no such guarantee by itself.
		existingDigest, rerr := existingDigestFor(ctx, m, scope, id)
		if rerr == nil && existingDigest == c.Digest {
			return nil // genuinely the same catalog, already recorded
		}
		// existingDigestFor returns an error precisely so a read failure is not
		// mistaken for a mismatch — which is exactly what discarding rerr here
		// did. "Different tool set" is a claim about what is stored; when the
		// read-back failed, nobody knows what is stored.
		if rerr != nil {
			return fmt.Errorf("toolcatalog.Record: catalog changed at turn %d but that index is already "+
				"recorded and could NOT be compared against it — the read-back failed, so whether the tool "+
				"sets differ is UNKNOWN (new digest %q; read-back error: %v): %w",
				c.FromTurnIndex, c.Digest, rerr, err)
		}
		return fmt.Errorf("toolcatalog.Record: catalog changed at turn %d but that index is already "+
			"recorded with a different tool set (existing digest %q, new digest %q): %w",
			c.FromTurnIndex, existingDigest, c.Digest, err)
	}
	if err != nil {
		return fmt.Errorf("toolcatalog.Record: %w", err)
	}
	return nil
}

// existingDigestFor reads back, by exact id, the entry a Put just conflicted
// against, and returns its stored Digest. A read failure (including "not
// found", which should not happen right after a genuine conflict but is not
// assumed) returns an error rather than a zero-value digest that could be
// mistaken for a real match.
func existingDigestFor(ctx context.Context, m memory.Memory, scope memory.Scope, id string) (string, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{id}})
	if err != nil {
		return "", fmt.Errorf("read existing entry %s: %w", id, err)
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("conflict on %s but no existing entry found", id)
	}
	var existing Content
	if err := json.Unmarshal(res.Entries[0].Content, &existing); err != nil {
		return "", fmt.Errorf("undecodable existing entry %s: %w", id, err)
	}
	return existing.Digest, nil
}

// List returns every recorded catalog change in scope, ordered by
// FromTurnIndex.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("toolcatalog.List: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// Skip-log rather than fail the whole read: one unreadable
			// append-only row must degrade the catalog, not wedge every reader
			// of the scope forever (the poisoned-turn lesson).
			//
			// The steelthread capture takes the OPPOSITE rule on the same
			// condition (see readDecisions in pkg/steelthread/read.go, which
			// refuses). Both are right for their reader: this one serves a live
			// session that must keep running, so degrading beats wedging; the
			// capture is producing an evidentiary artifact, and one that
			// silently claims less than the run did is worse than no artifact.
			slog.Default().Info("toolcatalog.List: undecodable entry",
				"scope", scope.ID, "id", e.ID, "err", err.Error())
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FromTurnIndex < out[j].FromTurnIndex })
	return out, nil
}

// InForce returns the tool set in force at turnIndex, given every recorded
// change in FromTurnIndex order, or nil when no change covers it.
//
// nil means UNKNOWN, not "no tools". A session captured before this Kind
// shipped has no records at all, and a caller must be able to say nothing about
// what it was offered rather than assert it could call nothing.
//
// Split out of ForTurn because the step function has two readers that must give
// the same answer: this Kind's own reader, over a live memory scope, and the
// steelthread replay, over the same changes carried in a bundle. A second copy
// of "last change at or before the turn" drifts at exactly the boundary that
// matters — the turn a tool set moved on.
func InForce(changes []Content, turnIndex int) []string {
	var got []string
	for _, c := range changes {
		if c.FromTurnIndex > turnIndex {
			break
		}
		got = c.Tools
	}
	return got
}

// ForTurn returns the tool set in force at turnIndex, or nil when no record
// covers it. See InForce for what nil means.
//
// # What reads this, and what does not
//
// The steelthread capture reads this Kind (Records.ToolCatalogs) and carries
// every recorded change into the bundle, where the replay resolves them through
// InForce and holds the run to the recorded set. It still does NOT derive
// bt.Expect.ToolOffered: toolOffered is a claim about which capability MATTERED
// to a scenario, and a capture that picked one would be inventing a claim
// nobody made — the same judgement DeriveAssertions declines to make for
// SystemPromptContains. Pinning the whole set asserts what the run WAS offered
// and invents nothing.
//
// This function is the shape a reader asking about one turn needs — an
// investigator, or a human hand-adding a toolOffered assertion — so that reader
// has one place to ask rather than re-walking List's ordering rules by hand.
func ForTurn(ctx context.Context, m memory.Memory, scope memory.Scope, turnIndex int) ([]string, error) {
	all, err := List(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	return InForce(all, turnIndex), nil
}

// Changed reports whether tools differ from the last recorded catalog. Callers
// hold lastDigest across turns and write only when this returns true.
func Changed(lastDigest string, tools []string) (string, bool) {
	d := Digest(tools)
	return d, !strings.EqualFold(d, lastDigest)
}
