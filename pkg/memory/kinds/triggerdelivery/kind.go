// Package triggerdelivery is the memory Kind for the append-only record of the
// signed webhook delivery that STARTED a session.
//
// channel_msg_ref indexes an inbound by an opaque, kind-scoped reference — it
// answers "which message was this turn" and cannot answer "what did the
// provider actually send". For a session nobody typed into, that body is the
// entire input: the payload, the event type, and the channel key derived from
// them are what a steelthread capture replays through the production webhook
// route, HMAC and event filtering included.
//
// One record per session, written by channelsd at the delivery that opened it.
// A session opened by a person typing has none, and that absence is meaningful
// rather than missing data.
package triggerdelivery

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// MaxBodyBytes caps what is stored. Provider pull-request payloads run tens of
// kilobytes and are carried over NATS before they land here, so the cap bounds
// both. A body over the cap is stored truncated and MARKED — see Content.
const MaxBodyBytes = 256 * 1024

// Content is one recorded delivery.
type Content struct {
	// Kind is the channel kind that received it, which selects the signing and
	// event-dispatch mechanics a replay must reproduce.
	Kind string `json:"kind"`
	// Event is the provider's event-type header value. The receiver dispatches
	// on it and the body alone does not carry it.
	Event string `json:"event"`
	// ChannelKey is the binding key this delivery produced.
	ChannelKey string `json:"channelKey"`
	// Body is the raw bytes the provider posted, verbatim — including any
	// insignificant whitespace, and even a body cut mid-token by truncation
	// (see Truncated).
	//
	// Deliberately []byte, not json.RawMessage: a RawMessage embedded in a
	// marshaled struct is whitespace-COMPACTED on the way through and must be
	// syntactically valid JSON, and either one would falsify the one property
	// this field exists for — a replay re-signs these exact bytes, so a
	// reformatted or invalid-truncated body fails HMAC verification. []byte
	// round-trips through JSON as base64, which is immune to both.
	Body []byte `json:"body"`
	// Truncated marks a body cut at MaxBodyBytes.
	//
	// A truncated body cannot be re-signed into a valid delivery, so a capture
	// must REFUSE it. That refusal requires knowing, which is why this is a
	// stored fact rather than something a reader infers from a length.
	Truncated bool `json:"truncated,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "trigger_delivery"

// IDPrefix is the entry-id prefix.
const IDPrefix = "trigdel-"

// entryID is fixed: one delivery opens one session, so a redelivery of the same
// webhook lands on the same id and is an idempotent re-put rather than a second
// row claiming the session had two openings.
//
// Unlike systemprompt's digest-derived id, a conflict on this FIXED id carries
// no content guarantee by itself — Record proves idempotency by comparing the
// stored body against the incoming one before treating a conflict as a no-op.
const entryID = IDPrefix + "opening"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: channelsd owns the inbound mapping, as it owns
// channel_msg_ref. A session must not be able to rewrite the delivery that
// started it.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
		// The input that caused an autonomous session to act is evidence, and
		// the one input no human can attest to.
		AppendOnly: true,
	}
}

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"channelKey"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
