package memory

import "errors"

var (
	// ErrAppendOnlyConflict: Put attempted to change an existing entry
	// of an append-only Kind. Byte-identical re-puts are not conflicts.
	ErrAppendOnlyConflict = errors.New("append-only kind: entry exists with different content")
	// ErrAppendOnlyKind: per-entry Delete attempted on an append-only
	// Kind (scope-level retention deletion is unaffected).
	ErrAppendOnlyKind = errors.New("append-only kind: entries cannot be deleted")
	// ErrProvenanceRequired: a write to an append-only Kind lacked a Provenance
	// envelope. Raised by the operator's verify-on-write; httpsrv answers 403.
	ErrProvenanceRequired = errors.New("append-only kind: signed provenance required")
	// ErrBadProvenance: the Provenance envelope failed verification.
	ErrBadProvenance = errors.New("append-only kind: provenance verification failed")
	// ErrInvalidEntry: Put refused the Entry on its own merits — an unregistered
	// Kind, or an ID / link ID not carrying its Kind's prefix. It separates a
	// CALLER's bug, which no retry can fix, from a store or authorization fault,
	// which a retry usually does.
	//
	// Load-bearing across the wire: httpsrv maps it to 400 and everything
	// unrecognized to 500, and httpclient retries 5xx while treating 4xx as
	// permanent — so the sentinel is what keeps a transient store fault from
	// reaching the caller as a permanent bad request nobody retries. Wrap every
	// new validation refusal in Put with it; never wrap a fault.
	ErrInvalidEntry = errors.New("memory: invalid entry")
	// ErrInvalidQuery is ErrInvalidEntry's read-side twin: the REQUEST is wrong
	// on its own merits, so no retry can make it succeed — a Query whose
	// FieldEquals.Path names no content key of any Kind it queries (see
	// validateFieldPaths), or a knowledge-graph read whose entity UUID is not
	// UUID-shaped.
	//
	// Same wire weight as ErrInvalidEntry: httpsrv answers 400 and httpclient
	// treats 4xx as permanent. Without it the agent's query_memory tool — whose
	// Kinds and FieldEquals come from LLM-supplied arguments — spent eight
	// retries and ~17s of a turn on a guessed field name before the "did you
	// mean" surfaced. Wrap every read-side refusal that blames the caller;
	// never wrap a store fault.
	ErrInvalidQuery = errors.New("memory: invalid query")
	// ErrKGScopeMismatch: a scoped knowledge-graph read named an entity that
	// belongs to another session's graph. A policy refusal, not a fault —
	// permanent, so httpsrv answers 403 rather than letting the caller retry
	// it eight times. The refusal text deliberately says nothing about which
	// group the entity IS in; see graphiti.GetEntity.
	ErrKGScopeMismatch = errors.New("memory: entity is not in this session's graph")
	// ErrKGUnsupported: a knowledge-graph read this deployment's provider
	// cannot answer at all — Graphiti's self-hosted REST server (graph_service)
	// has no endpoint that returns entities or communities, only facts, and one
	// read (GetEntity) cannot even be scope-checked because its response never
	// carries a group id. Not a fault and not the caller's error — a retry
	// cannot install an endpoint that does not exist, so httpsrv answers 404
	// like ErrNoSearchProviders rather than a retry loop. A richer KGProvider
	// (Graphiti's separate MCP server, or a future backend) might answer the
	// same call; this deployment's cannot.
	ErrKGUnsupported = errors.New("memory: knowledge-graph capability not supported by this deployment")
	// ErrKindNotSessionWritable: a per-session credential tried to author (Put
	// or Delete) a Kind whose WriteAuthority is ComponentWritten. A policy
	// refusal, not a fault — permanent, so httpsrv answers 403 and httpclient
	// does not retry it.
	//
	// The Kind-level half of the write door. The scope-level half
	// (ErrMissingApproval) answers only "may this credential write into this
	// session?", which for a runner's own session is always yes — so without
	// this half a session bearer could author every registered Kind in its
	// scope, including the component-owned records authzd and the operator read
	// to make authorization decisions.
	ErrKindNotSessionWritable = errors.New("memory: kind is not session-writable")
	// ErrKindNotSessionReadable: a per-session credential NAMED a Kind it may
	// not read. The mirror of ErrKindNotSessionWritable, and equally a policy
	// refusal rather than a fault — permanent, so httpsrv answers 403 and
	// httpclient does not retry it.
	//
	// It exists because some Kinds hold data the platform keeps ON BEHALF of a
	// session that the session's own agent must never see — redacted content
	// being the first. Returning an empty result instead would be worse than
	// refusing: the caller would read absence as "there is nothing there" and
	// carry on believing it had looked.
	ErrKindNotSessionReadable = errors.New("memory: kind is not session-readable")
)
