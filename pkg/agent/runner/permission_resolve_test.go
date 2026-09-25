package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// J3's fixture: five fabricated tools whose Permission()/PermissionVariants()/
// ServerReadOnlyHint() are independently controllable, spanning every axis
// couldEverBeReadonly (the static ceiling) and the per-call gate in
// handleAppToolCallReq (the runtime predicate) must agree — and, for
// demo_variant_read, deliberately disagree on a PER-ARGS basis, which is the
// whole point of resolving variants at the gate instead of reading only the
// static fallback.
var (
	demoAlwaysRead = &fakeAppTool{
		name: "demo_always_read", perm: authz.Permission{StateImpact: authz.Readonly}, roHint: true,
	}
	demoVariantRead = &fakeAppTool{
		name: "demo_variant_read", perm: authz.Permission{StateImpact: authz.Passthrough}, roHint: true,
		variants: []authz.PermissionVariant{
			{When: `args.kind == "read"`, Check: authz.Permission{StateImpact: authz.Readonly}},
			{When: `args.kind == "write"`, Check: authz.Permission{StateImpact: authz.Readwrite}},
		},
	}
	demoNoHint = &fakeAppTool{
		name: "demo_no_hint", perm: authz.Permission{StateImpact: authz.Readonly}, roHint: false,
	}
	demoAllWrite = &fakeAppTool{
		name: "demo_all_write", perm: authz.Permission{StateImpact: authz.Passthrough}, roHint: true,
		variants: []authz.PermissionVariant{
			{When: `args.kind == "x"`, Check: authz.Permission{StateImpact: authz.Readwrite}},
		},
	}
	demoStateless = &fakeAppTool{
		name: "demo_stateless", perm: authz.Permission{StateImpact: authz.Stateless}, roHint: true,
	}
)

// runGateForTest drives the REAL handleAppToolCallReq (via the exported
// HandleUIDataBinding shell) for tool ft with the given args, and reports
// whether the call auto-ran: OK/error status means it ran synchronously;
// anything else (denied, requires_approval, not_found, rate_limited, error
// from a malformed request) means it did not. Used instead of a
// re-implementation of the readonly predicate, so J3's spanning test proves
// the REAL gate agrees with couldEverBeReadonly rather than proving a copy of
// the predicate agrees with itself.
func runGateForTest(t *testing.T, ft *fakeAppTool, args string) (autoRan bool, status string) {
	t.Helper()
	const ns, name = "demo-ns", "demo-session"
	l := &Loop{
		SessionKey: memory.NamespacedName{Namespace: ns, Name: name},
		AppTools:   map[string]tool.Tool{ft.name: ft},
	}
	reg := pipeline.NewRegistry()
	loopWithInjectedExecutor(t, l, reg)
	ia := &fakeInteract{allow: true}
	execCtx := memory.WithSystemApproval(context.Background(), "test")

	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindUIDataBinding,
		channelevents.AppToolCallRequest{ToolName: ft.name, Args: json.RawMessage(args), Requester: "user:viewer", RequestID: "req-1"})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)

	resp := l.HandleUIDataBinding(execCtx, ia, ns, name, b)
	autoRan = resp.Status == channelevents.AppToolCallStatusOK
	return autoRan, string(resp.Status)
}

// TestReadonlyCeilingContainsEveryAutoRunnableCall is J3's spanning test: it
// proves couldEverBeReadonly (the static, per-document ceiling
// UIToolOptions/the AgentUI validator use) and the REAL per-call gate in
// handleAppToolCallReq (driven here through HandleUIDataBinding, since a data
// binding is exactly the surface the readonly gate restricts) agree in BOTH
// directions:
//
//   - containment: every (tool, args) the gate ever auto-runs is a tool the
//     ceiling admits — this is what stops a widened runtime predicate from
//     silently outrunning the document-time validator.
//   - completeness: every tool the ceiling admits has SOME args in the
//     fixture that the gate actually auto-runs — this is what stops the
//     opposite "fix": widening the ceiling to admit a tool the gate can
//     never actually run for anything.
func TestReadonlyCeilingContainsEveryAutoRunnableCall(t *testing.T) {
	fixture := []*fakeAppTool{demoAlwaysRead, demoVariantRead, demoNoHint, demoAllWrite, demoStateless}
	argSets := []string{`{}`, `{"kind":"read"}`, `{"kind":"write"}`, `{"kind":"x"}`, `{"kind":"other"}`}

	admittedButNeverRan := map[string]bool{}
	for _, ft := range fixture {
		admitted := couldEverBeReadonly(ft)
		ranForSomeArgs := false
		for _, args := range argSets {
			autoRan, _ := runGateForTest(t, ft, args)
			if autoRan {
				ranForSomeArgs = true
				assert.True(t, admitted, "containment: %s auto-ran for %s but couldEverBeReadonly denies it", ft.name, args)
			}
		}
		if admitted && !ranForSomeArgs {
			admittedButNeverRan[ft.name] = true
		}
	}
	assert.Empty(t, admittedButNeverRan,
		"completeness: a tool couldEverBeReadonly admits must auto-run for SOME args in the fixture — otherwise the ceiling widened past what the gate can ever grant")
}

