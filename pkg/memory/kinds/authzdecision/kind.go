// Package authzdecision is the memory Kind for authz.Check outcomes.
// Every Check the runner performs writes a Decision entry here, linked
// to the targeted resource and subject. Forensics & "why was this
// denied?" UX read from this Kind.
package authzdecision

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// The canonical outcome strings. They live here rather than being whatever a
// caller's Stringer happens to emit: the tag written at Record time is
// "outcome:"+Outcome and DeniesForResource queries that tag literally, so a
// caller spelling it "Denied" produces a log whose denials are unfindable —
// and a deny set that comes back empty fails OPEN.
const (
	OutcomeAllowed = "allowed"
	OutcomeDenied  = "denied"
)

// OutcomeOf maps an allow/deny boolean to the canonical string.
func OutcomeOf(allowed bool) string {
	if allowed {
		return OutcomeAllowed
	}
	return OutcomeDenied
}

// Decision is one record. ResourceType / ResourceID are SpiceDB
// object coordinates; ArgsHash is the filtered hash from the
// dispatcher (matches what the slice-2 approval grant binds against).
//
// The Pin* fields carry dependency-pin provenance facts for the tool
// that was called. They are populated by the runner when ToolProvenance
// is wired; all are omitempty so existing records (and tools without pin
// records) serialize cleanly without the pin block.
type Decision struct {
	Outcome string `json:"outcome"` // OutcomeAllowed | OutcomeDenied
	// Subject is the canonical identity the Check evaluated for.
	Subject string `json:"subject"`
	// ResourceType is the SpiceDB object type the Check targeted.
	ResourceType string `json:"resourceType"`
	// ResourceID is the SpiceDB object id the Check targeted.
	ResourceID string `json:"resourceId"`
	// Permission is the SpiceDB permission evaluated on that object.
	Permission string `json:"permission"`
	// ArgsHash is the dispatcher's filtered hash of the call arguments — the
	// same value a per-tool approval grant binds against. Empty when the tool
	// declares no hashed arguments.
	ArgsHash string `json:"argsHash,omitempty"`
	// EnforceMode records whether a SpiceDB deny was final regardless of the
	// class's spec.authz.toolCalls.mode: "inherit" (permissive classes log and proceed) or
	// "always". Empty when the check declared none.
	EnforceMode string `json:"enforceMode,omitempty"`
	// Message is the human-readable explanation, primarily the denial reason;
	// usually empty on an allow.
	Message string `json:"message,omitempty"`

	// Pin provenance, filled by the runner from the tool's ProvenanceRecord.

	// PinKind is the pinning registry kind ("mcp", "image", "cli", "skill", …).
	PinKind string `json:"pinKind,omitempty"`
	// PinName is the dependency's declared name (MCPServer ref, SidecarToolbox ref, …).
	PinName string `json:"pinName,omitempty"`
	// PinStrength is the syntactic pin strength ("frozen", "named", "unpinned").
	PinStrength string `json:"pinStrength,omitempty"`
	// PinDigest is the immutable identity recorded at session start (sha256:… hash).
	PinDigest string `json:"pinDigest,omitempty"`
	// PinVersion is the human-readable identity (tag, serverInfo.version, …).
	PinVersion string `json:"pinVersion,omitempty"`
	// PinDrifted is true when the tool's backing dependency drifted from its
	// recorded pin baseline at session start.
	PinDrifted bool `json:"pinDrifted,omitempty"`
	// PinBypassReason is non-empty when a pinning-policy bypass suppressed
	// an otherwise-applicable rule for this dependency.
	PinBypassReason string `json:"pinBypassReason,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "authz_decision"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "authzd-" }

// WriteAuthority: the runner records the authorization outcome of its own tool
// calls (pipeline_wiring.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
		AppendOnly:      true, // authz decisions are audit evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Decision{}) }
func (Kind) IndexedFields() []string                        { return []string{"outcome", "resourceType"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
