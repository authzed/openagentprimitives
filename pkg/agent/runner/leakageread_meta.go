// pkg/agent/runner/leakageread_meta.go
//
// The read-side info-leakage gate for the one meta tool that reaches resources
// outside the session.
//
// leakageGateApplies is `kind != tool.KindMeta`, and the comment above
// gatePipeline states the premise that exemption rests on: a meta tool with a
// trivial permission bypasses the pipeline because "no external SpiceDB-keyed
// resource enters context". Resource pools break exactly that premise for one
// tool. A memory search spans the session's own scope PLUS every pool the
// session reached through a slot grant, so one result carries data whose
// audience is view_memory on a resource the session does not own — and the two
// hooks that record and gate that (info_leak_read's per-pool taint and per-pool
// mint, info_leak_audience's same-turn egress gate) never saw the call, because
// the model's own tool_use takes the ungated meta branch at loop_dispatch.go.
// The exemption is still right for every meta tool that does not do this.
//
// This file closes that gap the way toolguard_meta.go closed the toolguard one:
// by running the hooks around the same ungated execute, NOT by making
// gatePipeline true for the tool. Routing memory search through the contained
// pipeline would subject every recall to the plan gate (which governs any meta
// tool that flows through — that is why delegate was routed there), to
// Scope.CheckScope's narrowing and tool-deny half, and to the pre-dispatch
// authz hooks. In enforcing plan-gate mode an agent recalling memory outside
// its plan would be refused. That is a behavior change to every agent, bought
// for a gate that only needs the RESULT: there is nothing to authorize at
// dispatch, since the memory door authorized the request per scope server-side
// and per-entry authorization ran inside the searcher before the result came
// back.
//
// What this path DOES keep is the executor and the runner host, and that is not
// the "approval plumbing" toolguard_meta declines. That one is the PRE-dispatch
// authorization ask, which cannot arise here because no Pre hook runs. An
// audience ask at Post is the audience hook's OWN verdict machinery: its
// leaked-to decision comes back as Decision{Approval: ask} carrying the ZERO
// (Allow) verdict, so a path that evaluated the hooks itself and dropped the ask
// would turn a refusal into a silent allow — the gate inverted, not skipped.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// readSidePostHooks names the PostToolCall hooks this pass runs. Both are
// read-side: info_leak_read records what the result brought into context,
// info_leak_audience gates where it may go this turn. Every other PostToolCall
// hook is deliberately absent — toolguard already ran around this execute
// (toolguard_meta.go), and scope's Post leg is the narrowing the ruling above
// keeps off this tool.
var readSidePostHooks = map[string]bool{
	"info_leak_read":     true,
	"info_leak_audience": true,
}

// metaResultReadsApplies reports whether the read-side pass must wrap this
// ungated meta call.
//
// It asks a resolved DECLARATION rather than testing the tool's name, for the
// reason metaToolGuardApplies gives for deriving from the resolved rule: a
// predicate that re-states what a declaration says is free to disagree with it.
// The qualifying shape is a declaration reporting resources derived from the
// RESULT, which is the only shape whose audience cannot be known before the call
// returns; a single-resource declaration belongs to a gated kind and reaches the
// hooks by the ordinary path.
//
// It asks the BUILT-IN declaration specifically, and not the full
// infoLeakReadDeps().LookupReads chain the hook itself will ask, for two
// reasons that point the same way:
//
//   - Only a built-in declaration can report plural resources at all.
//     ToolReadsDecl.ResultResources is deliberately not CRD-settable — reading a
//     result into the resources it came from needs that result's FORMAT, and a
//     spec author who could name plural arbitrary-typed resources would be
//     deciding the audience of their own tool's output. The CRD half of that
//     chain therefore cannot qualify a call, so asking it decides nothing.
//     TestMetaReadGate_OnlyABuiltInDeclarationCanCarryResultResources pins that;
//     an edit that starts sourcing ResultResources from a mapping fails there,
//     which is the signal to revisit this predicate.
//   - Asking it is NOT free. The runner wires LookupToolMapping
//     unconditionally, and it does a live client.Get per MCPServer ref and per
//     SidecarToolbox ref on an UNCACHED client. Routing the predicate through
//     the full chain charged every ungated meta call — respond_to_user,
//     update_status, every one of them — that round trip, in every leakage mode
//     including disabled, where the whole feature is inert.
//
// Today exactly one tool qualifies, so this and a name test pick out the same
// call; see the report's mutation table for what that does and does not mean.
// The difference is which one stays true when a second built-in plural
// declaration is added: this one follows it to the model's own dispatch, a name
// list leaves it unreachable there — which is the bug this whole file exists to
// fix, repeated.
func (l *Loop) metaResultReadsApplies(toolName string) bool {
	decl := l.memoryPoolReadsDecl(toolName)
	return decl != nil && decl.ResultResources != nil
}

