package runner

// slot_pin_mirror.go mirrors SpiceDB's slot_pin relation onto AgentSession
// status.slotPins, for every runner path that can produce a pin:
//
//   - the plan-gate approval (narrowToApproved, host_approval.go) — first
//     fills AND moves, the one path that can stamp MovedBy/MovedAt;
//   - the class-defaults bind at session start (internal/cmd/runner/main.go,
//     via the exported MirrorDeclaredSlotPins);
//   - the extracted promotion (loop_dispatch.go) and the observed promotion
//     (loop_autofill.go), via mirrorBoundSlotPins.
//
// The mirror is derived from ACTUAL SpiceDB state — SlotPinner.ReadPin per
// involved single-occupancy type — never from the bindings a caller ASKED to
// write. The two genuinely differ: authz.BindApproved under
// EnforcePreconditions silently drops Refused/Undetermined candidates and
// returns nil having bound nothing, and the promotion paths report only
// overall success, so a binding-derived mirror would claim pins SpiceDB never
// wrote. ReadPin returning "" for a type therefore mirrors NOTHING for it.
//
// Cost is bounded by skipping the read for a type already present in
// status.slotPins: a pin never changes outside an approved move, and the move
// site refreshes the mirror itself (its types are exempt from the skip), so a
// mirrored type is never re-read — steady-state cost zero. Only a declared
// single-occupancy type that has never bound keeps paying one advisory read
// per mirror call.
import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// recordSlotPin mirrors one pin observation onto status.slotPins, best
// effort: SpiceDB's slot_pin relation is the enforcement truth, so a mirror
// failure must never retroactively fail the bind that already succeeded.
// Logged rather than swallowed, per the no-silent-errors rule — same shape as
// sidecarsynth.go's recordReachability wrapping RecordSidecarReachability.
func recordSlotPin(ctx context.Context, sp *StatusPatcher, pin spiceboxv1alpha1.SlotPin) {
	if sp == nil {
		return
	}
	if err := sp.RecordSlotPin(ctx, pin); err != nil {
		slog.Default().Info("slot pin mirror failed; status.slotPins may lag SpiceDB's slot_pin relation",
			"resourceType", pin.ResourceType, "resourceID", pin.ResourceID, "movedBy", pin.MovedBy, "err", err.Error())
	}
}

// planGateApprovalRef derives the stable reference SlotPin.MovedBy records
// for a move a plan-gate approval authorized. PhaseKey identifies the
// approved phase by its authority (the permissions it holds and the
// instances it names) and is what a current record carries; PlanDigest
// plus the active phase index is the fallback for a record written before
// PhaseKey existed, the same fallback the fold itself uses (see
// plangateaudit.Content.PhaseKey's doc comment).
func planGateApprovalRef(rec plangateaudit.Content) string {
	if rec.PhaseKey != "" {
		return rec.PhaseKey
	}
	return fmt.Sprintf("%s#%d", rec.PlanDigest, phaseIndexOf(rec))
}

// pinMirrorMove marks a resource type the current call MOVED: ref is the
// approval-record reference MovedBy records, toID the instance the approval
// repointed the pin to. MovedBy is stamped only when the pin READ BACK equals
// toID — a pin that is somewhere else was not produced by this move, and
// attributing it would record a move nobody made.
type pinMirrorMove struct {
	ref  string
	toID string
}

// mirrorSlotPinsFromSpiceDB reads the CURRENT pin for each type and mirrors
// exactly what SpiceDB holds — see the file comment for why the read, the
// skip, and the empty-read-mirrors-nothing rule each exist. types carries the
// single-occupancy resource types the finished call attempted (the caller
// filters out multi-occupancy, which holds no pin); moves the subset it moved.
//
// The already-mirrored skip consults status through sp.Get once per call. A
// failed Get degrades to "nothing mirrored yet" — at worst a redundant
// advisory read plus an upsert that UpsertSlotPin's same-instance carve-out
// turns into a no-op — because skipping the whole mirror on a transient read
// error would leave a fresh pin invisible until the next bind.
func mirrorSlotPinsFromSpiceDB(
	ctx context.Context,
	sp *StatusPatcher,
	pinner authz.SlotPinner,
	sess authz.SessionRef,
	types []string,
	moves map[string]pinMirrorMove,
	now time.Time,
) {
	if sp == nil || sp.IsLocal() || pinner == nil || len(types) == 0 {
		return
	}
	mirrored := make(map[string]bool)
	var cur spiceboxv1alpha1.AgentSession
	if err := sp.Get(ctx, &cur); err != nil {
		slog.Default().Info("slot pin mirror could not read current status; mirroring without the already-mirrored skip",
			"session", sess.String(), "err", err.Error())
	} else {
		for _, p := range cur.Status.SlotPins {
			mirrored[p.ResourceType] = true
		}
	}
	movedAt := metav1.NewTime(now)
	seen := make(map[string]bool, len(types))
	for _, rt := range types {
		if rt == "" || seen[rt] {
			continue
		}
		seen[rt] = true
		mv, moved := moves[rt]
		if !moved && mirrored[rt] {
			// Already mirrored, and pins change only through the move path,
			// whose types bypass this skip — nothing to re-read.
			continue
		}
		id, err := pinner.ReadPin(ctx, rt, sess)
		if err != nil {
			slog.Default().Info("slot pin mirror could not read the pin; status.slotPins may lag SpiceDB",
				"session", sess.String(), "resourceType", rt, "err", err.Error())
			continue
		}
		if id == "" {
			// Nothing pinned — the bind may have dropped every candidate for
			// this type (a Refused precondition, an approver holding nothing).
			// Mirroring the attempt would claim a pin SpiceDB never wrote.
			continue
		}
		pin := spiceboxv1alpha1.SlotPin{ResourceType: rt, ResourceID: id}
		if moved && mv.toID == id {
			pin.MovedBy = mv.ref
			pin.MovedAt = &movedAt
		}
		recordSlotPin(ctx, sp, pin)
	}
}

