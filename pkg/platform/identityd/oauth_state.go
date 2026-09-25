// pkg/platform/identityd/oauth_state.go — TTL'd, single-use state-token store
// for the OAuth flow. Same shape as linkStateStore, but each state binds a
// richer entry: PKCE verifier, MCPServer reference, and the cookie subject to
// re-verify on callback.
//
// The store is process-local: a restart mid-flow invalidates every outstanding
// state, which fails the callback closed and costs only a re-link.
package identityd

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// oauthStateEntry is everything the callback needs, bound to one state token and
// looked up by the `state` query parameter.
type oauthStateEntry struct {
	// CredentialName is the credential this flow was STARTED for; the callback
	// refuses any URL naming a different one.
	CredentialName string
	// Subject is the cookie subject at authorize time; the callback requires
	// the presenting cookie to still prove this same subject.
	Subject identity.Subject
	// PKCEVerifier is the SECRET code_verifier sent with the code exchange.
	PKCEVerifier string
	// MCPServerNamespace + MCPServerName re-load the upstream server on
	// callback, in preference to anything the provider echoed back.
	MCPServerNamespace string
	MCPServerName      string
	// ClientID is the DCR-issued client the code was issued for; the exchange
	// MUST reuse it, since re-running DCR mints a different one and the
	// provider then rejects the code as issued to another client.
	ClientID string
	// ClientSecret is the SECRET half of that client; empty for public clients.
	ClientSecret string
	// AgentIdentityRef is "<W>/<agentIdentityName>" — set ONLY by the agent
	// (workshop-credential) authorize flow, never by the per-user one. Its
	// presence is the flow discriminant BOTH callbacks fail closed on: the
	// per-user /oauth/callback/ refuses any entry carrying it, and the agent
	// /oauth/agent-callback/ refuses any entry without it. Without this split,
	// a provider redirect (attacker-influenced — the builder controls
	// MCPServer.spec.server.URL in W) landing on the WRONG callback would
	// redeem the code into the wrong destination: a bot token filed under the
	// starter's own personal UserIdentity, or (the reverse) a person's own
	// token relayed to the operator as an agent-owned write.
	AgentIdentityRef string
	// SessionRef is "<B>/<X>" — the BUILDER session that requested this
	// credential, carried only on the agent flow. The agent callback re-runs
	// the workshop-starter authority check against it (the SAME fact the
	// authorize step already checked), rather than trusting that the
	// permission granted at authorize time still holds by the time the
	// provider redirects back.
	SessionRef string
	// createdAt anchors the store's TTL; entries past it are treated as absent.
	createdAt time.Time
}

type oauthStateStore struct {
	mu      sync.Mutex
	entries map[string]oauthStateEntry
	ttl     time.Duration
}

func newOAuthStateStore(ttl time.Duration) *oauthStateStore {
	return &oauthStateStore{entries: map[string]oauthStateEntry{}, ttl: ttl}
}

// NewState binds entry to a fresh random token for the authorize URL's `state`.
func (s *oauthStateStore) NewState(entry oauthStateEntry) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	entry.createdAt = time.Now()
	s.entries[tok] = entry
	return tok, nil
}

// Consume removes the entry and returns it. Single-use: a missing, already-used,
// or expired token yields (zero, false), and the token is gone either way.
func (s *oauthStateStore) Consume(tok string) (oauthStateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, ok := s.entries[tok]
	if !ok {
		return oauthStateEntry{}, false
	}
	delete(s.entries, tok)
	if time.Since(e.createdAt) > s.ttl {
		return oauthStateEntry{}, false
	}
	return e, true
}

func (s *oauthStateStore) gcLocked() {
	cutoff := time.Now().Add(-s.ttl)
	for k, e := range s.entries {
		if e.createdAt.Before(cutoff) {
			delete(s.entries, k)
		}
	}
}
