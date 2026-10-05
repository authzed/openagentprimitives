package runner

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// hooks_dataplane.go registers the runner's data-plane pipeline hooks as
// HookFactory entries (see hookregistry.go). Each factory owns its own
// activation condition and construction; hookregistry_test.go pins every hook's
// name, order and point.
func init() {
	RegisterHook(HookFactory{Name: "mcp_trust", Order: hooks.OrderMcpTrust, Build: func(l *Loop) []pipeline.Hook {
		if !l.hasMCPTools() {
			return nil
		}
		return []pipeline.Hook{hooks.NewMcpTrust(l.mcpSpecLookup())}
	}})

	RegisterHook(HookFactory{Name: "tool_guard", Order: hooks.OrderToolGuard, Build: func(l *Loop) []pipeline.Hook {
		if l.ToolGuardPolicy == nil {
			return nil
		}
		l.toolGuardReg = toolguard.NewRegistry(nil)
		return []pipeline.Hook{
			toolguard.NewGuard(l.toolGuardDeps()),
			toolguard.NewGuardRecord(l.toolGuardRecordDeps()),
		}
	}})

	RegisterHook(HookFactory{Name: "content_guard", Order: hooks.OrderContentGuard, Build: func(l *Loop) []pipeline.Hook {
		var out []pipeline.Hook
		for idx, inst := range l.ContentInspectors {
			out = append(out, contentguard.NewAdapter(
				l.contentInspectorID(idx), inst, l.recordContentGuardEvent, slog.Default(),
				timeoutPolicyFor("content_inspection"),
			))
		}
		return out
	}})

	// plan_gate activates on its OWN resolved mode, independent of
	// toolCalls.mode. Wiring it behind ToolCallAuthz would make it evaporate
	// silently whenever tool-call authz is permissive or disabled.
	RegisterHook(HookFactory{Name: "plan_gate", Order: hooks.OrderPlanGate, Build: func(l *Loop) []pipeline.Hook {
		if l.PlanGateMode == "" || l.PlanGateMode == "disabled" {
			return nil
		}
		// The SlotBinder is a *spicedb.RelationWriter in production, which
		// implements authz.SlotPinner; the card uses it to read the current pin
		// and render a move. Type-asserted into an interface variable rather than
		// assigned as a pointer, so a nil or non-pinner binder yields a true nil
		// interface (never a typed-nil that would panic on ReadPin) — the gate
		// then renders every card as a first-fill, which is correct.
		var slotPinner authz.SlotPinner
		if sp, ok := l.SlotBinder.(authz.SlotPinner); ok {
			slotPinner = sp
		}
		return []pipeline.Hook{hooks.NewPlanGate(hooks.PlanGateDeps{
			Mode:        l.PlanGateMode,
			RequirePlan: l.PlanGateRequirePlan,
			Plan:        l.PlanGatePlan,
			// Fallback index, used only when Records is nil.
			ActivePhase:          0,
			Surface:              l.PlanGateSurface,
			MaxSingleCardHandles: l.PlanGateMaxCardHandles,
			// Resolved ONCE at hook-build time, not per card. The population is
			// a SpiceDB lookup, and a card is rendered on the human's critical
			// path — re-resolving per render would put a network call between
			// the agent being denied and the approver seeing why. Membership
			// changing mid-session is the accepted cost, and it degrades to a
			// stale count rather than a wrong decision, because this string is
			// display only: the actual approve check runs against SpiceDB at
			// decision time.
			Approvers: l.planGateApprovers(context.Background()),
			// The slot maps mirror what approverCanDelegateSlots will actually do,
			// so the card and the audit record agree with the runtime decision
			// instead of describing it independently. PermissionTitles is display
			// only: the AgentClass controller's published titles, so a card prefers
			// them over the detokenized fallback.
			SlotStanding:    l.PlanGateSlotStanding,
			SlotPermissions: l.PlanGateSlotPermissions,
			// The same chains the binding uses to mint object ids, so the card
			// can tell when two slots of different types are one instance —
			// git names a repository by URL and gh by OWNER/NAME, and a human
			// deciding should see one target, not two.
			SlotValueTransforms: l.PlanGateSlotTransforms,
			PermissionTitles:    l.PlanGatePermissionTitles,
			ResourceDisplays:    l.PlanGateResourceDisplays,
			Resolve:             l.planGateHandleResolver(),
			// Records makes the active phase a FOLD of the log rather than a
			// value the gate holds. Without it the gate would sit on state a
			// restart loses and the agent's document could contradict.
			// CurrentPlan must be a live read: the frozen plan appears mid-session
			// when the agent declares phases, so a snapshot here would pin the gate
			// to the whole-surface fallback forever.
			CurrentPlan:    l.ActiveFrozenPlanNow,
			Records:        l.PlanGateRecords,
			DeriveApproval: l.PlanApprovalDeriver,
			Recorder:       planGateRecorder{l: l},
			Logger:         slog.Default(),
			// Advisory: used only to render a slot MOVE on the card. The MovePin
			// MUST_MATCH at decision time, not this read, is what makes the move
			// safe; a nil pinner just renders first-fills.
			SlotPinner: slotPinner,
		})}
	}})

	// trifecta activates on its OWN resolved mode, beside the plan gate at the
	// same order and for the same reason: hanging it off toolCalls.mode would
	// make it evaporate whenever an operator disabled an unrelated check.
	RegisterHook(HookFactory{Name: "trifecta", Order: hooks.OrderTrifecta, Build: func(l *Loop) []pipeline.Hook {
		if l.TrifectaMode == "" || l.TrifectaMode == "disabled" {
			return nil
		}
		return []pipeline.Hook{hooks.NewTrifecta(hooks.TrifectaDeps{
			Mode: l.TrifectaMode,
			StandingLegs: func(ctx context.Context) (trifecta.Legs, error) {
				return l.trifectaStandingLegs(ctx)
			},
			CallImpact: l.trifectaCallImpact,
			// The §2.8 closure denial set, read off THIS session's own status.
			//
			// The runner cannot derive it: answering "has anyone in my
			// delegation closure been denied" needs a List over the tree, and
			// the runner's Role grants no list on agentsessions — it may Get
			// its own object and nothing else. The operator stamps the fact
			// onto every member when a denial is recorded
			// (hold.ClosureDenialStamper), and the runner reads the one object
			// it is allowed to see.
			//
			// A live read rather than a start-time snapshot: a denial can land
			// in a sibling branch of the tree while this session is mid-turn,
			// and a gate consulting a stale copy would let exactly the call
			// through that the denial exists to stop.
			InClosureDenialSet: l.closureDenied,
			Logger:             slog.Default(),
		})}
	}})

	RegisterHook(HookFactory{Name: "revocation_guard", Order: hooks.OrderRevocation, Build: func(l *Loop) []pipeline.Hook {
		if l.RevokedOrigins == nil {
			return nil
		}
		return []pipeline.Hook{hooks.NewRevocationGuard(hooks.RevocationGuardDeps{
			Set:          l.RevokedOrigins,
			LookupOrigin: l.lookupOrigin(),
		})}
	}})

	RegisterHook(HookFactory{Name: "tool_call_authz", Order: hooks.OrderToolCallAuthz, Build: func(l *Loop) []pipeline.Hook {
		if !l.hookActive("tool_call_authz") {
			return nil
		}
		return []pipeline.Hook{hooks.NewToolCallAuthz(l.toolCallAuthzDeps())}
	}})

	RegisterHook(HookFactory{Name: "scope", Order: hooks.OrderScope, Build: func(l *Loop) []pipeline.Hook {
		if !l.hookActive("scope") {
			return nil
		}
		return []pipeline.Hook{hooks.NewScope(l.scopeDeps())}
	}})

	RegisterHook(HookFactory{Name: "info_leak_read", Order: hooks.OrderInfoLeakRead, Build: func(l *Loop) []pipeline.Hook {
		if !l.hookActive("info_leak_read") {
			return nil
		}
		return []pipeline.Hook{hooks.NewInfoLeakRead(l.infoLeakReadDeps())}
	}})

	RegisterHook(HookFactory{Name: "info_leak_audience", Order: hooks.OrderInfoLeakAudience, Build: func(l *Loop) []pipeline.Hook {
		if !l.hookActive("info_leak_audience") {
			return nil
		}
		// Capture the instance so leakage approval callbacks reach the same
		// session-scoped sets the PreResponse gate consults.
		audience := hooks.NewInfoLeakAudience(l.infoLeakAudienceDeps())
		l.infoLeakAudienceHook = audience
		return []pipeline.Hook{audience}
	}})

	RegisterHook(HookFactory{Name: "session_cleanup", Order: hooks.OrderSessionCleanup, Build: func(l *Loop) []pipeline.Hook {
		if !l.hookActive("session_cleanup") {
			return nil
		}
		return []pipeline.Hook{hooks.NewSessionCleanup(l.sessionCleanupDeps())}
	}})
}
