package v1alpha1

import (
	"reflect"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// StatusFieldOwner identifies the component responsible for writing a given
// AgentSession status field. This file is the ONE authoritative declaration of
// the partition: agentstatus.WriteOwned uses the ownership maps to determine
// which conditions belong to which writer, and TestEveryStatusFieldHasExactlyOneOwner
// fails if a newly-added status field is left unclassified — so ownership cannot
// silently drift.
type StatusFieldOwner string

const (
	OwnerOperator  StatusFieldOwner = "agentsession"           // operator agentsession controller
	OwnerApprovals StatusFieldOwner = "agentsession-approvals" // channelsd approval pipeline
)

// approvalOwnedStatusFields are the json names of AgentSessionStatus scalar/slice
// fields owned by channelsd (both the approval surface and lifecycle signals).
// `conditions` is owned per-type (IsApprovalCondition), not as a whole field, so
// it is intentionally absent here.
var approvalOwnedStatusFields = map[string]bool{
	"pendingRequesters":           true,
	"pendingInteractions":         true,
	"appliedInteractPermission":   true,
	"appliedInteractPermissionAt": true,
	// startFailure is a channelsd-owned lifecycle signal (set on authz-write failure
	// at session creation); the operator reads it and drives the Failed state.
	"startFailure": true,
}

// operatorOwnedStatusFields are the json names owned by the operator. Every
// AgentSessionStatus field except `conditions` must appear in exactly one of the
// two maps (enforced by TestEveryStatusFieldHasExactlyOneOwner).
var operatorOwnedStatusFields = map[string]bool{
	"observedGeneration": true,
	"phase":              true,
	"startedAt":          true,
	"finishedAt":         true,
	"lastWakeAt":         true,
	// Stamped by the operator when it acts on a UI-serve request, exactly as
	// lastWakeAt is for a conversation wake. Separate fields because the two
	// requests must not consume each other — see AnnotationUIServeRequestedAt.
	"lastUIServeAt":          true,
	"lastIdleAt":             true,
	"sleptAt":                true,
	"awaitingUserInputSince": true,
	// parentExchange is written by the RUNNER of a delegated child (the
	// ask_parent meta tool, and the loop's two answer-arrived clears), like
	// awaitingUserInputSince beside it: channelsd's approval surface never
	// touches it, so it sits on this side of the partition. The SubagentRequest
	// controller also clears .pending on the one path that refuses an exchange
	// (refuseExchange), which is a controller write to a runner-owned field and
	// is deliberate: the question it refuses is one no reply will ever answer.
	"parentExchange": true,
	// inputRequest is written by the RUNNER of a delegated child too — the
	// request_input meta tool — and sits beside parentExchange for the same
	// reason. The two are separate fields because they carry different asks: a
	// QUESTION whose answer is the parent's words, and a request for a DATUM
	// whose delivery is a slot binding that grading may route to a human.
	// Folding them together would make "the parent replied" and "the data
	// arrived" the same event, when the first can happen without the second.
	"inputRequest": true,
	// agentWakeCredit is written by CHANNELSD's inbound pipeline, which is the
	// only party that sees a turn's origin — it decides human-or-agent and
	// refills or spends in the same step.
	//
	// It sits on this side of the partition for the reason the field itself
	// exists: the sessions it bounds are the ones talking to each other, so a
	// budget either of them could write would be widened by the very loop it
	// stops. Same discipline as SubagentRequest.ExchangesRemaining, which is
	// controller-owned rather than mirrored from the child that it constrains.
	"agentWakeCredit": true,
	// closureDenied is an OBSERVATION about the whole delegation tree, and only
	// the operator can make it: answering it needs a List over sessions, which
	// the runner's Role does not grant at all (agentsessions is pinned to its
	// own name, get/watch/patch). The runner reads the answer off its own
	// object. Same shape as every other derived fact here — the party that can
	// see it writes it, the party that needs it reads it.
	"closureDenied":                 true,
	"runnerPodName":                 true,
	"runnerRestarts":                true,
	"retryAttempts":                 true,
	"bundleSessions":                true,
	"resolvedSidecarToolboxes":      true,
	"resolvedWorkspaceSource":       true,
	"resolvedContentGuardDetectors": true,
	"resolvedSkillBundles":          true,
	"observedPins":                  true,
	// sidecarReachability is written by the RUNNER (the session-layer sidecar
	// prober), like observedPins/progress — the operator never writes it, so a
	// runner merge-patch of it never conflicts with the operator rebuilding the
	// sibling operator-owned resolvedSidecarToolboxes array.
	"sidecarReachability": true,
	"progress":            true,
	"runDuration":         true,
	"result":              true,
	// pinnedMessage is the runner's projection of the opening-message status;
	// channelsd only READS it to edit the message, so a runner merge-patch of it
	// never conflicts with a channelsd-owned field. Same side as progress/result.
	"pinnedMessage": true,
	// completionBypasses sits with result for the same reason: both are written
	// by the runner's StatusPatcher, which is on the operator side of the
	// operator/channelsd partition this file draws. channelsd never touches it.
	"completionBypasses":     true,
	"failureReason":          true,
	"runnerNotes":            true,
	"supersededBy":           true,
	"satisfiedSecretOutputs": true,
	"auditPublicKey":         true,
	"auditKeyID":             true,
	"auditChainHeads":        true,
	"effectiveSettings":      true,
	"toolGuard":              true,
	"estimatedCost":          true,
	// effectiveIdentityMode / identityChoiceParkedAt: like toolGuard, these are
	// driven by the runner (which presents the ask|dynamic choice prompt
	// in-conversation, not via channelsd's approval pipeline) and the operator
	// (which parks the session in AwaitingIdentityChoice and enforces the
	// timeout) — neither is part of the channelsd approvals surface.
	"effectiveIdentityMode":  true,
	"identityChoiceParkedAt": true,
	// startApprovalParkedAt: operator-only bookkeeping for the
	// AwaitingStartApproval park deadline; channelsd owns the pending entry
	// and condition, the operator owns the park time and the phase.
	"startApprovalParkedAt": true,
	// passthroughCredHashes is the operator's per-credential projected-content
	// hash observation (agentsession passthrough reconcile). It is compared each
	// reconcile to detect a replaced/removed passthrough credential and emit a
	// per-session credential invalidation. Operator-owned, set-on-change via
	// WriteOwned(OwnerOperator).
	"passthroughCredHashes": true,
	// activeWidgets is written by the RUNNER (applyUIResource's durable
	// status.activeWidgets ref for a persisted MCP-UI widget), like
	// sidecarReachability/progress above — the operator never writes it, so a
	// runner merge-patch of it never conflicts with any operator-owned field.
	"activeWidgets": true,
	// credentialAuthFailures is written by the RUNNER (the credential-update
	// corroboration recorder — it is the only component that sees a tool
	// call's upstream outcome), like sidecarReachability/activeWidgets above.
	// The operator only READS it, to decide whether a CredentialUpdateRequest
	// is independently corroborated, so a runner merge-patch of it never
	// conflicts with an operator-owned field. Not part of the channelsd
	// approvals surface.
	"credentialAuthFailures": true,
	// permissionSurface is written by the RUNNER at session start, like
	// activeWidgets/sidecarReachability above — it is the enumeration of what
	// the live tool envelope can reach, and only the runner holds that
	// envelope. The operator never writes it, so the runner's single-field
	// merge patch cannot conflict with any operator-owned field.
	"permissionSurface": true,
	// interactorTuplesWritten is written by the agentsession CONTROLLER
	// (writeInteractorTuples, via applyStatus) — a dedup cache of subjects for
	// whom the idempotent agentclass#interactor SpiceDB touch has already
	// landed, like satisfiedSecretOutputs/auditChainHeads beside it: a
	// controller-owned, set-once/append marker channelsd's approvals surface
	// never touches.
	"interactorTuplesWritten": true,
}

// coOwnedStatusFields are the json names of AgentSessionStatus scalar/slice
// fields written by BOTH the operator and channelsd. channelsd sets these fields,
// the operator clears them. They must be absent from operatorOwnedStatusFields
// and approvalOwnedStatusFields — each field must appear in exactly one map.
var coOwnedStatusFields = map[string]bool{
	"pendingRestart": true,
}

// ApprovalConditionTypes are the AgentSession condition types written by channelsd.
// Disjoint from the operator's lifecycle conditions. This drives the SSA OWNERSHIP
// partition (which writer may set each condition) — so it intentionally includes
// channelsd's bookkeeping/lifecycle conditions (InteractPolicyApplied,
// CredentialRequestPublished) that are NOT "awaiting a human decision". For the
// awaiting-approval question, use pendingApprovalConditionTypes, NOT this map.
var ApprovalConditionTypes = map[string]bool{
	AgentSessionConditionPermissionRequestPending:         true,
	AgentSessionConditionStartApprovalPending:             true,
	AgentSessionConditionToolApprovalPending:              true,
	AgentSessionConditionInfoLeakageApprovalPending:       true,
	AgentSessionConditionContentInspectionApprovalPending: true,
	AgentSessionConditionPreconditionWaiverPending:        true,
	AgentSessionConditionPreferenceConfirmPending:         true,
	AgentSessionConditionCredentialRequestPublished:       true,
	AgentSessionConditionInteractPolicyApplied:            true,
}

// IsApprovalCondition reports whether t is a channelsd-owned approval condition.
func IsApprovalCondition(t string) bool { return ApprovalConditionTypes[t] }

// pendingApprovalConditionTypes are the channelsd-owned conditions that mean
// "this session is BLOCKED awaiting a human approval decision" — a STRICT
// SUBSET of ApprovalConditionTypes, which also carries bookkeeping conditions
// written for ownership and dedup: InteractPolicyApplied is set True at
// creation for EVERY session, and CredentialRequestPublished is the dedup flag
// for the separate AwaitingCredentials phase. Treating either as "awaiting
// approval" stranded passthrough-only sessions in AwaitingDecision with no
// prompt to act on, so ApprovalConditionsActive consults THIS map.
var pendingApprovalConditionTypes = map[string]bool{
	AgentSessionConditionPermissionRequestPending:         true,
	AgentSessionConditionToolApprovalPending:              true,
	AgentSessionConditionInfoLeakageApprovalPending:       true,
	AgentSessionConditionContentInspectionApprovalPending: true,
	AgentSessionConditionPreconditionWaiverPending:        true,
	AgentSessionConditionPreferenceConfirmPending:         true,
}

// IsPendingApprovalCondition reports whether t, when True, means the session is
// awaiting a human approval decision (a strict subset of IsApprovalCondition).
func IsPendingApprovalCondition(t string) bool { return pendingApprovalConditionTypes[t] }

// ApprovalConditionsActive reports whether the session is awaiting a human
// decision: any pending-approval queue is non-empty, or any pending-approval
// condition is True. The operator — the sole writer of phase — derives
// phase=AwaitingDecision from it. channelsd's own bookkeeping conditions that
// do not block on a human are deliberately excluded; see
// pendingApprovalConditionTypes.
func ApprovalConditionsActive(s *AgentSession) bool {
	if len(s.Status.PendingRequesters) > 0 ||
		len(s.Status.PendingInteractions) > 0 {
		return true
	}
	for i := range s.Status.Conditions {
		c := &s.Status.Conditions[i]
		if IsPendingApprovalCondition(c.Type) && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// StartedByCanonical returns the BARE canonical SpiceDB subject (no "user:"
// prefix) of the human who created sess, read from
// AnnotationStartedByCanonicalID. The annotation stores the "user:<canonical>"
// PREFIXED form (see AnnotationStartedByCanonicalID's doc); this helper strips
// the prefix so callers that need the bare id (e.g. a SpiceDB object id, or
// identity.Principal comparisons) don't hand-roll TrimPrefix at each call
// site. Returns "" when the annotation is absent (kubectl-driven sessions).
// For the prefixed form, use StartedBySubject.
func StartedByCanonical(sess *AgentSession) identity.CanonicalUserID {
	v := sess.Annotations[AnnotationStartedByCanonicalID]
	if v == "" {
		return identity.CanonicalUserID{}
	}
	// The operator wrote this annotation from an already-resolved principal at
	// session creation. Reading it back is not a fresh verification: whether
	// the original starter was IdP-proven is not recoverable from the string.
	return identity.CanonicalFromTrusted(strings.TrimPrefix(v, "user:"),
		"operator-written started-by annotation on this AgentSession")
}

// StartedBySubject returns the canonical SpiceDB subject, "user:<canonical>",
// of the human who created sess, read from AnnotationStartedByCanonicalID
// exactly as stored (no stripping). Use this at sites that compare against or
// emit the prefixed subject form; use StartedByCanonical for the bare id.
func StartedBySubject(sess *AgentSession) identity.Subject {
	return identity.Subject(sess.Annotations[AnnotationStartedByCanonicalID])
}

// StartApprovalPending reports whether sess is parked awaiting a platform
// admin's start_approval decision (AnnotationStartApprovalRequestRef present).
// The operator withholds the runner and owner resolution while this is true.
func StartApprovalPending(sess *AgentSession) bool {
	return StartApprovalRequestRef(sess) != ""
}

// StartApprovalRequestRef returns the RequestRef of the pending start_approval
// request sess is parked on, or "" when it is not parked.
func StartApprovalRequestRef(sess *AgentSession) string {
	return sess.Annotations[AnnotationStartApprovalRequestRef]
}

// StartedByExternalID returns the kind-specific external identity (e.g. a
// Slack user_id) of the human who created sess, read from
// AnnotationStartedByExternalID. Empty when absent.
func StartedByExternalID(sess *AgentSession) identity.RawExternalID {
	return identity.RawExternalID(sess.Annotations[AnnotationStartedByExternalID])
}

// StartedByEmail returns the verified email of the human who created sess,
// read from AnnotationStartedByEmail. Empty when the originating channel kind
// vouched for no email (see AnnotationStartedByEmail's doc).
func StartedByEmail(sess *AgentSession) identity.Email {
	return identity.Email(sess.Annotations[AnnotationStartedByEmail])
}

// ResolveStartedByCanonical returns the canonical id of the human who created
// sess, preferring the canonical the channel pipeline already stamped and
// falling back to a synthetic derived from the external id alone. Empty when
// sess carries neither — a kubectl-driven session has no human starter.
//
// Use this, never a hand-rolled derivation. The two encodings are NOT
// interchangeable: the pipeline keys a starter with a channel-verified email by
// base64url(email), while a derivation with no email in hand can only produce
// the synthetic base64url(kind:teamScope:externalID) — a different subject for
// the same person. Substituting the synthetic loses the email irrecoverably,
// and silently: a subject with no "@" is the ordinary state for a guest or
// foreign-workspace user, so nothing downstream treats it as an error.
//
// The fallback is correct where it applies — an email-less starter is INTENDED
// to be keyed by the synthetic subject — and wrong only as a substitute for a
// canonical that was already resolved.
func ResolveStartedByCanonical(sess *AgentSession) identity.CanonicalUserID {
	if c := StartedByCanonical(sess); !c.IsZero() {
		return c
	}
	externalID := StartedByExternalID(sess)
	if externalID == "" {
		return identity.CanonicalUserID{}
	}
	channelKind := ""
	if sess.Spec.InputChannel != nil {
		channelKind = sess.Spec.InputChannel.Kind
	}
	// The started_by is a channel participant, so keying an email-less one by
	// the synthetic subject is intended — opt in explicitly. With the opt-in
	// Canonical is total; fail safe (empty → treated as no started_by) on the
	// unreachable error rather than mint a phantom subject no grant resolves to.
	c, err := identity.FromExternal(identity.Kind(channelKind), "", externalID, "").AllowSynthetic().Canonical()
	if err != nil {
		return identity.CanonicalUserID{}
	}
	return c
}

// ParentRef returns the delegating session, if this session was delegated.
// Use this rather than reading Spec.Parent directly, so the nil check lives in
// one place.
func (s *AgentSession) ParentRef() (NamespacedRef, bool) {
	if s == nil || s.Spec.Parent == nil {
		return NamespacedRef{}, false
	}
	return *s.Spec.Parent, true
}

// AuthzServiceSubject returns the non-human SpiceDB subject sess acts as when
// its inbound carried no human — the value of AnnotationAuthzServiceSubject,
// which the channelsd pipeline stamps from the input Channel's declared
// spec.authzSubject. Empty for every session a human started, and for a
// kubectl-driven one.
//
// The return type is CanonicalUserID and the value is a FULLY QUALIFIED
// "service:<id>" reference, which reads like a contradiction and is not: a
// canonical that came from an inbound is already allowed to carry its own
// object type, precisely because a kind that supplies no starting user assigns
// the Channel's subject verbatim. CanonicalUserID.SubjectRef is the accessor
// that knows this — use it, not CanonicalUserID.Subject, on anything derived
// from here, or the value double-prefixes into "user:service:<id>" and names
// nothing.
func AuthzServiceSubject(sess *AgentSession) identity.CanonicalUserID {
	if sess == nil {
		return identity.CanonicalUserID{}
	}
	// A Channel's spec.authzSubject, written onto the session by the operator.
	// It is a tenant-authored value the platform accepted, not a proven
	// identity — see the audit note on authzSubject.
	return identity.CanonicalFromTrusted(sess.Annotations[AnnotationAuthzServiceSubject],
		"Channel spec.authzSubject, tenant-authored and accepted at admission")
}

// jsonFieldName returns a struct field's json name (tag minus options); "" for "-"
// or an untagged field.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		return tag[:i]
	}
	return tag
}
