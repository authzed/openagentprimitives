// Package webactionapproval is the append-only memory Kind recording a human's
// approval of a browser MUTATE against the exact page they were shown (Phase E,
// E9). It is the evidence half of the per-action web_action gate: the dispatch
// check (E8) proves a mutate held a fingerprint-caveated grant, and this record
// proves WHICH page fingerprint a human approved that grant against, who
// approved it, and when — so `oap audit` can answer "what irreversible web
// action was taken, and against what page state did a person consent to it".
//
// Mirrors kinds/approval: SessionWritten (the runner records its own acting
// approvals, pkg/agent/runner/host_approval.go) and AppendOnly (write-once audit
// evidence — a changing re-put is refused at the memory.Local facade).
package webactionapproval

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Record is one web_action approval event: a human cleared a browser mutate,
// and this is what they cleared it against.
//
// WebActionKey + ApprovedFingerprint are the two load-bearing fields: together
// they say "action X was approved while the page looked like fingerprint F",
// which is exactly what the pagestate_matches-caveated grant encodes — the
// record is the human-readable, tamper-evident twin of that grant. The rest are
// context an auditor wants but the grant does not carry.
type Record struct {
	// WebActionKey is the RAW (un-escaped) E4 action key the approval's grant was
	// written on (browserschema.WebActionValueTransforms escapes it into the
	// SpiceDB object id). Kept raw here because an auditor reads
	// "aa.com/click/place-order-abcd1234", not its spicedb_escape'd form.
	WebActionKey string `json:"webActionKey"`
	// ApprovedFingerprint is the E5 page fingerprint presented at the denied
	// check — the page AS THE HUMAN SAW IT when they approved. Empty ONLY in the
	// degenerate case where the tool could not fingerprint the page (a capture
	// failure); the write path refuses to mint a GRANT for an empty fingerprint
	// (it would be permanently dead), but still records the approval so the
	// absence is itself auditable.
	ApprovedFingerprint string `json:"approvedFingerprint"`
	// ConsequenceTier is the judge's tier for this action (E6/E7), when the
	// decision carried it. Empty when not plumbed through the approval ask.
	ConsequenceTier string `json:"consequenceTier,omitempty"`
	// JudgeRationale is the judge's stated reason, when available. Empty when not
	// plumbed through.
	JudgeRationale string `json:"judgeRationale,omitempty"`
	// ScreenshotRef is the Phase D redacted-screenshot artifact reference shown
	// on the approval card, when one was attached. Empty otherwise.
	ScreenshotRef string `json:"screenshotRef,omitempty"`
	// Decision is the human's answer: "approved" (the only value a write is made
	// for today — a deny mints no grant and records through the generic approval
	// outcome).
	Decision string `json:"decision"`
	// Approver is the canonical subject who actually answered.
	Approver string `json:"approver,omitempty"`
	// ToolName is the browser acting tool whose call was approved.
	ToolName string `json:"toolName,omitempty"`
	// At is the approval timestamp. Stamped ONCE by the caller and reused on
	// retry: the append-only facade compares marshaled bytes, so a re-stamped At
	// turns a safe idempotent re-put into ErrAppendOnlyConflict.
	At time.Time `json:"at"`
}

// KindName is the registered name of this memory Kind.
const KindName = "web_action_approval"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "webact-" }

// WriteAuthority: the runner records its own web_action acting approvals
// (pkg/agent/runner/host_approval.go), on the per-session memory bearer.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

// Retention AppendOnly: an approval against a page fingerprint is audit
// evidence — write-once. A changing re-put is refused (ErrAppendOnlyConflict),
// a per-entry delete is refused, a byte-identical re-put is idempotent.
func (Kind) Retention() memory.Retention                    { return memory.Retention{AppendOnly: true} }
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Record{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
