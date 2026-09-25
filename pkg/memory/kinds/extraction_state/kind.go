// Package extraction_state is the memory Kind tracking authzd's
// per-turn extraction lifecycle: pending → complete | failed. The
// runner's WaitForExtraction polls this Kind to gate autofill.
package extraction_state

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const (
	StatusPending  = "pending"
	StatusComplete = "complete"
	StatusFailed   = "failed"
)

type Content struct {
	// TurnIndex is the inbox turn this extraction covers. One entry per turn:
	// it is the whole of the entry's deterministic ID, so a re-Record for the
	// same turn overwrites, which is how pending advances to complete/failed.
	TurnIndex int `json:"turnIndex"`
	// Status is StatusPending, StatusComplete or StatusFailed. The runner's
	// WaitForExtraction unblocks on either terminal value, so a status that is
	// never advanced leaves it polling to its deadline.
	Status string `json:"status"`
	// StartedAt is when extraction began; defaulted to now by Record and used
	// as the entry's CreatedAt.
	StartedAt time.Time `json:"startedAt"`
	// CompletedAt is when extraction reached a terminal Status; zero while
	// still pending.
	CompletedAt time.Time `json:"completedAt,omitempty"`
	// Error is the failure message, set only with StatusFailed.
	Error string `json:"error,omitempty"`
	// CandidateCount is how many extracted_entity candidates this turn yielded.
	// Zero is a real answer (nothing found), not "not measured".
	CandidateCount int `json:"candidateCount,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "extraction_state"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "exs-" }

// WriteAuthority: authzd owns extraction progress
// (internal/cmd/authzd/extraction_pipeline.go); the runner polls it and must not be
// able to declare it complete.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"turnIndex", "status"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
