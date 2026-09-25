package identityd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// cliCodeTTL is the lifetime of a one-time CLI exchange code. Short by design:
// the code travels a loopback redirect to an already-running CLI process, so
// 60s is generous, and anyone able to intercept loopback within that window
// already has local access.
const cliCodeTTL = 60 * time.Second

// cliCodeEntry holds the state for a pending CLI exchange.
type cliCodeEntry struct {
	principal  identity.Principal
	sessionTTL time.Duration
	cliState   string // the opaque state the CLI generated; verified on exchange
	expires    time.Time
}

// cliCodeStore is an in-memory, single-use store for CLI one-time codes. Codes
// expire after cliCodeTTL and expired entries die lazily on Consume; no sweeper
// is needed, since the store is bounded by the CLI-login rate.
type cliCodeStore struct {
	mu      sync.Mutex
	entries map[string]cliCodeEntry
	// now is a test seam; production uses time.Now.
	now func() time.Time
}

func newCLICodeStore() *cliCodeStore {
	return &cliCodeStore{
		entries: map[string]cliCodeEntry{},
		now:     time.Now,
	}
}

// Create mints a 32-random-byte hex code bound to principal + cliState, with a
// cliCodeTTL expiry. sessionTTL rides along so handleCLIExchange can set the
// assertion's exp without re-loading the IdP CR.
func (s *cliCodeStore) Create(p identity.Principal, sessionTTL time.Duration, cliState string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[code] = cliCodeEntry{
		principal:  p,
		sessionTTL: sessionTTL,
		cliState:   cliState,
		expires:    s.now().Add(cliCodeTTL),
	}
	return code, nil
}

// Consume removes the code from the store and returns its entry only if the
// code exists, has not expired, AND cliState matches. Every failure returns the
// same (zero, false) so the caller's single generic 403 cannot tell a prober
// which leg failed.
//
// The map key IS the secret, so a miss is O(1) with no timing side-channel to
// exploit. cliState is compared with plain == because it is not secret-bearing:
// the CLI generates it and it rides the loopback redirect URL in the clear.
func (s *cliCodeStore) Consume(code, cliState string) (cliCodeEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[code]
	if !ok {
		return cliCodeEntry{}, false
	}
	delete(s.entries, code) // single-use regardless of expiry/state checks
	if s.now().After(e.expires) {
		return cliCodeEntry{}, false
	}
	if e.cliState != cliState {
		return cliCodeEntry{}, false
	}
	return e, true
}

// ---- HTTP handlers ----------------------------------------------------------

