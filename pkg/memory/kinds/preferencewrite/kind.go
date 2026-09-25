// Package preferencewrite is the memory Kind for the audit log of
// first-party human writes to user preferences: every direct edit via the
// Slack App Home (or other UI surfaces) into the acting user's own
// user_preference scope. It records who edited what, when, and why,
// creating a tamper-evident audit chain that lets a human answer
// "which settings did this user change, and through what path" after
// the fact — the opposite of preferenceaccess (agent reads on the session
// chain). Every user preference edit appends a signed, tamper-evident
// record into the user's own scope, not the session's.
package preferencewrite

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one user preference write — a first-party edit into the
// acting user's own user_preference scope, recorded alongside the
// user_preference Entry it documents.
type Content struct {
	// Subject is the canonical user id performing the write (the acting user,
	// never an agent on behalf of them).
	Subject string `json:"subject"`
	// ClassNamespace is the agent class namespace.
	ClassNamespace string `json:"classNamespace"`
	// ClassName is the agent class name.
	ClassName string `json:"className"`
	// Key is the preference key being written.
	Key string `json:"key"`
	// Value is the raw JSON bytes of the new value (e.g., "false", "true",
	// "\"notifications_enabled\"", or a complex object). Kept as json.RawMessage
	// to preserve the exact serialization, allowing exact replay or comparison.
	Value json.RawMessage `json:"value"`
	// Via is the UI path or surface the write came through (e.g., "app-home",
	// "web-ui", "cli").
	Via string `json:"via"`
}

// KindName is the registered name of this memory Kind.
const KindName = "preference_write"

// IDPrefix is the literal byte prefix every Entry of this Kind's ID starts
// with.
const IDPrefix = "prefwr-"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: ComponentWritten — only platform components (the Slack App
// Home handler, the CLI preference command, etc.) author this Kind, signed as
// the acting user through their own signing facade. A session credential can
// never author a write to a user scope — only the user themselves, through a
// platform surface.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // user preference writes are user audit evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(Content{}) }

// IndexedFields: "key" and "via" — fields a forensic sweep filters on (show me
// every write to "notifications", or every edit via "app-home"). JSON tag, not
// the Go name, per the Kind interface's own contract.
func (Kind) IndexedFields() []string                        { return []string{"key", "via"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
