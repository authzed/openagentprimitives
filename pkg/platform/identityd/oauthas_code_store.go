// pkg/platform/identityd/oauthas_code_store.go — TTL'd, single-use stores for
// identityd's OWN OAuth AUTHORIZATION-SERVER role (Tasks 7-8). Same shape as
// oauth_state.go's oauthStateStore, which plays the equivalent role for
// identityd's OPPOSITE role — an OAuth CLIENT of an upstream MCP server. The
// two never share state: a code minted here authenticates a TOOL to
// identityd, not identityd to some other service.
//
// Two stores live here because the authorize/consent split needs two
// different TTL'd handoffs: pendingAuthStore carries the validated
// /oauth/authorize GET params to the /oauth/consent POST (minutes, one
// browser round trip); authCodeStore carries the approved grant to the
// /oauth/token exchange (Task 8), per RFC 6749 §4.1.2's short-lived code.
//
// Both are process-local: a restart mid-flow invalidates every outstanding
// pending-authorize or code, which fails the flow closed and costs only a
// re-attempt.
//
// The same process-locality means pendingAuthStore and authCodeStore ALSO
// cannot be read across replicas: the authorize -> consent -> token sequence
// for one flow must land on the SAME webd replica that minted the pending id
// (and later the code), or the later step's lookup simply misses. Current
// install manifests pin webd to replicas:1, so this is latent today. Running
// webd at >1 replica would require either session affinity on the OAuth AS
// paths (/oauth/authorize, /oauth/consent, /oauth/token) so one flow always
// hits the replica that holds its state, or a future signed-code design that
// makes the pending id / authorization code self-describing and verifiable
// by any replica — mirroring how the OAuth client_id itself is already
// stateless. This does NOT affect /mcp's bearer auth: that check is a
// stateless hash lookup against the AccessToken CR (via the K8s API / its
// informer cache), not against either store here, so /mcp itself is
// replica-safe regardless of webd's replica count.
package identityd

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// authCodeEntry is everything /oauth/token (Task 8) needs to mint an access
// token, bound to one authorization code.
type authCodeEntry struct {
	ClientID    string
	RedirectURI string
	// Challenge is the PKCE S256 code_challenge from /oauth/authorize; the
	// token exchange recomputes SHA256(code_verifier) and compares against it.
	Challenge string
	// Subject is the human who approved the grant at /oauth/consent — proven
	// there, never trusted from the token request.
	Subject identity.CanonicalUserID
	// Role is one of accesstoken.Role{Read,Interact,Full}.
	Role string
	// ScopeClasses is the "ns/name" agent classes the subject scoped the grant
	// to. Empty + Unfiltered=true means "everything this subject can access" —
	// NOT "nothing": the two must never be conflated.
	ScopeClasses []string
	Unfiltered   bool
	// Expires is set by NewCode from the store's TTL at mint time.
	Expires time.Time
}

type authCodeStore struct {
	mu      sync.Mutex
	entries map[string]authCodeEntry
	ttl     time.Duration
}

func newAuthCodeStore(ttl time.Duration) *authCodeStore {
	return &authCodeStore{entries: map[string]authCodeEntry{}, ttl: ttl}
}

// NewCode mints a fresh random authorization code bound to e.
func (s *authCodeStore) NewCode(e authCodeEntry) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e.Expires = time.Now().Add(s.ttl)
	s.entries[code] = e
	return code, nil
}

// Consume removes the entry and returns it. Single-use: a missing,
// already-used, or expired code yields (zero, false), and the code is gone
// either way.
func (s *authCodeStore) Consume(code string) (authCodeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, ok := s.entries[code]
	if !ok {
		return authCodeEntry{}, false
	}
	delete(s.entries, code)
	if time.Now().After(e.Expires) {
		return authCodeEntry{}, false
	}
	return e, true
}

func (s *authCodeStore) gcLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.Expires) {
			delete(s.entries, k)
		}
	}
}

// pendingAuthEntry is the validated /oauth/authorize GET state, carried to
// /oauth/consent via the hidden `pending` form field.
type pendingAuthEntry struct {
	ClientID    string
	ClientName  string
	RedirectURI string
	Challenge   string
	State       string
	// Subject is the cookie subject AT AUTHORIZE TIME; the consent POST
	// requires the posting cookie to still prove this same subject — a
	// pending entry minted for one subject must never be redeemable by
	// another, even if the pending id leaks.
	Subject identity.CanonicalUserID
	// Expires is set by NewPending from the store's TTL at mint time.
	Expires time.Time
}

type pendingAuthStore struct {
	mu      sync.Mutex
	entries map[string]pendingAuthEntry
	ttl     time.Duration
}

func newPendingAuthStore(ttl time.Duration) *pendingAuthStore {
	return &pendingAuthStore{entries: map[string]pendingAuthEntry{}, ttl: ttl}
}

// NewPending mints a fresh random pending id bound to e.
func (s *pendingAuthStore) NewPending(e pendingAuthEntry) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e.Expires = time.Now().Add(s.ttl)
	s.entries[id] = e
	return id, nil
}

// Consume removes the entry and returns it. Single-use, same contract as
// authCodeStore.Consume.
func (s *pendingAuthStore) Consume(id string) (pendingAuthEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, ok := s.entries[id]
	if !ok {
		return pendingAuthEntry{}, false
	}
	delete(s.entries, id)
	if time.Now().After(e.Expires) {
		return pendingAuthEntry{}, false
	}
	return e, true
}

func (s *pendingAuthStore) gcLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.Expires) {
			delete(s.entries, k)
		}
	}
}