// handleCLILogin handles GET /cli/login?state=<cliState>&port=<n>.
//
// The CLI opens a browser tab here. identityd runs the ordinary cluster-IdP
// OIDC flow but stores a CLI marker in the state entry's `next` field instead
// of a same-origin path; handleIdPCallback then mints a one-time code and
// redirects the tab to the CLI's loopback listener:
//
//	http://127.0.0.1:<port>/callback?code=<code>&state=<cliState>
//
// No browser cookie is ever set on this path.
func (s *Server) handleCLILogin(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())
	ctx := r.Context()
	q := r.URL.Query()

	cliState := q.Get("state")
	portStr := q.Get("port")

	if cliState == "" {
		s.writeError(w, r, http.StatusBadRequest, "Invalid request", "Missing required parameter: state.")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1024 || port > 65535 {
		s.writeError(w, r, http.StatusBadRequest, "Invalid request",
			"Parameter 'port' must be an integer in [1024, 65535].")
		return
	}

	resolved, err := s.idp.Current(ctx)
	if err != nil {
		if errors.Is(err, ErrIdPNotConfigured) {
			s.writeError(w, r, http.StatusServiceUnavailable, "No identity provider",
				"This cluster has no identity provider configured. Ask your administrator to run `oap idp setup`.")
			return
		}
		logger.Info("cli login: cluster IdP misconfigured", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"The cluster identity provider is misconfigured. Ask your administrator to run `oap idp status`.")
		return
	}

	// The "cli|" prefix is parsed by parseCLINext in the IdP callback, and is
	// never a valid safeNext path (it doesn't start with "/"), so
	// redirectNextOrLink can never use it as a redirect target.
	next := "cli|" + portStr + "|" + cliState
	stateTok, err := s.beginLoginState(w, "", next, idpAuthenticator)
	if err != nil {
		logger.Info("cli login: could not start a browser-bound login state", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
		return
	}

	redirect, err := resolved.provider.Begin(ctx, stateTok)
	if err != nil {
		logger.Info("cli login: idp provider Begin failed", "err", err.Error())
		s.writeError(w, r, http.StatusInternalServerError, "Sign-in unavailable", "Could not start sign-in.")
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// parseCLINext parses the "cli|<port>|<cliState>" marker stored in the
// state-store `next` field. Strict: exactly the literal "cli", a decimal port
// in [1024,65535], and a non-empty cliState, else (_, _, false).
func parseCLINext(next string) (portStr, cliState string, ok bool) {
	const prefix = "cli|"
	if !strings.HasPrefix(next, prefix) {
		return "", "", false
	}
	rest := next[len(prefix):]
	idx := strings.IndexByte(rest, '|')
	if idx <= 0 {
		return "", "", false
	}
	portStr = rest[:idx]
	cliState = rest[idx+1:]
	if cliState == "" {
		return "", "", false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1024 || port > 65535 {
		return "", "", false
	}
	return portStr, cliState, true
}

// cliExchangeRequest is the JSON body POST /cli/exchange accepts.
type cliExchangeRequest struct {
	// Code is the SECRET single-use code from the loopback redirect; spent on first use.
	Code string `json:"code"`
	// State is the CLI-generated state echoed back, and must match the one bound to Code.
	State string `json:"state"`
}

// cliExchangeResponse is the JSON body POST /cli/exchange returns on success.
type cliExchangeResponse struct {
	// Assertion is the SECRET signed identity assertion the CLI presents as a bearer credential.
	Assertion string `json:"assertion"`
	// Subject is the BARE canonical (no "user:" prefix); the signed Assertion carries the prefixed form.
	Subject identity.CanonicalUserID `json:"subject"`
	// Email is the IdP-verified email the canonical was derived from.
	Email string `json:"email"`
	// DisplayName is the IdP-supplied human name, for CLI output only.
	DisplayName string `json:"displayName"`
	// ExpiresAt is the assertion's expiry as Unix seconds.
	ExpiresAt int64 `json:"expiresAt"`
}

// jsonError writes a JSON {"error":"<msg>"} response.
func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleCLIExchange handles POST /cli/exchange: the CLI posts the one-time code
// from its loopback redirect, identityd spends the code and returns a signed
// identity assertion. No browser cookie is set.
//
// This serves a program, not a browser, so every response is JSON (never the
// HTML writeError), and the body is capped at 4 KB.
func (s *Server) handleCLIExchange(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req cliExchangeRequest
	if err := dec.Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Code == "" || req.State == "" {
		jsonError(w, http.StatusBadRequest, "both 'code' and 'state' are required")
		return
	}

	entry, ok := s.cliCodes.Consume(req.Code, req.State)
	if !ok {
		// Single generic message: don't distinguish expired/wrong-code/wrong-state.
		jsonError(w, http.StatusForbidden, "invalid or expired code")
		return
	}

	// entry.principal is an IdP-verified login (email present), so
	// canonicalization never hits the synthetic guard — fail closed on the
	// impossible error rather than assert an unresolved subject.
	canon, err := entry.principal.Canonical()
	if err != nil {
		logger.Info("cli exchange: principal canonicalization failed", "err", err.Error())
		jsonError(w, http.StatusInternalServerError, "failed to resolve identity")
		return
	}
	subject := canon.Subject()

	expiresAt := s.cliCodes.now().Add(entry.sessionTTL).Unix()
	raw, err := s.deps.LinkSigner.Mint(passthroughlink.Payload{
		Subject:         subject,
		SubjectVerified: true,
		Audience:        passthroughlink.AudienceCLIIdentity,
		Purpose:         passthroughlink.PurposeCLIIdentity,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		logger.Info("cli exchange: assertion mint failed", "err", err.Error())
		jsonError(w, http.StatusInternalServerError, "failed to mint assertion")
		return
	}

	resp := cliExchangeResponse{
		Assertion:   raw,
		Subject:     canon,
		Email:       entry.principal.Email().String(),
		DisplayName: entry.principal.DisplayName(),
		ExpiresAt:   expiresAt,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Info("cli exchange: response encode failed", "err", err.Error())
	}
}
