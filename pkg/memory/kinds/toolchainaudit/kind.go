// Package toolchainaudit is the memory Kind for the append-only record of
// toolchain selection. Status is mutable and dies with the AgentSession; this
// is the durable, tamper-evident record.
//
// Only the `resolved` phase is written today — by the SpiceboxSession
// controller, carrying the frozen image digests of what actually ran. The
// `requested`/`rejected` phases are declared but unwritten: a session-side
// publisher for them would need WriteAuthority widened past ComponentWritten.
package toolchainaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// ResolvedRef is one toolchain as it was actually materialized.
type ResolvedRef struct {
	Name string `json:"name"`
	// Image is the container image it ran from, digest-pinned wherever the
	// catalog pins it.
	Image string `json:"image"`
}

// Content is one toolchain-audit record.
type Content struct {
	// Phase is which half of the record this is: requested|resolved|rejected.
	Phase string `json:"phase"`
	// Requested is the toolchain names the agent asked for, unresolved.
	Requested []string `json:"requested,omitempty"`
	// Resolved is what was materialized, with the image each one ran from.
	Resolved []ResolvedRef `json:"resolved,omitempty"`
	// SetDigest identifies the resolved toolchain set as a whole.
	SetDigest string `json:"setDigest,omitempty"`
	// Bundle is the AgentBundle the toolchains were resolved for, if any.
	Bundle string `json:"bundle,omitempty"`
	// Session is the SpiceboxSession the toolchains were materialized into.
	Session string `json:"session,omitempty"`
	// Subject is the canonical identity the selection was made on behalf of.
	Subject string `json:"subject,omitempty"`
	// CacheKey / CacheReused are declared for the warm-cache phase and are not
	// written yet; they exist so a cache-poisoning investigation can answer
	// "which sessions mounted this cache" from the log alone.
	CacheKey    string `json:"cacheKey,omitempty"`
	CacheReused bool   `json:"cacheReused,omitempty"`
	// Reason explains a `rejected` phase; empty on the others.
	Reason string `json:"reason,omitempty"`
	// At is when the phase this record captures happened.
	At time.Time `json:"at"`
}

// KindName is the registered name of this memory Kind.
const KindName = "toolchain_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "tcaud-" }

// WriteAuthority: the SpiceboxSession controller records toolchain overlays
// in-process (pkg/controllers/spiceboxsession).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // toolchain selection is security evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"at", "phase", "setDigest"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
