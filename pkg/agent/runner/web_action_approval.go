package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/webactionapproval"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// webActionApprovalWriteTimeout bounds the two durable side effects of a
// resolved web_action approval (the grant write and the audit append). They run
// on context.Background() rather than the turn ctx: the human has already
// decided, so a turn that is wrapping up must not abandon the record of what
// they consented to — the same reason recordApprovalOutcome detaches its write.
const webActionApprovalWriteTimeout = 10 * time.Second

// webActionGrantBinding builds the SlotBinding an approved browser mutate mints:
// a web_action#act grant on the ESCAPED action key, caveated with
// pagestate_matches so it resolves only while the live page still matches the
// fingerprint the human approved against.
//
// It is a pure function with no I/O so the exact binding shape — escaped id,
// caveat name, caveat context parameter — is unit-testable without a SpiceDB,
// and so the dispatch-side integration test can pin that it writes a grant the
// E8 check then accepts. The SINGLE place this binding is constructed: a second
// hand-rolled copy that spelled the caveat or its parameter differently would
// mint a grant no check could ever satisfy, silently.
//
// An EMPTY fingerprint is refused (ErrEmptyWebActionFingerprint) rather than
// turned into a grant: pagestate_matches guards `presented_fingerprint != "" &&
// approved_fingerprint == presented_fingerprint`, so an approved_fingerprint of
// "" can never match any live page — the grant would be permanently dead, and a
// dead grant that reads as "approved" is worse than none.
func webActionGrantBinding(rawKey, fingerprint string) (authz.SlotBinding, error) {
	if fingerprint == "" {
		return authz.SlotBinding{}, errEmptyWebActionFingerprint
	}
	id, err := authz.NewObjectID(rawKey, browserschema.WebActionValueTransforms())
	if err != nil {
		return authz.SlotBinding{}, err
	}
	return authz.SlotBinding{
		ResourceType:  browserschema.WebActionType,
		ResourceID:    id,
		Permission:    browserschema.ActPermission,
		CaveatName:    browserschema.PagestateMatchesCaveat,
		CaveatContext: map[string]any{browserschema.ApprovedFingerprintParam: fingerprint},
	}, nil
}

// errEmptyWebActionFingerprint is the degenerate-case sentinel: the mutate was
// approved but the tool could not fingerprint the page it acted on. No grant is
// minted (one would be permanently dead); the approval is still audited so the
// gap is itself a record.
var errEmptyWebActionFingerprint = errEmptyFingerprint{}

type errEmptyFingerprint struct{}

func (errEmptyFingerprint) Error() string {
	return "web_action approval: empty page fingerprint — refusing to mint a permanently-dead caveated grant"
}

// persistWebActionApproval is the E9 approval-write effect for a browser mutate:
// it mints the fingerprint-caveated web_action grant that lets the subsequent
// identical acting call pass without re-prompting, and appends the append-only
// web_action_approval audit record of what was approved against which page.
//
// Called ONLY on an approve of a tool_call whose pendingToolCall carries a
// web_action key (set by the ToolCallAuthz hook from the denied check's Inputs).
// Both halves are BEST-EFFORT: the current call already proceeded on the human's
// yes (External tools are not re-checked post-approval), so losing the grant
// costs at most a re-approval of the next identical action, and losing the audit
// must never take down the turn — but neither is ever silent.
func (h *runnerHost) persistWebActionApproval(_ context.Context, tc *pendingToolCall, d approval.Decision) {
	if tc == nil || tc.webActionKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), webActionApprovalWriteTimeout)
	defer cancel()

	sessionRef := tc.sessNS + "/" + tc.sessName

	// The grant. Skipped (but still audited below) when the fingerprint is empty,
	// or when the key will not escape — either way the next acting call re-asks,
	// which is the fail-closed direction.
	binding, berr := webActionGrantBinding(tc.webActionKey, tc.webActionFingerprint)
	switch {
	case berr != nil:
		slog.Default().Info("web_action approval: no grant minted; the next identical acting call will re-ask (fail-closed)",
			"session", sessionRef, "tool", tc.toolName, "webActionKey", tc.webActionKey, "reason", berr.Error())
	case h.l.SlotBinder == nil:
		// No writer wired (a test, or kubectl-driven): the approval is audited but
		// nothing is granted. GrantSlots would no-op on a nil writer anyway; say so
		// rather than pretend a grant was written.
		slog.Default().Info("web_action approval: no slot-grant writer wired; approval audited but not granted",
			"session", sessionRef, "tool", tc.toolName)
	default:
		var sessionExpiration time.Duration
		if h.l.Budget != nil {
			sessionExpiration = h.l.Budget.cfg.SessionExpiration.Duration
		}
		expiry := authz.SlotGrantExpiry(time.Now(), sessionExpiration)
		sess := authz.SessionRef{Namespace: tc.sessNS, Name: tc.sessName}
		if err := authz.GrantSlots(ctx, h.l.SlotBinder, sess, []authz.SlotBinding{binding}, expiry); err != nil {
			slog.Default().Info("web_action approval: minting the caveated grant failed; the next identical acting call will re-ask",
				"session", sessionRef, "tool", tc.toolName, "resource", browserschema.WebActionType+":"+binding.ResourceID.String(), "err", err.Error())
		}
	}

	// The audit record — written regardless of whether a grant was minted, so an
	// empty-fingerprint approval (grant skipped) is still evidence of what a human
	// consented to. tc.useID keys the record's for_tool_call link; a missing useID
	// means the audit cannot be paired, so it is skipped loudly rather than
	// written orphaned. tc.reqID (the ask's own request id) ALSO disambiguates the
	// entry id itself — see webactionapproval.Append's doc for why toolUseID alone
	// is not always unique per approval EVENT.
	if h.l.Mem == nil || tc.useID == "" {
		slog.Default().Info("web_action approval: not audited (no memory store or no tool-use id)",
			"session", sessionRef, "tool", tc.toolName, "haveMem", h.l.Mem != nil, "useID", tc.useID)
		return
	}
	scope := memory.Scope{Kind: "session", ID: sessionRef}
	rec := webactionapproval.Record{
		WebActionKey:        tc.webActionKey,
		ApprovedFingerprint: tc.webActionFingerprint,
		Decision:            "approved",
		Approver:            d.ApproverID,
		ToolName:            tc.toolName,
		At:                  hostNow(),
	}
	if err := webactionapproval.Append(ctx, h.l.Mem, scope, tc.useID, tc.reqID, rec); err != nil {
		slog.Default().Info("web_action approval: appending the audit record failed",
			"session", sessionRef, "tool", tc.toolName, "err", err.Error())
	}
}