// readSidePostRegistry returns a registry holding only the read-side
// PostToolCall hooks of reg.
//
// It selects from an ALREADY-BUILT registry rather than building its own hooks,
// so activation (hooks.ActiveHooks — a disabled leakage mode registers neither)
// is decided in exactly one place, and so the audience hook here is the same
// instance l.infoLeakAudienceHook hands the approval callbacks. Two instances
// would not be a correctness bug today — the approve/deny set is durable, not
// in-process — but building a second one re-runs the factory's capture of that
// field, which dispatch goroutines read concurrently.
//
// reg.Hooks is already ascending by the code-owned order, so re-registering by
// index preserves the relative order without re-deriving the numbers. The
// audience hook declares three Points and so lands in this registry at all
// three; only PostToolCall is ever run from it.
func readSidePostRegistry(reg *pipeline.Registry) *pipeline.Registry {
	out := pipeline.NewRegistry()
	if reg == nil {
		return out
	}
	for i, h := range reg.Hooks(pipeline.PostToolCall) {
		if readSidePostHooks[h.Name()] {
			out.Register(h, i)
		}
	}
	return out
}

// readSidePostRunner returns the executor for the read-side pass, or nil when
// this session registers neither read-side hook (a disabled leakage mode, or a
// test that injected its own executor). Declared as the pipelineRunner
// INTERFACE and assigned only once there is a real executor: assigning a
// typed-nil *pipeline.Executor would produce a non-nil interface that panics on
// the first Run.
//
// Forcing l.executor() first is load-bearing, not incidental. That build is
// what captures l.infoLeakAudienceHook, and doing it inside this Once — whose
// body blocks on the executor's own Once — is what orders the two builds
// against each other instead of racing a field dispatch goroutines read.
func (l *Loop) readSidePostRunner() pipelineRunner {
	l.readSideOnce.Do(func() {
		_ = l.executor()
		reg := readSidePostRegistry(l.pipelineReg)
		if len(reg.Hooks(pipeline.PostToolCall)) == 0 {
			// Disabled leakage mode registers neither hook, and running nothing
			// IS the configured behavior there. Any OTHER emptiness means a call
			// that declares pool reads will go ungated, which an operator must
			// not have to infer from missing records. Inside the Once, so it is
			// said at most once per session.
			if l.LeakageConfig.ResolvedMode() != "disabled" {
				slog.Default().Info("read-side PostToolCall pass has no hooks; pool reads on ungated meta tools are not recorded or gated",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"mode", l.LeakageConfig.ResolvedMode())
			}
			return
		}
		l.readSidePostExec = pipeline.NewExecutor(reg)
	})
	return l.readSidePostExec
}

