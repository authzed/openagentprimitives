// Package infoleakagedecision is the memory Kind for per-session information-
// leakage approval decisions. Each entry records that a (resourceType,
// resourceID) was approved or denied for sharing to the current session's
// audience, so a later gate can answer "did the approver already decide on this
// resource?" durably — surviving a runner restart (unlike an in-process map).
package infoleakagedecision

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const KindName = "infoleakage_decision"

// Decision values for DecisionRecord.Decision.
const (
	DecisionApproved = "approved"
	DecisionDenied   = "denied"
)

// DecisionRecord is one approve/deny decision for a (resourceType, resourceID)
// leakage event. Mirrors infoleakagetaint.TaintRecord's resource-identity shape.
type DecisionRecord struct {
	// ResourceType is the SpiceDB object type the decision covers.
	ResourceType string `json:"resourceType"`
	// ResourceID is the SpiceDB object id the decision covers. Together with
	// ResourceType it is the whole key IsApproved/IsDenied match on — the
	// decision binds to a resource, not to the reply that provoked it.
	ResourceID string    `json:"resourceID"`
	Decision   string    `json:"decision"` // "approved" | "denied"
	At         time.Time `json:"at"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "ild-" }

// WriteAuthority: the runner records the audience decisions its own hooks
// produce.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:  []memory.SignalKind{lifecycle.SigSessionCompleted},
		AppendOnly: true, // info-leakage gate decisions are security evidence — write-once.
		// TTLAfterArchive zero: decisions live as long as the archived session.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(DecisionRecord{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
