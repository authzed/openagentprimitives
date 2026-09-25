// Package metaagentaudit is the memory Kind for the per-invocation
// audit log of metaagent decisions. One entry per @metaagent mention
// (plus cold-start ScopeProposal triggers).
package metaagentaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Classification mirrors the deterministic output of ClassifySkipped +
// DetectCaveats, so audit consumers can replay it without re-running it.
//
// Deliberately NOT scope.MetaagentApprovalPayload or scope.MetaagentOutput:
// they carry the same three values under a different grouping, and the
// persisted grouping stands on its own (see Content). Only the grouping
// differs — the element types (scope.ScopeDelta, scope.SkippedItem,
// scope.CaveatItem) are declared once in scope and shared by all three.
type Classification struct {
	// Applied is the delta that survived classification.
	Applied scope.ScopeDelta `json:"applied"`
	// Skipped is each requested item that was dropped, with its reason.
	Skipped []scope.SkippedItem `json:"skipped,omitempty"`
	// Caveats is each applied item that carries a condition on its use.
	Caveats []scope.CaveatItem `json:"caveats,omitempty"`
}

// ComposerOutput is the LLM-composed prose the approval prompt renders
// (scope.MetaagentApprovalPayload's ApproverSummary / SkippedExplain /
// CaveatExplain). Distinct from authzd's same-named ComposerOutput, which
// carries the raw per-item explanation SLICES before they are joined into
// these three lines.
type ComposerOutput struct {
	// ApproverSummary is the one-paragraph "what you are approving" prose.
	ApproverSummary string `json:"approverSummary,omitempty"`
	// SkippedExplain is the joined prose for Classification.Skipped.
	SkippedExplain string `json:"skippedExplain,omitempty"`
	// CaveatExplain is the joined prose for Classification.Caveats.
	CaveatExplain string `json:"caveatExplain,omitempty"`
}

// Content is the persisted record. Its json tags are a SEPARATE contract from
// the approval prompt's: the two overlap in value and must not be collapsed.
// Two writers project onto it — the cold-start outcome path from a
// scope.MetaagentOutput (Delta → Classification.Applied) and the
// request-publish path from a scope.MetaagentApprovalPayload — and it carries
// facts neither has (model ids, per-stage latencies, ProposedDelta before
// classification, the final ApproverDecision).
//
// The keys are frozen harder than a wire key is: this Kind is AppendOnly and
// Ed25519-signed into a per-(scope, publisher) hash chain, and records are
// re-read and re-verified long after the prompt is gone (channelsd's Show
// Details once its cache is lost; authzd re-driving an approval from
// Classification.Applied). Renaming a key to track a wire rename migrates
// nothing already stored — it only stops old records decoding.
//
// What DOES bind them: a fact that becomes load-bearing for the approval
// decision belongs in both — in the payload so the approver sees it, and here
// so the record still explains what approving meant.
type Content struct {
	// Ts is when the metaagent invocation this record describes ran.
	Ts time.Time `json:"ts"`
	// Requester is the canonical id of the user whose mention triggered it.
	Requester string `json:"requester"`
	// Approver is the canonical id of whoever decided; empty until decided.
	Approver string `json:"approver,omitempty"`
	// RequestText is the requester's raw @metaagent text, unmodified.
	RequestText string `json:"requestText"`
	// RequestID is the approval request id minted by authzd's pipeline
	// host and embedded in the channel approval buttons. Set on entries
	// written at request-publish time (RecordRequested) so channel Show
	// Details can look the record up after channelsd's cache is gone.
	RequestID string `json:"requestId,omitempty"`
	// ColdStart + CleanedTask mirror the scope-approval payload for
	// new-session first-turn approvals (Show Details echoes the task).
	ColdStart   bool   `json:"coldStart,omitempty"`
	CleanedTask string `json:"cleanedTask,omitempty"`
	// ExtractorLLMModel is the model id that produced ProposedDelta.
	ExtractorLLMModel string `json:"extractorLlmModel,omitempty"`
	// ExtractorLatencyMs is extraction wall time in ms; 0 when it did not run.
	ExtractorLatencyMs int64 `json:"extractorLatencyMs,omitempty"`
	// ProposedDelta is the extractor's ask, before classification narrowed it.
	ProposedDelta scope.ScopeDelta `json:"proposedDelta,omitempty"`
	// Classification is the narrowed delta plus what was skipped and caveated.
	Classification Classification `json:"classification,omitempty"`
	// ComposerLLMModel is the model id that produced ComposerOutput.
	ComposerLLMModel string `json:"composerLlmModel,omitempty"`
	// ComposerLatencyMs is composition wall time in ms; 0 when it did not run.
	ComposerLatencyMs int64          `json:"composerLatencyMs,omitempty"`
	ComposerOutput    ComposerOutput `json:"composerOutput,omitempty"`
	// ApproverDecision is the action taken; empty on a request-time record.
	ApproverDecision string `json:"approverDecision,omitempty"`
	// AppliedDelta is what was actually applied; nil when nothing was.
	AppliedDelta *scope.ScopeDelta `json:"appliedDelta,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "metaagent_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "maud-" }

// WriteAuthority: authzd signs these as system:authzd (internal/cmd/authzd). The record
// attests that the platform approved a scope change; a session authoring one
// forges that attestation.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // meta-agent audit records cross-agent delegation evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"ts", "requester", "approverDecision"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
