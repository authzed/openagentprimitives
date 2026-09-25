// Package hooks holds the concrete authz lifecycle hooks for the runner.
// Each hook is constructed with its own dependency struct (DI) and registered
// into a per-Loop pipeline.Registry via buildPipeline in pkg/agent/runner.
package hooks

// Code-owned per-point hook order (spec §6). Lower runs first. The same
// constant is reused at both points a hook attaches to (Scope is third at
// PreToolCall and first at PostToolCall — these ints make that ordering
// hold because no PreToolCall hook shares a PostToolCall point).
//
// Operators do NOT reorder these; reordering security gates is a footgun.
const (
	OrderMcpTrust = 10
	// OrderRevocation slots the revoked-origin guard after MCP trust (10) but
	// before the toolguard breaker/rate-limit (15): a revoked origin should be
	// denied before consuming a rate slot or a breaker probe.
	OrderRevocation = 12
	// OrderToolGuard slots the toolguard breaker/rate-limit admission between
	// MCP trust (a distrusted server should fail as a trust error) and
	// ToolCallAuthz (a breaker-open denial must not burn a SpiceDB check or
	// approval ask). Its PostToolCall recorder reuses the same int and runs
	// before Scope (30) so it records the raw Execute outcome, not the
	// post-gated one.
	OrderToolGuard = 15
	// OrderContentGuard slots content-guard inspectors after the toolguard
	// circuit-breaker (15) but before ToolCallAuthz (20): content that
	// would trip a breaker is already denied; content inspection runs on
	// surviving calls before an approval ask is raised.
	OrderContentGuard = 18
	// OrderPlanGate slots the plan gate between content inspection (18) and
	// ToolCallAuthz (20). After content-guard, so content that would be blocked
	// outright never reaches the gate and never lands in the plan-gate dataset
	// as a legitimate call. Before ToolCallAuthz, so an out-of-ceiling call is
	// refused before it burns a SpiceDB check or raises an approval ask — the
	// same reasoning that puts the toolguard breaker ahead of ToolCallAuthz.
	//
	// The gate enforces on its OWN mode, deliberately not behind ToolCallAuthz:
	// wiring it behind that gate would make it evaporate silently whenever
	// toolCalls.mode is permissive or disabled.
	OrderPlanGate = 19
	// OrderTrifecta sits beside the plan gate, and for the same two reasons:
	// after content-guard so blocked content never lands in its dataset as a
	// legitimate call, and before ToolCallAuthz so an out-of-closure call is
	// refused before it burns a SpiceDB check or raises an approval ask.
	//
	// Its OWN mode, and this matters more here than anywhere. ToolCallAuthz.Eval
	// RETURNS BEFORE consulting ForceApproval when its mode is "disabled"
	// (toolcallauthz.go), so a trifecta routed through that gate would vanish
	// the moment an operator turned off a DIFFERENT check. An operator
	// disabling a permission check has not thereby decided that untrusted
	// content may drive a write.
	//
	// Ordering alone does not guarantee that independence — it is a fact about
	// today's wiring. TestTrifectaEnforcesWithToolCallAuthzDisabled is what
	// makes it a fact about the requirement.
	OrderTrifecta      = 19
	OrderToolCallAuthz = 20
	OrderScope         = 30
	// OrderInfoLeakRead and OrderInfoLeakAudience are the read-side PostToolCall
	// pair, and they run in one more place than the others: the runner runs
	// exactly these two around the UNGATED meta execute, so a memory search that
	// spans a resource pool is tagged and gated even though its Stateless
	// permission keeps it out of the containment pipeline. The set is named
	// there, not derived from these numbers — a new hook ordered after them is
	// NOT automatically on that path. If you add a PostToolCall hook, decide
	// whether it belongs, in pkg/agent/runner/leakageread_meta.go
	// (readSidePostHooks).
	OrderInfoLeakRead     = 40
	OrderInfoLeakAudience = 50

	// OrderIdentityChoiceGate is the SessionStart runner hook that resolves the
	// session identity for identityMode=ask|dynamic. It runs BEFORE ColdStartScope
	// (identity must resolve before scope review): a userPassthrough handoff halts
	// SessionStart before any scope work, and an agent choice must be settled
	// before ColdStartScope narrows the agent's scope. Only registered when the
	// Loop has IdentityGatePending set (see coldStartSessionStartExecutor).
	OrderIdentityChoiceGate = 5
	// OrderColdStartScope is the SessionStart hook order. Two different hosts
	// serve SessionStart — the runner-side ColdStartScope (publish + wait + map)
	// and the authzd-side ColdStartScope (extract + classify + apply). They live
	// in SEPARATE per-host registries (the runner builds its own; authzd builds
	// its own), so this single order int only needs to be internally consistent
	// within each host, not coordinated across them. Should EntityBind ever do
	// real work at SessionStart, it would order relative to ColdStartScope
	// within authzd's registry.
	OrderColdStartScope = 10
	// OrderSessionCleanup is the sole hook at SessionEnd today. Future SessionEnd
	// hooks (durable teardown migrating out of the operator finalizer) slot
	// relative to it.
	OrderSessionCleanup = 10

	// InboundTurn is served by hooks in TWO different hosts that never share a
	// registry: channelsd runs the Interact gate (cheap SpiceDB #interact check,
	// drops the inbound before the runner sees it); authzd runs EntityBind
	// (the untrusted-text entity extractor, advisory). Because the two hosts build
	// independent per-host registries, OrderInteract and OrderEntityBind only need
	// to be internally consistent per host — they are never compared against each
	// other at runtime. Each is the sole InboundTurn hook in its own host today.
	OrderInteract   = 10
	OrderEntityBind = 10

	// The four metaagent control-plane hooks. Each is the SOLE hook at its
	// point (the authzd worker builds a one-hook registry per stage and runs
	// Executor.Run(Received) → Run(Extract) → Run(Decide) → Run(Apply) in
	// sequence). The order int only needs to be internally consistent within
	// its own one-hook registry, so they all share 10.
	OrderMetaagentReceived = 10
	OrderMetaagentExtract  = 10
	OrderMetaagentDecide   = 10
	OrderMetaagentApply    = 10

	// OrderSessionFork is the sole SessionFork hook order. The operator builds a
	// one-hook registry for the fork gate, so this only needs internal consistency.
	OrderSessionFork = 10
)
