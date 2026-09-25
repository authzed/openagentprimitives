// Package sentinel is the single source of truth for how a memory sentinel
// error crosses the HTTP wire, in both directions: httpsrv reads Table to pick
// the response status and stamp Header; httpclient reads it to rebuild the
// sentinel from Header. Neither holds a switch of its own to fall out of date.
//
// Both halves are load-bearing. The status decides retry behaviour —
// httpclient retries 5xx eight times over a ~17s backoff budget and treats 4xx
// as permanent. The reconstruction decides whether errors.Is still matches once
// a call crosses a process boundary: the runner's memory IS an httpclient, so a
// sentinel with no row arrives as an opaque status error and every errors.Is
// test against it on the live path silently answers false.
//
// # Adding a sentinel
//
// Add a row here. The drift test (sentinel_test.go) fails if a memory sentinel
// exists with no row and no explicit "not on the wire" entry, so the decision
// cannot be made by omission.
package sentinel

import (
	"errors"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Header carries the machine-readable discriminator naming which sentinel a
// response represents.
//
// The status alone cannot identify one: 403 is answered by TWO sentinels
// (ErrMissingApproval, ErrKGScopeMismatch) and also by refusals that are no
// sentinel at all — handleKG's approval door, the read-only-token gate, a
// cross-session token, any intermediary proxy. Inferring a sentinel from "403"
// would hand the caller a forged authorization verdict; 400 and 404 are
// ambiguous the same way (a body-decode failure, an unroutable path).
//
// A header rather than a JSON error envelope keeps the plain-text bodies
// httpsrv writes (http.Error) intact for the callers and operators that read
// them as text. A server too old to send the header simply produces no
// reconstruction — the fail-toward-unknown direction a mixed-version cluster
// needs.
const Header = "X-Memory-Sentinel"

// Mapping is one sentinel's complete wire contract.
type Mapping struct {
	// Err is the sentinel itself. Both directions test with errors.Is, so a
	// wrapped sentinel matches and keeps its wrapper's message.
	Err error
	// Status is the HTTP status httpsrv answers Err with. Every one is 4xx:
	// these are permanent verdicts (a caller bug, a policy refusal, a
	// deployment fact), and no retry can produce a better answer. That is the
	// same property that makes their text worth guaranteeing delivery of.
	Status int
	// Code is Header's value — a stable, machine-readable name. It is wire
	// format: never rename one, only add.
	Code string
}

// Table is the mapping, in match order. Order is only consulted for an error
// that wraps two sentinels, which no construction in this repo produces.
var Table = []Mapping{
	{
		// The request is wrong on its own merits — a FieldEquals path that
		// names no content key, a KG entity id that is not UUID-shaped.
		Err:    memory.ErrInvalidQuery,
		Status: http.StatusBadRequest,
		Code:   "invalid_query",
	},
	{
		// No capability approval for this scope. Permanent; retrying would
		// look like a brute-force loop in the log.
		Err:    memory.ErrMissingApproval,
		Status: http.StatusForbidden,
		Code:   "missing_approval",
	},
	{
		// The named entity belongs to another session's graph. A policy
		// refusal, permanent, and deliberately not an oracle for which graph.
		Err:    memory.ErrKGScopeMismatch,
		Status: http.StatusForbidden,
		Code:   "kg_scope_mismatch",
	},
	{
		// The deployment configured no search backend. Not a fault and not the
		// caller's error; a retry cannot install one.
		Err:    memory.ErrNoSearchProviders,
		Status: http.StatusNotFound,
		Code:   "no_search_providers",
	},
	{
		// This deployment's KGProvider has no endpoint that can answer the
		// call. Not a fault and not the caller's error, same family as
		// ErrNoSearchProviders — a retry cannot install a capability the
		// backend does not have.
		Err:    memory.ErrKGUnsupported,
		Status: http.StatusNotFound,
		Code:   "kg_unsupported",
	},
	{
		// The Kind is component-written and the writer presented a per-session
		// credential. A settled policy verdict about WHO may author the Kind —
		// no retry, and no different credential the runner could present. On the
		// wire (rather than in notOnTheWire) because a caller that cannot rebuild
		// it sees an opaque 403 indistinguishable from a cross-session refusal or
		// a proxy's.
		Err:    memory.ErrKindNotSessionWritable,
		Status: http.StatusForbidden,
		Code:   "kind_not_session_writable",
	},
	{
		// The read-side twin, and on the wire for the same reason: the runner's
		// memory IS an HTTP client, so a refusal that arrives as an opaque 403
		// cannot be told apart from a cross-session denial or a proxy's, and
		// errors.Is stops matching for every caller that has one.
		//
		// Permanent, like its twin — no retry will help, and no other
		// credential the runner could present would either, because the point
		// is that a session credential is the wrong kind of caller for this
		// Kind.
		Err:    memory.ErrKindNotSessionReadable,
		Status: http.StatusForbidden,
		Code:   "kind_not_session_readable",
	},
}

// StatusAndCode reports the wire status and discriminator for err — the SERVER
// direction. ok is false for anything that is not a sentinel, which the caller
// must treat as a fault (5xx) so the client's backoff can ride it out.
func StatusAndCode(err error) (status int, code string, ok bool) {
	for _, m := range Table {
		if errors.Is(err, m.Err) {
			return m.Status, m.Code, true
		}
	}
	return 0, "", false
}

// ForCode returns the sentinel a wire code names — the CLIENT direction.
//
// Lookup is by code alone, never by status, and an unknown or empty code yields
// ok=false. Only httpsrv sets Header, so a 403 from an intermediary or from one
// of the server's non-sentinel refusals stays out of the sentinel space.
func ForCode(code string) (error, bool) {
	if code == "" {
		return nil, false
	}
	for _, m := range Table {
		if m.Code == code {
			return m.Err, true
		}
	}
	return nil, false
}
