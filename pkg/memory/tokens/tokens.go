// Package tokens maintains a per-AgentSession bearer-token registry. The
// memory HTTP handler uses Lookup to authenticate; the operator's
// AgentSession reconciler calls Set on EVERY reconcile of a live session and
// Revoke on deletion.
//
// Every map here is process memory: a restarted operator begins with an empty
// registry while the per-session Secret holding each token — and every client
// presenting one — survives untouched. Two things put tokens back. At startup
// the operator restores every bearer it served (each live session's, each
// Ready Workshop's) in one pass (pkg/controllers/tokenrestore), and holds the
// registry in the "restoring" state (BeginRestore / FinishRestore) until that
// pass is done, so an unknown bearer is answered "retry" rather than "refused"
// for exactly as long as the registry is incomplete. And the reconciler
// re-installs a session's token on every pass, not only at creation (see the
// reconciler's reregisterMemoryToken, which reads the durable value back out
// of the Secret). Set on creation alone once left a finished session's memory
// unreadable for the rest of the operator's life, answering every read with a
// bare 401.
package tokens

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TokenInfo holds the session identity and optional caller ID associated with a token.
type TokenInfo struct {
	Session  memory.NamespacedName
	CallerID string
}

type Registry struct {
	mu             sync.RWMutex
	byKey          map[memory.NamespacedName]string              // session → current token
	byToken        map[string]TokenInfo                          // token → primary session + caller
	authzed        map[string]map[memory.NamespacedName]struct{} // token → set of authorized session paths
	channelsdToken string                                        // system-level read+append token
	authzdToken    string                                        // system-level authzd read+append token
	webdToken      string                                        // system-level READ-ONLY token (webd artifact view)
	pubKeys        map[string]map[string]ed25519.PublicKey       // publisher → keyID → key

	// restored is closed once the startup restore pass has run. nil means this
	// registry was never put in the restoring state (every in-process test
	// harness, which Sets tokens synchronously), so it is complete from birth.
	restored   chan struct{}
	finishOnce sync.Once
}

func NewRegistry() *Registry {
	return &Registry{
		byKey:   map[memory.NamespacedName]string{},
		byToken: map[string]TokenInfo{},
		authzed: map[string]map[memory.NamespacedName]struct{}{},
		pubKeys: map[string]map[string]ed25519.PublicKey{},
	}
}

// BeginRestore puts the registry in the restoring state: it holds no tokens
// yet, but the sessions they belong to still exist, and their runners are
// still presenting them. Call it once, at construction, before anything can
// serve a request against the registry.
//
// While restoring, Restoring reports true, and callers that authenticate
// bearers answer an unknown one with a retryable 503 rather than a 401. A 401
// fails a live session for good, and during a restore an unknown bearer is
// far likelier a session not restored yet than a forged token. This replaces
// a fixed post-boot grace, which measured nothing about the restore: on a
// large install the restore outlasted it, and sessions it had not reached yet
// were refused permanently.
func (r *Registry) BeginRestore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.restored == nil {
		r.restored = make(chan struct{})
	}
}

// FinishRestore ends the restoring state. Safe to call more than once, and a
// no-op on a registry that never began restoring.
func (r *Registry) FinishRestore() {
	r.mu.RLock()
	ch := r.restored
	r.mu.RUnlock()
	if ch != nil {
		r.finishOnce.Do(func() { close(ch) })
	}
}

