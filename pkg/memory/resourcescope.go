package memory

import (
	"context"
	"fmt"
	"strings"
)

// ScopeKindResource is the Scope.Kind for memory that lives on a RESOURCE
// rather than a session. Its ID is a SpiceDB object reference, "<type>:<id>".
//
// Type-qualified rather than a bare id for two reasons. Audience derivation
// needs the type to know which definition's view_memory to expand; and a bare
// id collides across types the moment two resource types share one.
const ScopeKindResource = "resource"

// PermissionViewMemory is the permission that names a resource pool's
// AUDIENCE: whoever holds it on the resource may see what the pool holds.
//
// It is what a read of pool entries is expanded under — the per-datum tag for a
// pool's entries is minted against `<type>:<id>#view_memory` — and it is
// deliberately NOT the permission the session reached the pool through. A
// session may hold a slot grant of `(customer, read)`; that says the agent may
// work with the resource, and says nothing about who may see the resource's
// memory. Minting under the slot's permission would give the datum whatever
// audience `read` happens to expand to, which nothing downstream would notice.
//
// Named here, beside ScopeKindResource, because the pool scope and the pool
// audience are one design: a scope of this Kind is exactly the set of entries
// this permission governs.
const PermissionViewMemory = "view_memory"

// ResourceScope builds the Scope for one resource instance's memory pool.
//
// Refuses a '/' in either half. The memory capability door keys on scope.ID
// ALONE — httpsrv mints ForBearerToken(perm, id, …) and CompositeSearcher
// checks EnsureApproval(ctx, ReadMemory, sc.ID) — and a session's ID is
// "<ns>/<name>". Allowing a slash here would let an approval minted for a
// resource authorize a session, or the reverse. The two ID shapes staying
// disjoint is a security property, not a formatting preference.
func ResourceScope(objType, objID string) (Scope, error) {
	switch {
	case objType == "":
		return Scope{}, fmt.Errorf("memory: resource scope needs an object type")
	case objID == "":
		return Scope{}, fmt.Errorf("memory: resource scope needs an object id")
	case strings.Contains(objType, ":"), strings.Contains(objID, ":"):
		return Scope{}, fmt.Errorf("memory: resource ref %q:%q must not contain ':' — the separator would be ambiguous", objType, objID)
	case strings.Contains(objType, "/"), strings.Contains(objID, "/"):
		return Scope{}, fmt.Errorf("memory: resource ref %q:%q must not contain '/' — it would collide with a session scope ID", objType, objID)
	}
	return Scope{Kind: ScopeKindResource, ID: objType + ":" + objID}, nil
}

// IsResourceScope reports whether s addresses a resource pool.
func IsResourceScope(s Scope) bool { return s.Kind == ScopeKindResource }

// ResourceRef splits a resource scope back into its SpiceDB object ref.
// ok is true only for an ID that ResourceScope could have produced — a
// resource Kind whose ID is anything else (empty halves, a second ':', an
// embedded '/') is refused rather than half-parsed, since a caller that
// hand-built a Scope must not be able to smuggle a '/' into objType past
// the collision guard ResourceScope enforces.
func ResourceRef(s Scope) (objType, objID string, ok bool) {
	if !IsResourceScope(s) {
		return "", "", false
	}
	// t cannot contain ':' — Cut stops at the first one — so only id needs
	// the ':' check; both need the '/' check ResourceScope also enforces.
	t, id, found := strings.Cut(s.ID, ":")
	if !found || t == "" || id == "" ||
		strings.Contains(t, "/") ||
		strings.Contains(id, ":") || strings.Contains(id, "/") {
		return "", "", false
	}
	return t, id, true
}

// ParseResourceRef parses ref as "<type>:<id>" and returns the Scope
// ResourceScope would build from the two halves.
//
// The one parser every reader of a `resource`/`?pool=`/write-destination
// argument should call, rather than each hand-rolling its own
// strings.Cut — a review of this feature found three call sites doing
// exactly that (record_observation's Execute, httpsrv's destinationFor, the
// info-leakage audience gate's evalPoolWrite) at three different
// strictnesses: two delegated to ResourceScope's validation and one did not,
// so a ref like "dossier:a:b" was a resolvable destination to the gate and a
// refusal to the tool that actually writes. The direction was safe (nothing
// was written), but it is a crack in the exact agreement
// pkg/agent/runner's cross-package pin exists to hold, and it let the gate
// hand a malformed object ref through to a permission lookup. One parser
// closes it structurally: the three sites cannot drift on strictness again
// because there is only one strictness to drift from.
func ParseResourceRef(ref string) (Scope, error) {
	objType, objID, found := strings.Cut(ref, ":")
	if !found {
		return Scope{}, fmt.Errorf(`ref must be "<type>:<id>", got %q`, ref)
	}
	return ResourceScope(objType, objID)
}

// PoolWriter is the capability of writing an entry into a resource's memory
// pool rather than the caller's own scope — httpclient.Client.PutToPool's
// exact signature, named here so every caller that needs to ASSERT for the
// capability (rather than call a concrete *httpclient.Client directly)
// shares one definition instead of each redeclaring an identical anonymous
// interface.
//
// Deliberately NOT part of Memory (see httpclient.Client.PutToPool's own
// doc): Memory.Put routes by e.Scope, and a pool write cannot, since the
// server refuses to read a destination out of the body at all — a caller
// that wants a pool has to say so at the call site, via this interface,
// rather than through the ambient Memory contract every store implements.
//
// A wrapper that decorates a Memory (provenance.SigningMemory) and wants to
// forward a pool write to whatever concrete store it holds type-asserts its
// inner value against this interface — and must fail LOUDLY when that
// assertion fails, naming the concrete inner type, rather than silently
// answering "no" to a capability the inner may genuinely have further down
// a chain of wrappers. A silent "unsupported" answer here is indistinguishable
// from a genuinely pool-incapable backend, which is exactly the shape of bug
// that left record_observation refusing every call in a live runner while
// every test in the tree stayed green: SigningMemory embeds Memory as an
// INTERFACE field, so Go's method promotion could never forward the
// concrete *httpclient.Client's PutToPool.
type PoolWriter interface {
	PutToPool(ctx context.Context, sessionScope, poolScope Scope, e Entry) (Entry, error)
}

// PoolReader is the READ half of the same addressing: query a resource pool's
// entries rather than the caller's own scope.
//
// It exists for the same reason PoolWriter does, and the reason is the same
// shape: over an HTTP transport a scope is not a value the caller passes, it is
// the URL, and that URL names a SESSION (which is what the bearer token
// authorizes). Query builds it from Scope.ID split on "/", so a resource scope
// ID — "dossier:d-1", which has no "/" — produced "/memory/observation/dossier:d-1/"
// and a 404 from the real server. An in-process facade has no such problem; a
// client does, and it has to say so at the call site.
//
// sessionScope is therefore not redundant with poolScope: it is what the
// request is authenticated AS, while poolScope is what it addresses. The server
// proves the session holds a grant on the pool before serving it, exactly as it
// does for a write (httpsrv.destinationFor runs on every method).
//
// The signature takes a whole Query rather than just the pool so a caller can
// filter — the provenance chain-head scan reads one Kind at a time.
type PoolReader interface {
	QueryPool(ctx context.Context, sessionScope, poolScope Scope, q Query) (QueryResult, error)
}
