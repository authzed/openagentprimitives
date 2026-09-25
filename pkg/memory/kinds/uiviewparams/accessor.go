package uiviewparams

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Content is the agent's binding-parameter choices for one UI.
type Content struct {
	// UI is the AgentUI CR name. The namespace is already the memory scope's
	// (scope.ID is "<ns>/<session>"), so it is not repeated here.
	UI string `json:"ui"`

	// Params maps a runtime parameter KEY to the value the agent chose —
	// "window.from", not "window" (see uicomponents.ParamKeys for why the two
	// differ). Whole-map replacement, never a merge: the agent states the set
	// of controls it is driving, and a key it stops naming is a control it has
	// stopped driving. A merge would make an agent-set filter impossible to
	// clear, since there would be no way to express "no longer mine".
	Params map[string]string `json:"params"`

	// WrittenAt is when the agent set them. Presentation and diagnostics only
	// — the record is not part of any signed chain (see Kind.Retention).
	WrittenAt time.Time `json:"writtenAt"`
}

// EntryID is the deterministic memory Entry.ID for one ui: a hex-truncated
// SHA-256 of the name.
//
// Hashed for the same reason uiviewmodel.EntryID hashes: an AgentUI CR name is
// DNS-1123 today, but building an entry ID out of a name is how an ID's
// charset becomes an accident of somebody's bundle. Content.UI carries the
// readable name for everything a human reads.
func EntryID(ui string) string {
	sum := sha256.Sum256([]byte(ui))
	return IDPrefix + hex.EncodeToString(sum[:])[:32]
}

// Record writes (or replaces) one UI's parameter choices. Errors are RETURNED,
// never swallowed: a lost write is a page the agent believes it set up and did
// not, which the viewer meets as a dashboard that ignored what they asked for.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("uiviewparams.Record: marshal %s: %w", c.UI, err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        EntryID(c.UI),
		CreatedAt: c.WrittenAt,
		Content:   raw,
	}); err != nil {
		return fmt.Errorf("uiviewparams.Record: put %s: %w", c.UI, err)
	}
	return nil
}

// Get returns the stored parameter choices for one UI, or an empty map when
// the agent has never set any.
//
// "Never set any" is NOT an error and must not be reported as one: it is the
// ordinary state of every UI whose viewer drives its own controls, which is
// most of them. Treating absence as a failure would make the common case noisy
// and hide the real failures in that noise.
func Get(ctx context.Context, m memory.Memory, scope memory.Scope, ui string) (map[string]string, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("uiviewparams.Get: query: %w", err)
	}
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// A record that will not decode is a page that silently ignores the
			// window its viewer asked for — returned, never skipped, per
			// AGENTS.md's no-silent-errors rule.
			return nil, fmt.Errorf("uiviewparams.Get: unmarshal %q: %w", e.ID, err)
		}
		if c.UI != ui {
			continue
		}
		if c.Params == nil {
			return map[string]string{}, nil
		}
		return c.Params, nil
	}
	return map[string]string{}, nil
}
