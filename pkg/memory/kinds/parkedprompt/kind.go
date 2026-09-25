// Package parkedprompt implements the "parked_prompt" memory kind: the durable
// record of a render-ready prompt a session is parked on, so it can be
// re-surfaced to a surface that attaches later, or after a restart.
//
// Delivery is a ONE-SHOT live publish and its publisher dedups, so a prompt is
// sent exactly once, ever. Held only in a process-local cache, a channelsd
// restart lost it permanently and wedged the session with no card and no
// recovery.
//
// ONLY categories declaring Resurface == ResurfaceCached are stored here. A
// ResurfaceRegenerate category (credential_link) stores nothing on purpose:
// its actionable part is a freshly-minted signed link that must never be
// persisted in replayable form, and its regenerator rebuilds the prompt from
// live durable state given only the session's phase.
package parkedprompt

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "parked_prompt"
	IDPrefix = "parked-"
)

// Content is the JSON payload: the cached OUT envelope plus the fields the
// resurface path reads without decoding it.
type Content struct {
	// RequestRef correlates the prompt with its decision, and is the key a
	// resolve clears by.
	RequestRef string `json:"requestRef"`

	// Category is the interaction category. The Park phase and Resurface policy
	// are looked up from the registry at read time rather than frozen here, so
	// a category's policy may legitimately change across a deploy.
	Category string `json:"category"`

	// Interruptible mirrors the payload flag the republish path reads to decide
	// whether to stamp a fresh interrupt id.
	Interruptible bool `json:"interruptible,omitempty"`

	// Envelope is the render-ready OUT envelope, stored verbatim so a replay is
	// byte-identical to the original delivery.
	Envelope []byte `json:"envelope"`

	// Resolved marks a prompt whose interaction has been decided or timed out;
	// without it a resolved prompt would be re-surfaced forever. A tombstone
	// rather than a deletion because memory.Memory exposes Put/Query but no
	// per-entry Delete: the read path filters these out, and scope-level GC
	// reclaims them with the rest of the session.
	Resolved bool `json:"resolved,omitempty"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: channelsd owns the parked-prompt record and its tombstone
// (pkg/channels/channelsd); a session must not be able to resolve or replace a prompt
// it is being asked to answer.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

// Retention is deliberately NOT AppendOnly. Unlike the audit kinds (transcript,
// approval, authz-decision) this is working state, not evidence: a resolved
// prompt must be overwritable with its tombstone, and an append-only kind
// refuses exactly that. EssentialWhileLive keeps it for the session's lifetime,
// which is precisely as long as a prompt can still be outstanding.
func (Kind) Retention() memory.Retention { return memory.Retention{EssentialWhileLive: true} }

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
