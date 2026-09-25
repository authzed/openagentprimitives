// Package provenance signs and verifies append-only memory entries:
// per-publisher Ed25519 signatures over a canonical entry digest,
// hash-chained per (scope, publisher) for gap/truncation detection.
package provenance

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/x/keyid"
)

// EntryDigest returns the hex SHA-256 of the canonical serialization of
// e: scope, kind, id, createdAt (UTC RFC3339Nano), sorted tags, links,
// content, publisher, keyID, seq, prevHash — everything except the
// signature. It is BOTH the chain link and the signed message.
func EntryDigest(e memory.Entry) string {
	// WIRE FORMAT. encoding/json emits struct fields in DECLARATION order, so
	// the field order and every json tag below are part of the signed message:
	// reorder, rename, or retype one and every signature ever produced stops
	// verifying, with no error anywhere but `oap audit verify`. Append-only
	// additions are equally unsafe — an old entry re-digested under a new field
	// set yields a different digest.
	type canonical struct {
		ScopeKind string          `json:"scopeKind"`
		ScopeID   string          `json:"scopeId"`
		Kind      string          `json:"kind"`
		ID        string          `json:"id"`
		CreatedAt string          `json:"createdAt"`
		Tags      []string        `json:"tags,omitempty"`
		Links     []memory.Link   `json:"links,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
		Publisher string          `json:"publisher"`
		KeyID     string          `json:"keyId"`
		Seq       uint64          `json:"seq"`
		PrevHash  string          `json:"prevHash,omitempty"`
	}
	c := canonical{
		ScopeKind: e.Scope.Kind, ScopeID: e.Scope.ID,
		Kind: e.Kind, ID: e.ID,
		// Truncate to microseconds: postgres TIMESTAMPTZ stores only
		// microsecond precision, so a nanosecond CreatedAt signed by the
		// runner would not re-verify after a postgres round-trip. inmem
		// is unaffected (it stores verbatim and also truncates here).
		CreatedAt: e.CreatedAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		Links:     e.Links, Content: canonicalContent(e.Content),
	}
	if len(e.Tags) > 0 {
		c.Tags = append([]string(nil), e.Tags...)
		sort.Strings(c.Tags)
	}
	if p := e.Provenance; p != nil {
		c.Publisher, c.KeyID, c.Seq, c.PrevHash = p.Publisher, p.KeyID, p.Seq, p.PrevHash
	}
	body, err := json.Marshal(c)
	if err != nil {
		// Unreachable: every field is a string, integer, slice of those, or
		// content canonicalContent has already proven marshalable. Folding the
		// error in rather than hashing a nil body is what keeps a future field
		// from silently collapsing every digest onto sha256("") — one signature
		// covering arbitrarily many entries, with PrevHash unable to tell them
		// apart.
		sum := sha256.Sum256([]byte("provenance: undigestable entry: " + err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// canonicalContent normalizes JSON content so the digest is invariant under
// semantically-equivalent encodings — critically, the byte reshaping a postgres
// JSONB round-trip applies (key reordering, whitespace, number normalization).
// Without it, an entry signed by the runner and read back from postgres would
// fail verification. Applied at BOTH sign and verify time, so the only
// requirement is semantic JSON equality, which JSONB preserves. Numbers
// collapse to their float64 canonical form (audit content is strings and small
// integers; both sides round identically, so the round-trip is stable even past
// float64 integer precision).
//
// The result is ALWAYS valid JSON, including for content that is not: invalid
// bytes are carried as a base64 JSON string. Returning them raw instead makes
// json.Marshal of the enclosing canonical struct fail, and a failed marshal
// digests to sha256("") — identical for every such entry no matter its scope,
// kind, id, seq or prevHash. Base64 keeps the fallback injective, so two
// different malformed entries still get different digests.
func canonicalContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return rawFallback(raw)
	}
	out, err := json.Marshal(v) // json.Marshal sorts map keys
	if err != nil {
		return rawFallback(raw)
	}
	return out
}

// rawFallback encodes bytes that are not valid JSON as a JSON string, so the
// canonical form stays marshalable. json.Marshal of a []byte is base64 and
// cannot fail, so the returned value is always valid JSON.
func rawFallback(raw json.RawMessage) json.RawMessage {
	b, err := json.Marshal([]byte(raw))
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(b)
}

// KeyID returns the key identifier for a public key: hex of the first
// 16 bytes of its SHA-256.
//
// The derivation lives in pkg/x/keyid so a package can name a key without
// importing this one — importing this one confers the ability to SIGN, which
// pkg/web/webui/chat is guarded against acquiring at any depth.
func KeyID(pub ed25519.PublicKey) string { return keyid.For(pub) }

// DecodePubKey decodes a standard-base64 Ed25519 public key, validating it is
// exactly ed25519.PublicKeySize bytes. Delegates to pkg/x/keyid; see KeyID.
func DecodePubKey(b64 string) (ed25519.PublicKey, error) { return keyid.DecodePubKey(b64) }

// SessionPublisher returns the provenance publisher string for a
// session's signed audit entries: "session:<ns>/<name>". Controller
// (key registration) and runner (signing) MUST derive it identically.
func SessionPublisher(namespace, name string) string {
	return "session:" + namespace + "/" + name
}