func TestDataBindingToAVariantReadonlyToolIsAllowed(t *testing.T) {
	autoRan, status := runGateForTest(t, demoVariantRead, `{"kind":"read"}`)
	assert.True(t, autoRan, "status was %s", status)
}

func TestDataBindingToAVariantWriteToolIsDenied(t *testing.T) {
	_, status := runGateForTest(t, demoVariantRead, `{"kind":"write"}`)
	assert.Equal(t, string(channelevents.AppToolCallStatusDenied), status)
}

// TestDataBindingToAVariantFallthroughToolIsDenied is the row the brief's own
// Step-3 mutation ("add authz.Passthrough to the auto-runnable set") needs to
// exist BEFORE that mutation is run: demo_variant_read's variants only cover
// "read"/"write", so {"kind":"other"} matches neither and falls through to
// the tool's own Passthrough fallback. TestDataBindingToAVariantWriteToolIsDenied
// alone would NOT catch a Passthrough-admitting mutation, because its "write"
// row resolves to a variant of Readwrite, not the fallback.
func TestDataBindingToAVariantFallthroughToolIsDenied(t *testing.T) {
	_, status := runGateForTest(t, demoVariantRead, `{"kind":"other"}`)
	assert.Equal(t, string(channelevents.AppToolCallStatusDenied), status)
}

func TestDataBindingToAStatelessToolIsStillDenied(t *testing.T) {
	_, status := runGateForTest(t, demoStateless, `{}`)
	assert.Equal(t, string(channelevents.AppToolCallStatusDenied), status,
		"the auto-runnable StateImpact set must stay exactly {Readonly} — Stateless does not widen it")
}

// TestAnUnevaluableVariantDenies covers both routes to an unknown
// authorization posture: a variant whose When cannot compile, and a call
// whose req.Args cannot be parsed into an args map. Both must DENY — not
// merely fail to auto-run, which a fall-back-to-Permission() implementation
// would also produce for a fallback that isn't Readonly, and so cannot tell
// apart from a correct deny-on-error implementation.
func TestAnUnevaluableVariantDenies(t *testing.T) {
	t.Run("uncompilable variant When", func(t *testing.T) {
		// The static fallback is deliberately Readonly (not Passthrough): a
		// fail-OPEN implementation that silently falls back to t.Permission()
		// on a resolve error would auto-run this tool, since its FALLBACK
		// alone satisfies the readonly predicate. A Passthrough fallback would
		// stay denied under fail-open too (Passthrough != Readonly), which
		// would make this row blind to exactly the bug it exists to catch.
		ft := &fakeAppTool{
			name: "demo_bad_cel", perm: authz.Permission{StateImpact: authz.Readonly}, roHint: true,
			variants: []authz.PermissionVariant{{When: `this is not valid CEL %%%`, Check: authz.Permission{StateImpact: authz.Readonly}}},
		}
		_, status := runGateForTest(t, ft, `{"kind":"read"}`)
		assert.Equal(t, string(channelevents.AppToolCallStatusDenied), status)
	})

	t.Run("malformed req.Args (a JSON string, not an object)", func(t *testing.T) {
		// A raw JSON string is itself well-formed JSON — the envelope this
		// helper builds still marshals fine — but it cannot unmarshal into
		// map[string]any, which is the failure mode actually reachable over
		// the wire (an envelope that parsed already guarantees req.Args is
		// SYNTACTICALLY valid JSON; a type mismatch is the live case).
		_, status := runGateForTest(t, demoVariantRead, `"not-an-object"`)
		assert.Equal(t, string(channelevents.AppToolCallStatusDenied), status)
	})
}

func TestResolvePermissionForArgsMatchesTheMigratedSites(t *testing.T) {
	t.Run("a matching variant wins", func(t *testing.T) {
		got, err := ResolvePermissionForArgs(demoVariantRead, map[string]any{"kind": "read"})
		require.NoError(t, err)
		assert.Equal(t, authz.Readonly, got.StateImpact)
	})
	t.Run("no matching variant falls back to Permission()", func(t *testing.T) {
		got, err := ResolvePermissionForArgs(demoVariantRead, map[string]any{"kind": "other"})
		require.NoError(t, err)
		assert.Equal(t, authz.Passthrough, got.StateImpact)
	})
	t.Run("an unevaluable variant returns the error, not the fallback", func(t *testing.T) {
		ft := &fakeAppTool{
			perm:     authz.Permission{StateImpact: authz.Readonly},
			variants: []authz.PermissionVariant{{When: `this is not valid CEL %%%`, Check: authz.Permission{StateImpact: authz.Readwrite}}},
		}
		_, err := ResolvePermissionForArgs(ft, map[string]any{})
		require.Error(t, err)
	})
	t.Run("no variants at all: the static fallback, unconditionally", func(t *testing.T) {
		got, err := ResolvePermissionForArgs(demoAlwaysRead, map[string]any{"anything": "goes"})
		require.NoError(t, err)
		assert.Equal(t, authz.Readonly, got.StateImpact)
	})
}
