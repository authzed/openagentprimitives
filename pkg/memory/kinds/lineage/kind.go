// Package lineage implements the "lineage" memory kind: bidirectional
// fork edges that record parent ↔ child relationships between
// AgentSessions when a user triggers Restart-from-here.
//
// Per fork, two entries are written:
//   - lineage-fork-out-<peer> at the parent's scope
//   - lineage-fork-in-<peer>  at the child's scope
//
// A fork-of-fork chain adds one out-edge to the parent and one in-edge
// to each child. Walking the tree requires only this kind — no K8s join.
package lineage

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "lineage"
	IDPrefix = "lineage-"

	// IDPrefixForkOut and IDPrefixForkIn are the ID sub-prefixes the accessor
	// composes edge IDs from: IDPrefixForkOut+peerName at the parent's scope,
	// IDPrefixForkIn+peerName at the child's. edgesWithPrefix filters on them.
	IDPrefixForkOut = "lineage-fork-out-"
	IDPrefixForkIn  = "lineage-fork-in-"

	// ReasonRestart marks a restart-from-here fork: the user cut the
	// transcript at a turn and edited the message there.
	ReasonRestart = "restart"

	// ReasonContinuation marks a continuation-inherit fork: a new inbound
	// arrived for a terminal session, so the whole transcript carries
	// forward into a fresh session that continues it.
	ReasonContinuation = "continuation"

	// ReasonTakeover marks a different-user takeover fork: a new user continued
	// a terminal predecessor in the same thread and became the child's owner.
	// Introspection only. (Whether the transcript carried forward depends on the
	// takeover's InheritHistory — ordinary terminal inherits; a policy/security
	// halt starts fresh.)
	ReasonTakeover = "takeover"
)

// Content is the JSON payload stored in Entry.Content.
type Content struct {
	// Direction is "out" (this scope is the parent) or "in" (this
	// scope is the child). Redundant with the ID prefix but explicit
	// for readability and forward-compat with non-prefix lookups.
	Direction string `json:"direction"`

	// Peer is the other session's AgentSession name (same namespace).
	Peer string `json:"peer"`

	// AtTurn is the cut turn index N. The child has memory turns 0..N
	// copied from the parent; the user's edit became inbox turn N+1.
	AtTurn int `json:"atTurn"`

	// Reason is one of ReasonRestart, ReasonContinuation or ReasonTakeover —
	// what triggered the edge. Introspection only; nothing branches on it.
	Reason string `json:"reason"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the operator records fork/restart lineage in-process
// (pkg/controllers/agentsession/restart.go), into BOTH parent and child
// scopes.
func (Kind) WriteAuthority() memory.WriteAuthority          { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention                    { return memory.Retention{EssentialWhileLive: true} }
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
