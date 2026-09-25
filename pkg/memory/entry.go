package memory

import (
	"encoding/json"
	"time"
)

// Scope identifies a sub-universe of entries. Two Kinds are in use: "session",
// which most callers emit, and ScopeKindResource, a memory pool living on a
// resource (see resourcescope.go). The framework permits others ("agentclass",
// "user") for cross-session use, and nothing branches on the value.
type Scope struct {
	// Kind is the sub-universe's type — "session" or ScopeKindResource today.
	Kind string `json:"kind"`
	// ID identifies the scope within its Kind — "<namespace>/<name>" for
	// sessions, "<type>:<id>" for a resource pool. The two shapes are
	// deliberately disjoint; ResourceScope enforces it.
	ID string `json:"id"`
}

// Link is a typed reference to another entry.
type Link struct {
	// Relation is a free-form label from a small conventional vocabulary
	// ("of_operation", "for_resource", "by_subject", "approved_by", …).
	Relation string `json:"relation"`
	// Scope of the target; zero means the containing entry's own scope.
	Scope Scope `json:"scope"`
	// Kind of the target. An unregistered Kind skips ID-prefix validation.
	Kind string `json:"kind"`
	// ID of the target; validated against Kind's IDPrefix when Kind is registered.
	ID string `json:"id"`
}

// Provenance carries the publisher attestation for append-only kinds: a
// per-publisher Ed25519 signature over a canonical entry digest, hash-chained
// per (scope, publisher) so gaps, reordering and truncation are detectable.
// nil on mutable kinds.
type Provenance struct {
	// Publisher identifies the signer; equals the memory layer's caller ID for
	// the writer (per-session IDs for runners/CLI; "system:channelsd",
	// "system:authzd", "system:operator").
	Publisher string `json:"publisher"`
	// KeyID is the hex SHA-256 (first 16 bytes) of the Ed25519 public key, used
	// to look the key up in the publisher-key registry.
	KeyID string `json:"keyId"`
	// Seq is the publisher's per-scope 1-based monotonic sequence, spanning all
	// append-only kinds the publisher writes in the scope. A gap means loss.
	Seq uint64 `json:"seq"`
	// PrevHash is the EntryDigest of the publisher's previous entry in this
	// scope; empty only for Seq 1.
	PrevHash string `json:"prevHash,omitempty"`
	// Sig is the Ed25519 signature over the canonical entry digest.
	Sig []byte `json:"sig"`
}

// Entry is the unit of storage; (Scope, Kind, ID) is the unique key.
type Entry struct {
	// Scope is the sub-universe the entry belongs to; queries never cross it.
	Scope Scope `json:"scope"`
	// Kind names the registered Kind; an unregistered one is refused at Put.
	Kind string `json:"kind"`
	// ID MUST start with the Kind's registered IDPrefix — enforced at Put.
	ID string `json:"id"`
	// CreatedAt is authorship time; canonicalized to microseconds before any
	// sameness comparison, since that is all a TIMESTAMPTZ column keeps.
	CreatedAt time.Time `json:"createdAt"`
	// Links are typed references out of this entry; nil and empty are the same
	// entry to the signed digest.
	Links []Link `json:"links,omitempty"`
	// Tags are an unordered SET — the digest sorts them, so two orderings are
	// one entry; nil and empty are likewise the same.
	Tags []string `json:"tags,omitempty"`
	// Content is the Kind's ContentSchema payload, opaque when it declares none.
	Content json.RawMessage `json:"content,omitempty"`
	// Provenance is nil on mutable kinds and required on append-only ones
	// wherever a ProvenanceVerifier is configured.
	Provenance *Provenance `json:"provenance,omitempty"`
}

// StatusState is the lifecycle state of a Scope as observed by the
// backend.
type StatusState string

const (
	// StatusLive: the backend has (or expects) entries for this scope and
	// intends to serve them.
	StatusLive StatusState = "live"
	// StatusArchived: the backend once held this scope's entries and has since
	// shed them; runner-on-resume surfaces it as "session expired". No shipped
	// backend produces it — time-based archival is not implemented.
	StatusArchived StatusState = "archived"
	// StatusUnknown: no entries the backend can vouch for — brand new or really
	// gone. Callers treat it the same as a fresh scope.
	StatusUnknown StatusState = "unknown"
)

// ScopeStatus is the result of Backend.Status(scope).
type ScopeStatus struct {
	// State is the backend's verdict on the scope.
	State StatusState `json:"state"`
	// ArchivedAt is when the entries were shed; nil unless State is archived.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}
