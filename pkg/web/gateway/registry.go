// Package gateway provides the streaming gRPC service that connects clients
// to active ToolCall exec streams in the operator.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// ActiveStream is the registered state for a streaming ToolCall in flight.
type ActiveStream struct {
	// Namespace and ToolCallName identify the ToolCall this exec was launched
	// for. Together they form the key a connecting client must name in Hello.
	Namespace    string
	ToolCallName string
	// TokenHash is the hex-encoded SHA-256 of the gateway stream token. The
	// operator registers the stream with the hash it read from
	// ToolCall.spec.streamTokenHash; the raw token never reaches the operator.
	TokenHash string
	// Stream is the live exec itself: the stdin/stdout/stderr pipes into the
	// sandbox process, plus Wait and Close. The gateway server pumps the
	// client's bytes through these pipes and nothing else touches them.
	Stream *exec.Stream
	// Cancel stops the underlying exec context. Set for interactive streams
	// so the controller's deletion path can end a live session. nil for
	// plain stream-mode calls.
	Cancel context.CancelFunc

	// claimed records that a client has already connected to this stream.
	// Guarded by Registry.mu. Single-use is enforced by this flag rather than
	// by removing the entry: the sole client is the runner's own bridge, which
	// claims the instant it connects, and the controller still has to reach
	// this stream *after* that to end the session on delete.
	claimed bool
}

// Registry holds all active streaming ToolCalls keyed by "namespace/name".
//
// It is in-process state with no durable backing: the operator that ran the
// exec is the only process that can serve it, and a restart empties the map.
// That is deliberate — an exec has no reattach story — and the controller
// treats an absent key for a Running ToolCall as proof the call was orphaned.
type Registry struct {
	mu      sync.Mutex
	streams map[string]*ActiveStream
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{streams: map[string]*ActiveStream{}}
}

// Register parks an active stream so the gateway can find it. Returns an error
// if a stream already exists for that key: one ToolCall has exactly one exec,
// and re-registering would strand the first one with no route to its client.
func (r *Registry) Register(s *ActiveStream) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := streamKey(s.Namespace, s.ToolCallName)
	if _, exists := r.streams[key]; exists {
		return fmt.Errorf("stream for %s already registered", key)
	}
	r.streams[key] = s
	return nil
}

// Claim looks up a stream by (namespace, toolCallName) and verifies the token.
// The client presents the raw token over the wire; the registry stores only its
// SHA-256 hash and constant-time compares against it — the raw token is never
// persisted server-side. Returns the stream and a "release" function the caller
// must invoke when done. A stream can be claimed at most once (no resume, no
// concurrent connections); a second claim errors.
//
// The claim MARKS the entry rather than deleting it. Deleting would enforce
// single-use too, but would also make the stream unreachable by
// CancelAndUnregister — and since the claiming client is the runner's own
// bridge, which claims the moment it connects, that breaks the idle teardown of
// every interactive session (OnIdle deletes the ToolCall, the controller cancels
// the exec, the cancel unblocks the bridge's Recv loop). The entry is removed by
// Unregister when the exec finishes.
func (r *Registry) Claim(namespace, toolCallName, presentedToken string) (*ActiveStream, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := streamKey(namespace, toolCallName)
	s, ok := r.streams[key]
	if !ok {
		return nil, nil, fmt.Errorf("no active stream for %s", key)
	}
	sum := sha256.Sum256([]byte(presentedToken))
	presentedHash := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(s.TokenHash), []byte(presentedHash)) != 1 {
		return nil, nil, fmt.Errorf("token mismatch")
	}
	if s.claimed {
		return nil, nil, fmt.Errorf("stream for %s is already claimed", key)
	}
	s.claimed = true
	release := func() {
		// no-op: the claim is permanent. A stream is never re-served, so
		// there is nothing to hand back — the caller tears the exec down via
		// Stream.Close(), and the entry leaves the registry when the exec
		// watcher calls Unregister.
	}
	return s, release, nil
}

// Has reports whether this process still holds a live stream for the key.
// The registry is in-process only and is empty after an operator restart, so
// a Running ToolCall whose key is absent here has been orphaned — there is no
// goroutine left to finalize it.
func (r *Registry) Has(namespace, toolCallName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.streams[streamKey(namespace, toolCallName)]
	return ok
}

// Unregister removes an entry without claiming (called when the controller
// detects the stream completed without a client ever connecting).
func (r *Registry) Unregister(namespace, toolCallName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.streams, streamKey(namespace, toolCallName))
}

// CancelAndUnregister stops the exec for an in-flight stream (if it has a
// Cancel func) and removes it from the registry. Safe to call for an unknown
// key (no-op). Used by the controller's deletion path for interactive
// ToolCalls so DELETE actually ends a live session.
func (r *Registry) CancelAndUnregister(namespace, toolCallName string) {
	r.mu.Lock()
	key := streamKey(namespace, toolCallName)
	s, ok := r.streams[key]
	delete(r.streams, key)
	r.mu.Unlock()
	if ok && s.Cancel != nil {
		s.Cancel()
	}
}

func streamKey(ns, name string) string { return ns + "/" + name }
