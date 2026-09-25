// Package extracted_entity is the memory Kind for raw LLM-extracted
// entity candidates produced by internal/cmd/authzd. Unchecked: a future
// CheckEntityCanBind path reads these, runs the Check, and promotes
// allowed candidates to binding entries.
package extracted_entity

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

type Content struct {
	// ResourceType is the SpiceDB object type the extractor believes it found.
	// UNCHECKED — the candidate is not yet authorized against anything.
	ResourceType string `json:"resourceType"`
	// ResourceID is the SpiceDB object id, as the extractor read it out of the
	// turn. Attacker-influenced: it comes from LLM output over turn text.
	ResourceID string `json:"resourceID"`
	// SourceText is the span the candidate was read from, kept so a reviewer
	// can judge the extraction; empty when the extractor reported no span.
	SourceText string `json:"sourceText,omitempty"`
	// TurnIndex is the inbox turn the extraction ran over. It is the query key
	// ForTurn filters on, and part of the entry's deterministic ID.
	TurnIndex int `json:"turnIndex"`
	// ExtractedAt is when extraction produced this candidate; defaulted to now
	// by Record and used as the entry's CreatedAt.
	ExtractedAt time.Time `json:"extractedAt"`
}

// KindName is the registered name of this memory Kind.
const KindName = "extracted_entity"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "eex-" }

// WriteAuthority: authzd writes what its extractor found
// (internal/cmd/authzd/extraction_pipeline.go); the entities it yields drive entity
// binding.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"turnIndex", "resourceType"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
