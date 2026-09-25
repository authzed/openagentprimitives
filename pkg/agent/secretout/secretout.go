// Package secretout is the per-session, out-of-band store for tool-produced
// secret values. A producer tool emits a value here; the store returns an
// opaque handle. The LLM only ever sees handles — never values — so a
// produced credential (e.g. a kubeconfig) never enters the model's context.
// The store is in-process and session-scoped; it is torn down with the runner.
package secretout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// Spec is what a producer tool's Result carries to request out-of-band
// capture. The runner reads it at dispatch, diverts the value to the store,
// and replaces the tool_result Content with a handle + Description.
type Spec struct {
	// Name is the logical name of the secret (e.g. "kubeconfig"). Shown to
	// the LLM/user in the handle line; used as the per-session Secret key.
	Name string
	// Description is freeform human-visible text (gotchas, caveats, usage). It
	// MUST NOT contain the value.
	Description string
}

// Entry is a stored secret value plus its metadata.
type Entry struct {
	// Name is the logical secret name, as given to Put.
	Name string
	// Value is the raw secret bytes — never rendered into an LLM-visible string.
	Value []byte
}

// Store is the per-session secret-output store. Nil-safe at the call site:
// the runner only invokes it when a Result carries a SecretOutput.
type Store interface {
	// Put stores value under a freshly minted opaque handle and returns it.
	Put(ctx context.Context, name string, value []byte) (handle string, err error)
	// Get resolves a handle to its entry; ok=false for unknown handles.
	Get(handle string) (Entry, bool)
}

// SessionStore is the in-memory Store for one runner/session lifetime.
type SessionStore struct {
	sessionUID string
	mu         sync.Mutex
	byHandle   map[string]Entry
}

func NewSessionStore(sessionUID string) *SessionStore {
	return &SessionStore{sessionUID: sessionUID, byHandle: map[string]Entry{}}
}

func (s *SessionStore) Put(_ context.Context, name string, value []byte) (string, error) {
	if name == "" {
		return "", fmt.Errorf("secretout: empty name")
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secretout: mint handle: %w", err)
	}
	h := "so-" + hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHandle[h] = Entry{Name: name, Value: value}
	return h, nil
}

func (s *SessionStore) Get(handle string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byHandle[handle]
	return e, ok
}

var _ Store = (*SessionStore)(nil)
