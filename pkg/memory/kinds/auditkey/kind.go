// Package auditkey implements the "audit_key" memory Kind: the operator's
// durable witness of which Ed25519 key an AgentSession instance signs its
// append-only records with.
//
// Why the witness exists. A session's signing seed lives in a Secret owned by
// its AgentSession and the public half is anchored on that CR's status, so both
// die with the CR. The records the key signed do not: append-only entries are
// permanent (memory.Local.DeleteScope structurally cannot remove them) and they
// sit in a scope keyed by namespace/name, which the NEXT AgentSession of that
// name inherits — a redelivered webhook, a re-fired trigger. Verifying that
// survivor would then find a chain whose earlier entries are signed by a key
// nothing can produce any more, and report every one of them as unknown-key.
//
// Recording the binding here puts it where the records are, keeps it for exactly
// as long as they last, and roots its trust in the operator's own publisher key,
// which lives in the publisher-keys ConfigMap and outlives every session. It is
// deliberately NOT the ConfigMap itself: that would grow by one entry per
// session, forever, against a 1MB object.
package auditkey

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	// KindName is the registered name of this memory Kind.
	KindName = "audit_key"
	// IDPrefix namespaces every entry ID this Kind writes. An entry is named
	// IDPrefix+keyID, so the record is content-addressed by the key it attests
	// and one key can be witnessed exactly once.
	IDPrefix = "audit-key-"
)

// Content is one key binding: the AgentSession instance, and the key it signs
// its append-only records with.
type Content struct {
	// SessionUID is the UID of the AgentSession instance that holds the private
	// half. It is what distinguishes the attempt that failed from the one that
	// replaced it under the same name.
	SessionUID string `json:"sessionUID"`
	// KeyID is the key's content address (provenance.KeyID of PubKey), matching
	// the keyID every entry that key signed carries.
	KeyID string `json:"keyId"`
	// PubKey is the 32-byte Ed25519 public key in STANDARD base64 — the same
	// encoding AgentSession.status.auditPublicKey uses.
	PubKey string `json:"pubKey"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the operator records the binding when it mints or re-derives a
// session's audit key (pkg/controllers/agentsession). A session may never author
// its own key binding — a self-attested key proves nothing.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

// Retention: append-only, and with no ArchiveOn signal. The binding must outlive
// the records it authenticates, and those are kept indefinitely.
func (Kind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true} // a key binding is the trust root of a chain — write-once, kept.
}

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
