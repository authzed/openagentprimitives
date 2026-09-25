// Package infoleakageaudit is the memory Kind for information-leakage audit events.
// Each entry records a single gate event (read denied, leakage detected, approval
// granted/denied, etc.) with 90-day retention after archive.
package infoleakageaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const KindName = "infoleakage_audit"

// ResourceRef identifies a SpiceDB resource by type + id.
type ResourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AuditRecord is a structured record of a single information-leakage
// gate event (read denied, leakage detected, approval granted/denied, etc.).
type AuditRecord struct {
	// At is when the gate event occurred; defaulted to now by Append and used
	// as the entry's CreatedAt.
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // event subtype: "read_denied" | "leakage_detected" | ...
	// Session is the AgentSession as "namespace/name". Redundant with the
	// entry's scope, and recorded so an exported record stands alone.
	Session string `json:"session,omitempty"`
	// Tool is the tool whose call produced the event; empty for events that
	// are not tied to one (session_end, audience resolution).
	Tool string `json:"tool,omitempty"`
	// Resources are the tainted resources implicated in this event — for a
	// leakage decision, the ones that would have been disclosed.
	Resources []ResourceRef `json:"resources,omitempty"`
	// Audience is every canonical identity the proposed reply would reach.
	Audience []string `json:"audience,omitempty"`
	// LeakedTo is the subset of Audience NOT permitted on Resources: the
	// members whose presence makes the reply a leak. Empty means no leak.
	LeakedTo []string `json:"leakedTo,omitempty"`
	// Requester is the canonical identity whose turn produced the event.
	Requester string `json:"requester,omitempty"`
	// Details is free-form key/value context whose keys depend on Kind (e.g.
	// approver, approvalKind, approved).
	Details map[string]string `json:"details,omitempty"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "ila-" }

// WriteAuthority: the runner records its own info-leakage events
// (internal/cmd/runner/main.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // info-leakage audit records are security evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(AuditRecord{}) }
func (Kind) IndexedFields() []string                        { return []string{"kind"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
