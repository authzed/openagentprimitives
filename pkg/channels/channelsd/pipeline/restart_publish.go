package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// NewRestartPublisher returns a RestartSubmitFunc that:
//  1. Resolves the trigger user's canonical subject.
//  2. Computes a deterministic child session name.
//  3. Patches AgentSession.Status.PendingRestart so the operator's
//     reconciler picks up the restart on its next watch event.
//  4. Publishes a KindRestartTrigger envelope on NATS for
//     observability. Best-effort — a publish failure is logged but
//     doesn't block the reconciler since the CR-status patch is the
//     authoritative trigger.
//
// signer attests the marker in step 3. The operator refuses an unsigned marker
// (the session's own runner can write that field too — see pkg/agent/restartmarker),
// so a nil signer is an error rather than an unsigned write.
func NewRestartPublisher(publish channelevents.PublishFunc, k8s client.Client, signer *restartmarker.Signer) channelkinds.RestartSubmitFunc {
	return func(ctx context.Context, t channelkinds.RestartTrigger) error {
		// The forker must resolve to a real user — the SessionFork gate keys
		// agentsession#fork on the owner's user:<email>. A dropped email would
		// mint a synthetic subject that fails the owner-check and denies the
		// owner the right to restart their own session; surface it as an error
		// instead of silently minting the phantom subject.
		canonical, err := identity.FromExternal(
			identity.Kind(t.TriggeredBy.Kind),
			identity.TeamScope(t.TriggeredBy.TeamScope),
			identity.RawExternalID(t.TriggeredBy.ExternalID),
			identity.Email(t.TriggeredBy.Email),
		).Subject()
		if err != nil {
			return fmt.Errorf("NewRestartPublisher: forker identity unresolved (no verified email): %w", err)
		}
		target := deterministicForkName(t.SessionRef.Name, int(t.CutTurnIndex), t.NewUserText)
		payload := channelevents.RestartTriggerPayload{
			CutTurnIndex:      t.CutTurnIndex,
			NewUserText:       t.NewUserText,
			TriggeredBy:       canonical,
			TargetSessionName: target,
			KindRequestRef:    t.KindRequestRef,
		}
		if verr := payload.Validate(); verr != nil {
			return fmt.Errorf("NewRestartPublisher: validate: %w", verr)
		}

		// 1. Patch AgentSession.Status.PendingRestart — the authoritative
		//    trigger for the operator's restart reconciler.
		var sess spiceboxv1alpha1.AgentSession
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: t.SessionRef.Namespace, Name: t.SessionRef.Name}, &sess); err != nil {
			return fmt.Errorf("NewRestartPublisher: get session: %w", err)
		}
		patched := sess.DeepCopy()
		patched.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
			CutTurnIndex:      t.CutTurnIndex,
			NewUserText:       t.NewUserText,
			TriggeredBy:       canonical,
			RequestedAt:       metav1.Now(),
			TargetSessionName: target,
		}
		if signer == nil {
			return fmt.Errorf("NewRestartPublisher: no restart-marker signer configured; refusing to write a marker the operator will reject")
		}
		if err := signer.Sign(patched, patched.Status.PendingRestart); err != nil {
			return fmt.Errorf("NewRestartPublisher: sign marker: %w", err)
		}
		if err := applyApprovalStatus(ctx, k8s, patched, &sess); err != nil {
			return fmt.Errorf("NewRestartPublisher: apply agentsession approval status: %w", err)
		}

		// 2. NATS envelope (best-effort, observability only).
		if err := channelevents.PublishIn(publish, t.SessionRef.Namespace, t.SessionRef.Name,
			channelevents.KindRestartTrigger, payload); err != nil {
			log.FromContext(ctx).Info("NewRestartPublisher: NATS publish failed (best-effort)",
				"session", t.SessionRef.Namespace+"/"+t.SessionRef.Name, "err", err.Error())
		}
		return nil
	}
}

// maxParentLen is the maximum number of bytes taken from the parent
// session name before appending the "-fk<6-hex>" suffix. K8s names are
// capped at 253 chars; the suffix is 9 chars ("-fk" + 6 hex digits),
// so the parent prefix may be at most 244 chars.
const maxParentLen = 244 // 253 - len("-fk") - 6 (hex) = 244

// deterministicForkName computes the child session name. Schema:
//
//	<parent>-fk<6-hex-of-sha256(cutTurn || newText)>
//
// parent+hash makes the name idempotent for the same
// (parent, cut, edit) tuple — a duplicate trigger from two channelsd
// replicas yields the same name, so the controller's Create returns
// AlreadyExists harmlessly.
//
// If parent exceeds maxParentLen bytes it is truncated before the
// suffix is appended so the resulting name never exceeds 253 chars.
func deterministicForkName(parent string, cutTurn int, newText string) string {
	if len(parent) > maxParentLen {
		parent = parent[:maxParentLen]
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d|", cutTurn)
	h.Write([]byte(newText))
	sum := h.Sum(nil)
	return fmt.Sprintf("%s-fk%s", parent, hex.EncodeToString(sum[:3])) // 6 hex chars
}

// deterministicInheritName computes the child session name for a
// continuation-inherit fork. Schema:
//
//	<parent>-ic<6-hex-of-sha256(newText)>
//
// The "-ic" suffix (inherit-continuation) keeps it from colliding with the
// parent or with a restart child ("-fk"). Deterministic on (parent, newText)
// so a duplicate inbound from two channelsd replicas yields the same name —
// the reconciler's Create then returns AlreadyExists harmlessly. There is no
// cut turn in inherit mode (the whole transcript carries), so newText is the
// only hashed input.
func deterministicInheritName(parent, newText string) string {
	if len(parent) > maxParentLen {
		parent = parent[:maxParentLen]
	}
	h := sha256.New()
	h.Write([]byte(newText))
	sum := h.Sum(nil)
	return fmt.Sprintf("%s-ic%s", parent, hex.EncodeToString(sum[:3])) // 6 hex chars
}

// deterministicTakeoverName computes the child session name for a different-user
// takeover fork. Schema:
//
//	<parent>-tk<6-hex-of-sha256(ownerCanonical\x00newText)>
//
// The "-tk" suffix (takeover) keeps it distinct from the parent, a restart
// child ("-fk"), and an inherit child ("-ic"). The new owner is hashed in
// alongside the text so two DIFFERENT users taking the same terminal thread
// over yield DIFFERENT child names (they must not collide). Deterministic on
// (parent, owner, newText) so a duplicate inbound from two channelsd replicas
// yields the same name — Create then returns AlreadyExists harmlessly.
func deterministicTakeoverName(parent, ownerCanonical, newText string) string {
	if len(parent) > maxParentLen {
		parent = parent[:maxParentLen]
	}
	h := sha256.New()
	h.Write([]byte(ownerCanonical))
	h.Write([]byte{0})
	h.Write([]byte(newText))
	sum := h.Sum(nil)
	return fmt.Sprintf("%s-tk%s", parent, hex.EncodeToString(sum[:3])) // 6 hex chars
}