// readSidePostForMetaTool runs the read-side PostToolCall hooks over a finished
// ungated meta call, returning the same (Result, containedOutcome) pair
// executeToolContained returns so the dispatcher's deny/halt bookkeeping is
// identical on both branches.
//
// It is a pass-through for everything that does not qualify, and the test for
// qualifying is a name comparison against the built-in declaration — no CRD
// read, no I/O — so the ungated meta path every session walks constantly
// (respond_to_user, update_status, agent_work_complete) costs what it did.
//
// oc is the outcome the call already has. A non-containRanOK phase returns
// untouched: the executor short-circuits its remaining Post hooks on a Deny or
// Halt, so a toolguard refusal must end the Post leg here too — running the read
// hooks over a result toolguard already replaced would attribute the refusal
// text rather than the data, and refuse it again for a reason that has nothing
// to do with what was read.
func (l *Loop) readSidePostForMetaTool(
	ctx context.Context,
	sess *tool.SessionContext,
	name string,
	args json.RawMessage,
	useID string,
	res tool.Result,
	oc containedOutcome,
) (tool.Result, containedOutcome) {
	if oc.Phase != containRanOK || !l.metaResultReadsApplies(name) {
		return res, oc
	}
	// A user-cancelled call produced no tool output to attribute, and these
	// gates on the already-cancelled ctx would fail closed and flip the
	// synthesized cancellation notice into an error — the same reason
	// executeToolContained skips PostToolCall for one.
	if context.Cause(ctx) == errInterruptedByUser {
		return res, oc
	}
	runner := l.readSidePostRunner()
	if runner == nil {
		return res, oc
	}

	host := newRunnerHost(l, hostSession{Namespace: sess.Namespace, Name: sess.Name, Class: l.AgentName})
	in := pipeline.Input{
		Session: pipeline.SessionRef{
			Namespace: sess.Namespace,
			Name:      sess.Name,
			Class:     l.AgentName,
		},
		// The LLM loop's principal, as the gated branch passes it: the audience
		// hook addresses its logging-mode notice to whoever made the read.
		Requester: l.authSubject,
		Subjects:  l.authSubjects,
		Tool: &pipeline.ToolCallInfo{
			Name: name,
			// The bytes the tool was actually called with. The gated path passes
			// its stripArgTags'd copy because it strips before executing; this
			// path does not strip at all, so the same copy the tool saw is the
			// honest answer. No plural declaration reads Args anyway — a
			// resource named by an ARG is knowable before the call and takes the
			// single-resource path, which this pass never qualifies.
			Args:    args,
			UseID:   useID,
			Result:  res.Content,
			IsError: res.IsError,
		},
	}

	out, err := runner.Run(ctx, pipeline.PostToolCall, in, host)
	if err != nil {
		// A host-primitive failure the executor could not turn into a verdict;
		// its Verdict is the zero-value Allow. Fail CLOSED, as the gated path
		// does: the result is withheld rather than handed over unvetted.
		slog.Default().Info("read-side PostToolCall pass errored; withholding tool result (fail-closed)",
			"session", sess.Namespace+"/"+sess.Name, "tool", name, "err", err.Error())
		// The gated path's wording, verbatim: an operator greps this string and
		// a bundle may assert on it, and a fail-closed withhold should not read
		// differently for having taken the ungated branch.
		return tool.Result{
				Content: fmt.Sprintf("post-tool-call gate failed for %q: %v (result withheld)", name, err),
				IsError: true,
			},
			containedOutcome{Phase: containPostErr}
	}
	switch out.Verdict {
	case pipeline.Deny:
		l.reportDefinitionError(sess, name, out)
		return tool.Result{Content: out.Reason, IsError: true},
			containedOutcome{Phase: containPostDeny, Reason: out.Reason, Definition: out.Definition}
	case pipeline.Halt:
		// host.Halt already wrote the terminal Failed status inside the
		// executor; the caller records the halt so the loop stops.
		return tool.Result{Content: out.Reason, IsError: true},
			containedOutcome{Phase: containPostHalt, Reason: out.Reason}
	}
	return res, oc
}