// mirrorApprovedSlotPins reflects a nil-error authz.BindApproved onto
// status.slotPins: every single-occupancy type the approved bindings name is
// re-read from SpiceDB and mirrored as found. A binding carrying a non-zero
// PriorID asked to MOVE the pin — moveApprovedPins runs before bindSlots
// inside BindApproved, so on the nil-error path the move already landed — and
// its type gets MovedBy (planGateApprovalRef(rec)) and MovedAt stamped, but
// only when the read-back pin equals the move's target.
//
// Deliberately NOT called on BindApproved's error paths: a non-nil error can
// leave SpiceDB mid-way (a move landed, a later type refused), and the
// existing error handling already treats that as "did not narrow the session"
// without accounting per type. status.slotPins is display-only and may lag;
// the next successful bind or move catches it up.
func mirrorApprovedSlotPins(
	ctx context.Context, sp *StatusPatcher, binder authz.RelWriter, sess authz.SessionRef,
	rec plangateaudit.Content, bindings []authz.SlotBinding, now time.Time,
) {
	// Same assertion shape as hooks_dataplane.go: a binder that is not a
	// SlotPinner cannot have pinned anything (GrantSlots refuses
	// single-occupancy types for it), so there is nothing to read or mirror.
	pinner, ok := binder.(authz.SlotPinner)
	if !ok {
		return
	}
	var types []string
	var moves map[string]pinMirrorMove
	seen := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		// Multi-occupancy slots hold no pin — SlotPin mirrors the
		// single-occupancy commitment, not the open-ended multi-occupancy
		// grant set (that is what `oap session slots` lists).
		if b.ResourceType == "" || b.Occupancy == authz.SlotOccupancyMulti {
			continue
		}
		if !seen[b.ResourceType] {
			seen[b.ResourceType] = true
			types = append(types, b.ResourceType)
		}
		if !b.PriorID.IsZero() {
			if moves == nil {
				moves = make(map[string]pinMirrorMove)
			}
			if _, dup := moves[b.ResourceType]; !dup {
				moves[b.ResourceType] = pinMirrorMove{ref: planGateApprovalRef(rec), toID: b.ResourceID.String()}
			}
		}
	}
	mirrorSlotPinsFromSpiceDB(ctx, sp, pinner, sess, types, moves, now)
}

// mirrorBoundSlotPins mirrors the pin state of every single-occupancy slot
// type the given specs declare, after one of the non-approval bind paths
// (class defaults, extracted promotion, observed promotion) reported overall
// success. Those paths never say which instance (if any) a type ended up
// pinned to — their signatures return only error — so the mirror reads the
// answer from SpiceDB. First-fill shape only: no MovedBy/MovedAt, because a
// non-approval path can never move a pin.
func (l *Loop) mirrorBoundSlotPins(ctx context.Context, specs []authz.BoundEntitySpec) {
	if l == nil || len(specs) == 0 {
		return
	}
	pinner, ok := l.SlotBinder.(authz.SlotPinner)
	if !ok {
		return
	}
	var types []string
	for _, et := range specs {
		// Empty Occupancy means single (occupancyOf's fail-closed default).
		if et.ResourceType != "" && et.Occupancy != authz.SlotOccupancyMulti {
			types = append(types, et.ResourceType)
		}
	}
	mirrorSlotPinsFromSpiceDB(ctx, l.Status, pinner, l.authzSessionRef(), types, nil, time.Now())
}

// MirrorDeclaredSlotPins is mirrorBoundSlotPins for a caller outside this
// package that holds no spec slice — internal/cmd/runner, after the
// cold-start BindClassDefaults — deriving the declared slots from the Loop's
// own AgentClass. A class whose slot preconditions do not compile mirrors
// nothing, exactly as every bind path skips such a class.
func (l *Loop) MirrorDeclaredSlotPins(ctx context.Context) {
	if l == nil || l.AgentClass == nil || len(l.AgentClass.Spec.GetSlots()) == 0 {
		return
	}
	specs, err := boundEntitySpecsForAutofill(l.AgentClass)
	if err != nil {
		// The bind this mirrors was itself skipped for the same reason and
		// said so; one more line here would be noise.
		return
	}
	l.mirrorBoundSlotPins(ctx, specs)
}