// Restoring reports whether the startup restore is still in progress, so an
// unknown bearer may yet belong to a session whose token is about to return.
func (r *Registry) Restoring() bool {
	r.mu.RLock()
	ch := r.restored
	r.mu.RUnlock()
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

// WaitRestored blocks until the startup restore has finished or ctx ends.
// It returns at once for a registry that never began restoring.
func (r *Registry) WaitRestored(ctx context.Context) error {
	r.mu.RLock()
	ch := r.restored
	r.mu.RUnlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetryWhileRestoring answers w with a retryable 503 + Retry-After when r is
// still restoring, and reports whether it did. A caller that authenticates
// bearers against r calls it on a registry MISS, before refusing the bearer:
// mid-restore, an unknown bearer is far likelier a session not restored yet
// than a forged token, and a 401 would fail that session for good. Once the
// restore is done (or on a registry never put in the restoring state) it
// writes nothing, and the caller's refusal stands. msg is the response body.
func RetryWhileRestoring(w http.ResponseWriter, r *Registry, msg string) bool {
	if r == nil || !r.Restoring() {
		return false
	}
	w.Header().Set("Retry-After", "1")
	http.Error(w, msg, http.StatusServiceUnavailable)
	return true
}

// Set installs token for k, replacing (and invalidating) any prior token.
// callerID is the canonical user ID of the token holder (may be empty).
// extras lists additional session paths the token should authorize for
// artifact READS (e.g. per-bundle SpiceboxSessions whose artifacts the
// AgentSession's runner needs to fetch).
//
// "Reads" is a CONTRACT, not a description: an extra scope is a foreign
// session's data this token may look at, never one it may write into.
// Authorizes and AuthorizesMutation are the two halves of that — a consumer
// answering a mutation with Authorizes grants every extra scope full write
// access.
func (r *Registry) Set(k memory.NamespacedName, token string, callerID string, extras ...memory.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byKey[k]; ok {
		delete(r.byToken, old)
		delete(r.authzed, old)
	}
	r.byKey[k] = token
	r.byToken[token] = TokenInfo{Session: k, CallerID: callerID}
	set := map[memory.NamespacedName]struct{}{k: {}}
	for _, e := range extras {
		set[e] = struct{}{}
	}
	r.authzed[token] = set
}

// Revoke removes any token associated with k. Idempotent.
func (r *Registry) Revoke(k memory.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.byKey[k]; ok {
		delete(r.byToken, old)
		delete(r.authzed, old)
		delete(r.byKey, k)
	}
}

// Lookup resolves token to its primary session.
func (r *Registry) Lookup(token string) (memory.NamespacedName, bool) {
	if token == "" {
		return memory.NamespacedName{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.byToken[token]
	return info.Session, ok
}

// LookupInfo resolves token to its full TokenInfo including CallerID.
func (r *Registry) LookupInfo(token string) (TokenInfo, bool) {
	if token == "" {
		return TokenInfo{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.byToken[token]
	return info, ok
}

// Authorizes reports whether token is registered AND key is one of the
// session paths it was registered for (the primary or any extra).
//
// This is the READ answer. It says "this token may look at key's data"; it does
// NOT say the token may change it — see AuthorizesMutation.
func (r *Registry) Authorizes(token string, key memory.NamespacedName) bool {
	if token == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	set, ok := r.authzed[token]
	if !ok {
		return false
	}
	_, ok = set[key]
	return ok
}

// AuthorizesMutation reports whether token may CHANGE data under key: write an
// entry, delete one, or send a signal into the scope.
//
// Only the token's PRIMARY session qualifies. The extras Set registers are READ
// scopes — a runner holds them to fetch artifacts produced by ToolCalls it
// dispatched against per-bundle SpiceboxSessions, and nothing in the runner
// writes into one. Answering a mutation with Authorizes instead would let a
// runner's token Put and DELETE across every bundle scope it was registered
// with — a strictly wider surface than its own session.
func (r *Registry) AuthorizesMutation(token string, key memory.NamespacedName) bool {
	if token == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.byToken[token]
	return ok && info.Session == key
}

// SetChannelsdToken records the system-level channelsd token. It is accepted
// for GET (read transcript) and POST (append turn) on every session. DELETE
// remains denied — only per-session bearers may delete a session's memory.
// Setting an empty string disables it.
func (r *Registry) SetChannelsdToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channelsdToken = token
}

// IsChannelsdToken returns true if the bearer matches the system channelsd
// token (and the token has been configured). A channelsd token is authorized
// for GET (read transcript) and POST (append turn) on any session; DELETE
// remains denied.
func (r *Registry) IsChannelsdToken(token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.channelsdToken != "" && r.channelsdToken == token
}

// SetAuthzdToken records the system-level authzd token. Accepted for
// GET (read) and POST (append) on every session; DELETE remains denied.
// Empty string disables.
func (r *Registry) SetAuthzdToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authzdToken = token
}

// IsAuthzdToken returns true if the bearer matches the configured
// authzd token.
func (r *Registry) IsAuthzdToken(token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.authzdToken != "" && r.authzdToken == token
}

// SetWebdToken records the system-level webd token. Unlike the channelsd and
// authzd tokens, it is READ-ONLY: accepted for reads on any session (so webd
// can serve the browser artifact view) but never for writes or publisher-key
// registration. webd is browser-facing, so it must not hold a credential that
// can append to or forge the audit log. Empty string disables it.
func (r *Registry) SetWebdToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webdToken = token
}

// IsWebdToken returns true if the bearer matches the configured webd token.
// Callers MUST gate it to read operations only — this method reports identity,
// not authorization. httpsrv does so at both entry points in one vocabulary:
// ServeHTTP derives a route's access from memoryRouteAccess, and the routes
// mounted beside /memory/* pass their access to checkSystemBearer, which
// refuses this token on anything but a read. Reducing the question to "is it a
// system token?" reintroduces the defect this doc exists to prevent
// (/inbound-asset once accepted this token on a POST).
func (r *Registry) IsWebdToken(token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.webdToken != "" && r.webdToken == token
}

// SetPublisherKey registers (or appends) a public key for a publisher
// (caller ID). Old keys remain valid for verifying old entries.
func (r *Registry) SetPublisherKey(publisher, keyID string, pub ed25519.PublicKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pubKeys[publisher] == nil {
		r.pubKeys[publisher] = map[string]ed25519.PublicKey{}
	}
	r.pubKeys[publisher][keyID] = pub
}

// PublisherKey implements provenance.PublisherKeyLookup.
func (r *Registry) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pub, ok := r.pubKeys[publisher][keyID]
	return pub, ok
}

// Registered reports whether k currently has a token installed. It answers
// "does this process still know this session's bearer?", which is the question
// a restarted operator asks before paying to read the token back out of the
// session's Secret — the maps above are process memory and start empty, while
// the Secret and every client holding its value do not.
//
// It deliberately says nothing about WHICH token is installed: the caller that
// re-registers cannot mint, so a true answer means the durable value was
// already installed by whoever minted it.
func (r *Registry) Registered(k memory.NamespacedName) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byKey[k]
	return ok
}
