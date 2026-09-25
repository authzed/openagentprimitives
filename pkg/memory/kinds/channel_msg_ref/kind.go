// Package channel_msg_ref implements the "channel_msg_ref" memory
// kind: a sparse index that maps a channel-kind-scoped message
// reference (Slack: channel_id:thread_ts:message_ts; future kinds:
// their own) to the turn index where the inbound message was
// recorded.
//
// Used by channel-kind UIs (the Slack "Restart from here" modal,
// future Discord/Teams equivalents) to resolve "the message the user
// clicked" to "the turn to cut at" — without text-matching or
// per-kind side indexes.
package channel_msg_ref

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "channel_msg_ref"
	IDPrefix = "cmref-"
)

// Content is the JSON payload. (Kind, Ref) is the channel-scoped
// identity of the message; TurnIndex is the turn it became.
type Content struct {
	// Kind is the channel kind ("slack", "fake", future "discord", ...).
	Kind string `json:"kind"`

	// Ref is the channel-kind-opaque message reference. For Slack:
	// a `channel_id:thread_ts:message_ts` triple. Treated as opaque
	// by this kind.
	Ref string `json:"ref"`

	// TurnIndex is the inbox turn the message was recorded at.
	TurnIndex int `json:"turnIndex"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: channelsd owns the channel-message mapping
// (internal/cmd/channelsd/memory.go); a session must not be able to re-point a turn at
// another message.
func (Kind) WriteAuthority() memory.WriteAuthority          { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention                    { return memory.Retention{EssentialWhileLive: true} }
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
