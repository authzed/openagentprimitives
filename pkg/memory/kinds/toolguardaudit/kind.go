// Package toolguardaudit is the memory Kind for the audit log of tool-guard
// enforcement events: breaker trips/recoveries, rate-limit hits, denials and
// halts. One entry per event. Queried post-hoc via `oap memory query`.
package toolguardaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one tool-guard enforcement event, mirroring toolguard.Event.
type Content struct {
	// Event is the occurrence: breaker_*, *_limit_hit, or guard_deny|warn|halt.
	Event string `json:"event"`
	// Tool is the agent-facing tool name the guard was evaluating.
	Tool string `json:"tool"`
	// Origin is the tool's backing origin; empty for origin-less tools.
	Origin string `json:"origin,omitempty"`
	// Key is the breaker key: "tool/<name>" or "origin/<ref>".
	Key string `json:"key"`
	// UseID is the tool_use block id of the call that hit the guard.
	UseID string `json:"useID,omitempty"`
	// Trips is how often this breaker has opened; 0 on non-breaker events.
	Trips int32 `json:"trips,omitempty"`
	// CoolOff is how long the breaker stays open, as a duration string ("30s").
	CoolOff string `json:"coolOff,omitempty"`
	// RetryAt is the absolute time at which the call may be retried.
	RetryAt time.Time `json:"retryAt"`
	// Limit is the bound hit: per_turn|window (rate) or egress|ingress (bytes).
	Limit string `json:"limit,omitempty"`
	// ObservedBytes is the measured size on a byte-limit event; 0 otherwise.
	ObservedBytes int64 `json:"observedBytes,omitempty"`
	// Action is the verdict the guard applied for this event.
	Action string `json:"action"`
	// Provenance is the rule's source tier, e.g. "class[0]" or "ceiling".
	Provenance string `json:"provenance"`
	// At is when the event occurred.
	At time.Time `json:"at"`
}

// KindName is the registered name of this memory Kind.
const KindName = "toolguard_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "tgaud-" }

// WriteAuthority: the runner records its own toolguard verdicts
// (pkg/agent/runner/pipeline_wiring.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // toolguard circuit-breaker events are security audit evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"at", "tool", "event"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
