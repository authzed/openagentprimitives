// Package authz_session_config is the memory Kind for the per-session authz
// config snapshot: the entity-extraction config plus the two fields authzd
// derives this session's cold-start authorization policy from.
//
// The AgentSession reconciler writes it, before the runner pod is created.
// There are two independent readers: authzd's cold-start policy resolves
// ScopeEnabled + ColdStart at session start, before turn 0 exists and while the
// runner is blocked on the resulting cold_start_task; authzd's extraction
// worker re-reads BoundEntities + PerToolPrompts + Subject on each wake-up.
//
// Importing pkg/apis/v1alpha1 is fine here: pkg/memory/kinds is not on
// the v1alpha1 → pkg/authz cycle.
package authz_session_config

import (
	"context"
	"reflect"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

type Content struct {
	// BoundEntities are the AgentClass's declared entity types the extractor
	// may bind; empty disables entity extraction for the session.
	BoundEntities []spiceboxv1alpha1.BoundEntityType `json:"boundEntities"`

	// PerToolPrompts is resourceType → extraction-prompt hints, read only by
	// the advisory extraction path (internal/cmd/authzd/extraction_pipeline.go) and
	// overridden there by the class's own BoundEntityType.ExtractionPrompt
	// whenever that is set.
	//
	// NOT POPULATED: the operator, the sole writer, cannot reproduce the
	// runner's runtime-merged tool set (which includes tools discovered live
	// from MCP servers) without re-walking the MCPServer / SpiceboxToolkit /
	// SidecarToolbox / toolspec declarations capability.Assemble merges.
	// Deriving it from those declarations — where every extractionPrompt is
	// declared, so it IS derivable, and more stably than from live discovery —
	// is separable work. Nothing that makes an authorization decision reads it.
	PerToolPrompts map[string][]string `json:"perToolPrompts,omitempty"`

	// Subject is the canonical identity the session's checks run as.
	Subject string `json:"subject"`
	// ScopeEnabled mirrors AgentClass.spec.authz.scope.enabled.
	ScopeEnabled bool `json:"scopeEnabled,omitempty"`

	// ColdStart mirrors AgentClass.spec.authz.scope.coldStart.
	ColdStart string `json:"coldStart,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "authz_session_config"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "asc-" }

// WriteAuthority is ComponentWritten: only the OPERATOR authors this snapshot.
//
// authzd derives the session's cold-start authorization policy from this record
// (internal/cmd/authzd/cold_start_policy.go reads ScopeEnabled + ColdStart, and fails the
// session closed when either is absent or unreadable). Whoever writes it
// therefore chooses which gates run — so the session must not be able to write
// its own, or a compromised runner could hand authzd the policy it wanted
// applied to itself.
//
// The writer is pkg/controllers/agentsession's reconcileAuthzSessionConfig,
// deriving every field from the AgentSession and AgentClass the operator itself
// Got from the API server. The record is K8s-witnessed and written before the
// runner pod is created, so this door cannot starve its readers (both in authzd).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
