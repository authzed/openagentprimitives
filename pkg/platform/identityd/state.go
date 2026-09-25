package identityd

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// maxLinkStates caps the in-flight state table.
//
// Every anonymous hit on /oidc/login inserts an entry for the full TTL, and
// the signed link needed to reach it is handed out by a cookie-less GET on
// /admin and is replayable for that window — so without a cap the only bound
// is how fast an attacker can issue requests, against a pod capped at 256Mi.
// TTL eviction does not help: the whole flood fits inside one TTL window.
//
// Sized well above any real concurrent-login count, so the cap is reached
// only by abuse.
const maxLinkStates = 4096

// maxNextLen bounds the caller-supplied post-auth destination.
//
// `next` was stored BEFORE any validation — safeNext runs at redirect time —
// so a request-line-sized query string was retained per entry for the TTL.
// A legitimate value is a path on this host; anything approaching this length
// is not one.
const maxNextLen = 2048

// linkStateStore is the in-memory CSRF/anti-replay store for OIDC link flows.
// Each state token maps to the original signed link the caller minted, so the
// OIDC callback can redirect back to the right /link?d=...&sig=... afterwards.
//
// Tokens are 32 random hex chars, expire after 10 minutes, and are GC'd lazily
// on access. In-memory only: identityd is single-replica, and a restart merely
// loses pending flows (the user re-clicks the deep-link). Multi-replica would
// need a shared store.
type linkStateStore struct {
	mu      sync.Mutex
	entries map[string]linkStateEntry
	ttl     time.Duration
}

type linkStateEntry struct {
	linkRaw string // the original "<b64>.<sig>" signed link from passthroughlink.Signer.Mint
	next    string // optional post-auth destination (the generic /oidc/login ?next=); "" ⇒ reconstruct /link from linkRaw
	// authenticator names WHICH sign-in path minted this state: a channel-kind
	// name, or "idp" for the cluster IdP's own callback.
	//
	// The callback route takes its kind from the URL (/oidc/callback/{kind}),
	// and a state carried no record of where it came from -- so a state minted
	// for the IdP flow was redeemable at a channel-kind callback, and vice
	// versa. Whichever authenticator Complete() runs then interprets a code
	// issued for a different one. Recording the minter is what makes
	// "ignore the kind override" a sufficient fix rather than half of one.
	authenticator string

	// binding is the per-flow nonce the begin handler also wrote to the
	// browser's loginBindingCookie. Consume requires it back.
	//
	// `state` alone is anti-replay, never proof of WHO is walking the flow, and
	// whoever holds the value can finish it in any browser. That is login CSRF:
	// an attacker starts a flow, authenticates at the IdP as themselves to get a
	// code, and lures the victim to the callback with that pair — the victim's
	// browser is then issued a session for the ATTACKER's subject, and anything
	// the victim connects afterwards lands in the attacker's account. The
	// attacker can hand over a state; they cannot write the victim's cookie jar.
	binding string
	created time.Time
}

func newLinkStateStore() *linkStateStore {
	return &linkStateStore{
		entries: map[string]linkStateEntry{},
		ttl:     10 * time.Minute,
	}
}

// NewState returns a fresh random state token bound to linkRaw and to the
// browser holding `binding`, with no post-auth destination override.
// NewStateWithNext(linkRaw, "", binding).
func (s *linkStateStore) NewState(linkRaw, binding, authenticator string) (string, error) {
	return s.NewStateWithNext(linkRaw, "", binding, authenticator)
}

