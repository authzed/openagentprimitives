package systemprompt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Digest returns the hex sha256 of a prompt. Exported so a caller can compare a
// recorded digest against a locally-composed prompt without re-deriving the
// hashing rule and getting it subtly wrong.
func Digest(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Record stores the prompt under its digest, once.
//
// Set-once is achieved by DERIVING the entry id from the digest rather than
// minting a fresh one: an unchanged prompt re-recorded on a later turn lands on
// the same id with the same content, which the append-only facade treats as an
// idempotent re-put. A CHANGED prompt gets a different id and is a new entry, so
// a mid-session change is visible rather than overwriting the original.
//
// ErrAppendOnlyConflict is not an error here, and that is the subtle part: the
// id is a function of the content, so a conflict on it means the same prompt was
// already recorded and only the entry's CreatedAt differs (entriesEquivalent
// compares it). The first write's timestamp is the correct one — it is when the
// prompt was first seen — so the later write is genuinely a no-op.
//
// The Memory passed in MUST be the caller's provenance-signing facade; the
// append-only facade rejects unsigned writes.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, prompt string) error {
	if prompt == "" {
		// A digest of "" would read as real evidence that the agent ran under
		// empty instructions. Compose producing nothing is a bug to surface,
		// not a row to write.
		return fmt.Errorf("systemprompt.Record: refusing to record an empty prompt for %s", scope.ID)
	}
	digest := Digest(prompt)
	raw, err := json.Marshal(Content{Digest: digest, Prompt: prompt})
	if err != nil {
		return fmt.Errorf("systemprompt.Record: marshal: %w", err)
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        Kind{}.IDPrefix() + digest[:32],
		CreatedAt: time.Now().UTC(),
		Content:   raw,
	})
	if errors.Is(err, memory.ErrAppendOnlyConflict) {
		return nil // same prompt, already recorded
	}
	if err != nil {
		return fmt.Errorf("systemprompt.Record: %w", err)
	}
	return nil
}

// List returns every recorded prompt in scope, oldest-first.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]Content, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{Kind{}.Name()}})
	if err != nil {
		return nil, fmt.Errorf("systemprompt.List: %w", err)
	}
	out := make([]Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// Skip-log rather than drop silently: an undecodable append-only row
			// is itself a finding.
			slog.Default().Info("systemprompt.List: undecodable entry",
				"scope", scope.ID, "id", e.ID, "err", err.Error())
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
