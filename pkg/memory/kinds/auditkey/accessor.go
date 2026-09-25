package auditkey

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Witness records that the AgentSession instance sessionUID signs scope's
// append-only records with the key (keyID, pubKeyB64). m MUST be the operator's
// signing facade — the record's whole value is that a trusted component
// publisher attested it.
//
// at is the entry's createdAt and MUST be a value the caller can reproduce (the
// AgentSession's creationTimestamp, not time.Now): the entry ID is derived from
// keyID, so a re-record of the same binding is a re-put of an existing
// append-only entry, and only a byte-identical one is idempotent. A wall-clock
// stamp would make every reconcile an ErrAppendOnlyConflict instead.
func Witness(ctx context.Context, m memory.Memory, scope memory.Scope, sessionUID, keyID, pubKeyB64 string, at time.Time) error {
	if keyID == "" || pubKeyB64 == "" {
		return fmt.Errorf("auditkey: refusing to witness an empty key binding (keyID=%q, pubKey set=%t)", keyID, pubKeyB64 != "")
	}
	content, err := json.Marshal(Content{SessionUID: sessionUID, KeyID: keyID, PubKey: pubKeyB64})
	if err != nil {
		return fmt.Errorf("auditkey: marshal key binding for %s: %w", scope.ID, err)
	}
	if _, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      KindName,
		ID:        IDPrefix + keyID,
		CreatedAt: at.UTC(),
		Content:   content,
	}); err != nil {
		return fmt.Errorf("auditkey: witness key %s for %s: %w", keyID, scope.ID, err)
	}
	return nil
}

// Entries returns every key-binding record stored in scope, provenance intact.
// Raw entries, not decoded Contents: a caller must verify the signature on the
// record before it may believe the binding inside it, and that needs the
// envelope.
func Entries(ctx context.Context, m memory.Memory, scope memory.Scope) ([]memory.Entry, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, fmt.Errorf("auditkey: query key bindings in %s: %w", scope.ID, err)
	}
	return res.Entries, nil
}

// Decode reads the key binding out of one stored record.
func Decode(e memory.Entry) (Content, error) {
	var c Content
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return Content{}, fmt.Errorf("auditkey: decode key binding %s: %w", e.ID, err)
	}
	return c, nil
}
