// Package scopeaudit is the memory Kind for the audit log of applied
// ScopeDeltas — one entry per applied delta. Read by the admin console's
// audit timeline (pkg/web/admind/audit) and covered by `oap audit verify`.
package scopeaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one applied-delta record.
type Content struct {
	// Delta is the change as applied — after classification narrowed it.
	Delta scope.ScopeDelta `json:"delta"`
	// AppliedAt is when the delta was folded into the session scope.
	AppliedAt time.Time `json:"appliedAt"`
	// Source is a scope.Source value naming what authorized the change.
	Source string `json:"source"`
	// Approver is the canonical id of who approved; empty when nobody did.
	Approver string `json:"approver,omitempty"`
	// Requester is the canonical id of who asked; empty on non-user sources.
	Requester string `json:"requester,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "scope_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "saud-" }

// WriteAuthority: authzd records scope mutations (internal/cmd/authzd/metaagent.go) as
// the audit of what it applied.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // scope audit events record scope state transitions — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"appliedAt"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
