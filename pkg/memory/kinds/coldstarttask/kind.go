// Package coldstarttask is the memory Kind recording the approver's
// decision for a new session's cold-start scope proposal: which task
// (cleaned / original / none) the runner should run as the first turn,
// and whether scope was applied. One entry per session, stable ID
// "cst-config". The runner reads it after WaitForColdStartTask resolves.
package coldstarttask

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Status values — what the runner should do with the first turn.
const (
	StatusApprovedCleaned  = "approved_cleaned"  // run CleanedText; scope applied
	StatusApprovedOriginal = "approved_original" // run raw inbox turn; scope applied
	StatusRanWithoutScope  = "ran_without_scope" // run raw inbox turn; no scope (approver explicitly chose this)
	StatusDenied           = "denied"            // run nothing; abort + notify
	// StatusScopeReviewFailed signals that scope review could NOT complete
	// (the extractor LLM errored, or no decider was available). The runner
	// treats it as fail-closed: HALT the session with a surfaced error rather
	// than running the raw prompt unscoped. Distinct from StatusRanWithoutScope,
	// which is a deliberate approver choice to proceed without scope.
	StatusScopeReviewFailed = "scope_review_failed" // halt the session (fail closed)
)

// Content is the payload stored in a cold_start_task entry.
type Content struct {
	// Status is one of the Status* constants above; it alone tells the runner
	// whether to run, and which text to run.
	Status string `json:"status"`
	// CleanedText is the scope-reviewed rewrite of the first turn, run only
	// under StatusApprovedCleaned. Empty for every other status.
	CleanedText string `json:"cleanedText,omitempty"`
	// InboxIdx is the inbox turn this decision applies to — 0 for a genuine
	// cold start.
	InboxIdx int `json:"inboxIdx"`
	// DecidedAt is when the decision was reached; defaulted to now by Put.
	DecidedAt time.Time `json:"decidedAt"`
	// Approver is the canonical subject who decided; empty when no human did
	// (an auto-approval, or a review that failed before reaching one).
	Approver string `json:"approver,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "cold_start_task"

// Kind implements memory.Kind for cold_start_task.
type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "cst-" }

// WriteAuthority: authzd alone decides a cold-start scope review
// (internal/cmd/authzd/cold_start_pipeline.go). The runner BLOCKS on this entry and
// authzd treats its presence as "already decided", so a session that could
// author it would both approve its own scope and replay the review.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"status"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
