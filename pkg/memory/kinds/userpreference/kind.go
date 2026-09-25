// Package userpreference is the memory Kind for one user's saved preference
// values, keyed per (classNamespace, className, key) in the user's own scope
// (memory.UserScope). Mutable — last write wins on the deterministic entry
// ID. ComponentWritten: only the operator's preferences commit path may
// author it; a session bearer is refused at the facade door, which is the
// structural guard that keeps a prompt-injected agent from bypassing the
// human-confirm flow.
package userpreference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// KindName is the registered name of this memory Kind.
const KindName = "user_preference"

// IDPrefix is the literal byte prefix every Entry of this Kind's ID starts
// with. EntryID always includes it — deliberately unlike factcontent.EntryID,
// whose callers apply the prefix themselves — because this Kind has exactly
// one deterministic-ID producer and no reason to make a caller repeat it.
const IDPrefix = "upref-"

// SoftCap bounds pathology across classes; the schema already bounds keys
// per class.
const SoftCap = 1024

// Preference is the Kind's Content payload: one saved value for one class's
// declared preference key, in the user's own scope.
type Preference struct {
	// ClassNamespace is the AgentClass namespace the preference was set for.
	ClassNamespace string `json:"classNamespace"`
	// ClassName is the AgentClass name the preference was set for.
	ClassName string `json:"className"`
	// Key is the preference's declared key within the class's schema.
	Key string `json:"key"`
	// Value is the saved value, or JSON null for a cleared tombstone: an
	// explicit "the user unset this" distinct from "never set", so a later
	// read does not fall back to the class default it was deliberately
	// cleared away from.
	Value json.RawMessage `json:"value"`
	// SetViaSession names the session the value was set through, when set
	// through one — empty for values set by another route (e.g. a CLI/admin
	// write).
	SetViaSession string `json:"setViaSession,omitempty"`
}

// EntryID is deterministic per (classNS, className, key) so a re-save
// overwrites; the 0x1f separator prevents field-boundary collisions
// (("ns","a","bc") must not hash the same as ("ns","ab","c")).
func EntryID(classNS, className, key string) string {
	const sep = "\x1f"
	sum := sha256.Sum256([]byte(classNS + sep + className + sep + key))
	return IDPrefix + hex.EncodeToString(sum[:])[:32]
}

// Kind is the memory.Kind implementation for user_preference.
type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: ComponentWritten — the operator's preferences commit path
// is the only author; see the package doc for the threat this closes.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention { return memory.Retention{SoftCapPerScope: SoftCap} }

func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(Preference{}) }

func (Kind) IndexedFields() []string { return []string{"classNamespace", "className"} }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
