package runner

// leakage_notice.go implements the info_leakage_notice producer: the read-only
// FYI sibling of buildLeakagePending (host_approval.go). Fired when the
// info-leakage audience gate (pkg/authz/hooks/infoleakaudience.go,
// InfoLeakAudience.enforceAudienceForTaint) runs in "logging" mode — a leak is
// detected but auto-allowed, since logging mode never blocks the response — so
// instead of pausing for a data owner's approval this delivers a notice card to
// the requester. Wired via InfoLeakAudienceDeps.PublishNotice
// (pipeline_wiring.go's infoLeakAudienceDeps), gated by
// InformationLeakagePolicy.ResolvedLoggingNoticeToRequester (default false;
// opt-in).
//
// The notice rides categories.InfoLeakageNotice — a nil-actions generic
// Interaction, the same shape credential_link/portal_access use.

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/stringsx"
)

// publishLeakageNotice builds + publishes the info_leakage_notice
// interaction_request. requester is the PER-CALL principal whose read caused
// the would-block event (the widget viewer on a proxy-exec, the session subject
// on the LLM path), threaded in from pipeline.Input.Requester by the audience
// hook rather than read back out of loop state — see Loop.RequesterCanonicalID.
// Delivered to the requester only (Audience.Requester) with nil Actions (no
// decision leg, per InteractionRequestPayload.Validate's read-only-notice
// carve-out), so the generic renderer delivers it on every surface exactly like
// credential_link/portal_access.
//
// HARD-ERRORS on an empty leakedTo, mirroring buildLeakagePending's invariant: a
// notice naming no recipient is meaningless — refuse to build/deliver it rather
// than post a notice about sharing data with nobody.
func (l *Loop) publishLeakageNotice(ctx context.Context, requester identity.CanonicalUserID, leakedTo []string, taint []infoleakagetaint.TaintRecord) error {
	if l.InteractionRequestPublish == nil {
		return fmt.Errorf("info-leakage notice unavailable (interaction publish hook not configured)")
	}
	if len(leakedTo) == 0 {
		return fmt.Errorf("info-leakage notice has no recipient (leaked_to empty); refusing to notify")
	}
	if l.RequesterCanonicalID == nil {
		return fmt.Errorf("info-leakage notice unavailable (requester identity hook not configured)")
	}
	subject, serr := l.RequesterCanonicalID(ctx, requester)
	if serr != nil {
		return fmt.Errorf("info-leakage notice: resolve requester: %w", serr)
	}
	if subject == "" {
		// No channel identity to notify (e.g. a kubectl-driven session with no
		// channel binding) — nothing to deliver, not an error.
		return nil
	}

	sessNS, sessName := l.SessionKey.Namespace, l.SessionKey.Name
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        categories.InfoLeakageNotice,
		RequestRef:      newRequestID(),
		Lead:            infoLeakageNoticeLead(),
		Fields:          infoLeakageNoticeFields(l.ChannelKind, taint, leakedTo),
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Subject: subject},
		},
	}
	env, eerr := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if eerr != nil {
		return fmt.Errorf("build info-leakage notice envelope: %w", eerr)
	}
	return l.InteractionRequestPublish(ctx, sessNS, sessName, env)
}

// --- info_leakage_notice render copy. The renderer supplies the structure
// (tone chip, bold lead, "• *Label*: Value" fields); this file supplies only
// the words. ---

// infoLeakageNoticeLead is fixed text. Unlike infoLeakageLead (the approval
// card) it does not fold in the summarizer's proposed-share text — that goes
// in the "Accessed resources" field's summary instead.
func infoLeakageNoticeLead() string { return "Information may be shared" }

// infoLeakageNoticeFields emits the "Accessed resources" (source list) and
// "Will be shared with" (recipient list) sections as Fields. Values are
// rune-capped via the shared stringsx.CapRunes.
//
// "Will be shared with" also stamps Mentions with the FULL structured identity
// of each recipient (standing rule: carry structured identity, not just a
// flattened string — mirrors infoLeakageWouldShareWithFields). channelKind is
// the session's channel kind (l.ChannelKind); each leakedTo entry is a resolved
// canonical subject ("user:<canonical>") carried verbatim via Subject, never
// ExternalID.
func infoLeakageNoticeFields(channelKind string, taint []infoleakagetaint.TaintRecord, leakedTo []string) []channelevents.InteractionField {
	var fields []channelevents.InteractionField

	if len(taint) > 0 {
		var sb strings.Builder
		for i, t := range taint {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(fmt.Sprintf("• `%s:%s` via `%s`", t.ResourceType, t.ResourceID, t.ToolName))
		}
		fields = append(fields, channelevents.InteractionField{
			Label: "Accessed resources",
			Value: stringsx.CapRunes(sb.String(), 2800),
		})
	}

	// leakedTo is guaranteed non-empty by the caller (enforceAudienceForTaint
	// only reaches the "logging" case after leakedTo := subtractSubjects(...)
	// is confirmed non-empty) and by publishLeakageNotice's own hard-error
	// guard above — no length check needed, mirrors
	// infoLeakageWouldShareWithFields's unconditional field emission.
	names := make([]string, 0, len(leakedTo))
	mentions := make([]channelevents.ExternalIdentity, 0, len(leakedTo))
	for _, s := range leakedTo {
		names = append(names, strings.TrimPrefix(s, "user:"))
		mentions = append(mentions, channelevents.ExternalIdentity{
			Kind:    identity.Kind(channelKind),
			Subject: identity.Subject(s),
		})
	}
	fields = append(fields, channelevents.InteractionField{
		Label:    "Will be shared with",
		Value:    stringsx.CapRunes(strings.Join(names, ", "), 2800),
		Mentions: mentions,
	})

	return fields
}