// NewStateWithNext is NewState plus a post-auth destination: the ?next=<url>
// the generic /oidc/login entry was started with. Once the cookie is set the
// OIDC callback redirects there; an empty next means reconstruct /link from
// linkRaw instead.
//
// binding is the per-flow nonce the caller ALSO wrote to the browser's
// loginBindingCookie; Consume requires it back. It is a required parameter, not
// an option, so a login entry point added later cannot mint an unbound state
// without deleting an argument — see linkStateEntry.binding for the attack this
// closes.
func (s *linkStateStore) NewStateWithNext(linkRaw, next, binding, authenticator string) (string, error) {
	// Bound the retained value at the door rather than at redirect time.
	if len(next) > maxNextLen {
		return "", fmt.Errorf("identityd: next destination is %d bytes, over the %d-byte limit", len(next), maxNextLen)
	}
	if binding == "" {
		return "", fmt.Errorf("identityd: refusing to mint a login state with no browser binding")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err // io errors are real; surface
	}
	tok := hex.EncodeToString(b[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.evictOldestLocked(maxLinkStates - 1)
	s.entries[tok] = linkStateEntry{linkRaw: linkRaw, next: next, binding: binding, authenticator: authenticator, created: time.Now()}
	return tok, nil
}

// stateRefusal says why Consume declined, so the caller can tell "you were
// slow" from "this browser did not start this login" — very different
// diagnoses, and only the second is a sign someone is being lured. Returning a
// bare bool made them one log line.
type stateRefusal string

const (
	// stateAccepted is the zero value so a caller comparing against it cannot
	// accidentally read an unset refusal as a rejection.
	stateAccepted stateRefusal = ""
	// stateMissing: no such state, or it expired, or it was already spent.
	stateMissing stateRefusal = "missing_expired_or_used"
	// stateWrongBrowser: the state exists but the caller does not hold the
	// binding it was minted with — the login-CSRF signature.
	stateWrongBrowser stateRefusal = "wrong_browser"
	// stateWrongAuthenticator: the state was minted by a DIFFERENT sign-in path
	// than the callback redeeming it. The callback route takes its kind from the
	// URL, so without this an IdP-path state is redeemable at a channel-kind
	// callback and vice versa, and whichever authenticator runs then interprets a
	// code issued for another one.
	stateWrongAuthenticator stateRefusal = "wrong_authenticator"
)

// Consume looks up + removes the state entry. Returns the bound linkRaw,
// its optional post-auth `next` destination, and true on success;
// stateAccepted on success; stateMissing or stateWrongBrowser otherwise
// (single-use either way).
//
// The entry is deleted BEFORE the binding is compared, deliberately: a refused
// attempt still spends the state, so an attacker cannot keep re-luring victims
// at one token until a browser happens to hold a matching cookie. The right
// browser starting over costs one redirect.
func (s *linkStateStore) Consume(token, binding, authenticator string) (linkRaw, next string, refusal stateRefusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, found := s.entries[token]
	if !found {
		return "", "", stateMissing
	}
	delete(s.entries, token)
	if time.Since(e.created) > s.ttl {
		return "", "", stateMissing
	}
	if binding == "" || !hmac.Equal([]byte(binding), []byte(e.binding)) {
		return "", "", stateWrongBrowser
	}
	if authenticator != e.authenticator {
		return "", "", stateWrongAuthenticator
	}
	return e.linkRaw, e.next, stateAccepted
}

// Valid reports whether token exists and has not expired, WITHOUT consuming
// it. The password-login form render (GET /password/login) needs the check so
// a stale/forged state never gets a real form, but must not spend the token:
// rendering the form is not the security-relevant action, submitting the
// password is, and POST /password/verify is what calls Consume.
func (s *linkStateStore) Valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	e, found := s.entries[token]
	if !found {
		return false
	}
	return time.Since(e.created) <= s.ttl
}

// len reports the current entry count. Test seam for the cap.
func (s *linkStateStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// evictOldestLocked trims the table to at most keep entries, dropping the
// oldest first so a flood cannot push out a legitimate login that started
// moments before it.
func (s *linkStateStore) evictOldestLocked(keep int) {
	if len(s.entries) <= keep {
		return
	}
	type aged struct {
		tok     string
		created time.Time
	}
	all := make([]aged, 0, len(s.entries))
	for k, v := range s.entries {
		all = append(all, aged{tok: k, created: v.created})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].created.Before(all[j].created) })
	for i := 0; i < len(all)-keep; i++ {
		delete(s.entries, all[i].tok)
	}
}

func (s *linkStateStore) gcLocked() {
	cutoff := time.Now().Add(-s.ttl)
	for k, v := range s.entries {
		if v.created.Before(cutoff) {
			delete(s.entries, k)
		}
	}
}

// idpAuthenticator is the reserved authenticator name for the cluster IdP's own
// flow — the "idp" suffix on /oidc/callback/idp, which is not a channel kind.
//
// Named rather than spelled at each call site, because it is compared: a state
// minted by the IdP path must be redeemed at the IdP callback and nowhere else.
const idpAuthenticator = "idp"
