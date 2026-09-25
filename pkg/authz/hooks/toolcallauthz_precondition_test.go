package hooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The three sentences this file discriminates on. Each is unique in the repo,
// which is the point: a Deny alone proves nothing here — the same call is
// denied whether the precondition explained it, an ordinary permission check
// refused it, or the args failed to parse — so every row asserts on the exact
// words only ITS path can produce, and on the absence of the words the others
// would.
const (
	// preHint is what an author writes for a fact nobody has recorded yet: a
	// hint naming the call that would establish it.
	preHint = "hooks-test hint: nothing has observed whether this commit came from a fork; call the inspect tool first"
	// preRefusal is what an author writes for a predicate the recorded facts
	// decided against.
	preRefusal = "hooks-test refusal: this commit's head lives on a fork, so checking it out would run untrusted code"
	// checkerDeny is the message the CHECKER returns. A gated row must NOT be
	// explained by it: the SpiceDB sentence is true and useless, pointing the
	// agent at a permission grant nobody is going to write.
	checkerDeny = "hooks-test permission denied on git_commit"
	// unevaluatable is the clause every "could not compute a verdict" branch
	// shares, and the ONLY thing they share: each branch names its own cause
	// after it, because a missing argument is the agent's to fix and a dead
	// fact store is not.
	unevaluatable = "the gate could not be evaluated"
)

const (
	gatedType = "git_commit"
	gatedID   = "ba03f5969a"
	forkFact  = "is_cross_repository"
)

// seedFact records one observed fact about the commit under test, through the
// REAL accessor the runner writes with. A fake "here are the facts" reader
// would pass whether or not the explanation ever managed to look one up, which
// is exactly the class of green-for-the-wrong-reason this plan keeps finding.
func seedFact(t *testing.T, mem memory.Memory, memScope memory.Scope, id string, isFork bool) {
	t.Helper()
	require.NoError(t, observedfact.Record(memory.WithSystemApproval(context.Background(), "test"),
		mem, memScope, factcontent.Observation{
			Subjects: []factcontent.Subject{{ResourceType: gatedType, ResourceID: id}},
			Facts:    map[string]any{forkFact: isFork},
			Source:   factcontent.Source{ToolName: "inspect"},
		}))
}

// gatedSlotSpec is the class's slot declaration: gated on the fork fact, with
// both authored messages attached.
//
// A composite literal of authz.BoundEntitySpec, which slotspec's AST guard
// permits only in a test — a test cannot ship an ungated slot to a cluster, and
// pinning ONE field's behaviour requires writing that field alone.
func gatedSlotSpec(t *testing.T) authz.BoundEntitySpec {
	t.Helper()
	c, err := precondition.Compile(`facts.observed.` + forkFact + ` == false`)
	require.NoError(t, err, "the predicate under test must itself compile")
	return authz.BoundEntitySpec{
		ResourceType: gatedType,
		Permission:   "read",
		FillFrom:     []string{"observed"},
		Requires: []precondition.Rule{{
			Compiled:         c,
			UndeterminedHint: preHint,
			RefusalMessage:   preRefusal,
		}},
	}
}

// ungatedSlotSpec is the SAME slot with no requires[] — every AgentClass that
// predates preconditions. It is the counterfactual the "unchanged behaviour"
// rows rest on: same type, same recorded fact, same denial, and the only
// difference is that no gate is declared.
func ungatedSlotSpec() authz.BoundEntitySpec {
	return authz.BoundEntitySpec{ResourceType: gatedType, Permission: "read", FillFrom: []string{"observed"}}
}

// preconditionHook builds the hook under test with the REAL recomputation wired
// over a REAL in-memory store, exactly as the runner wires it.
//
// impact is a parameter rather than a fixed Readonly because needsApproval
// branches on it: External is the one state impact that pauses for a human
// unconditionally, so it is the only one where a precondition can REMOVE
// somebody's option rather than only change the sentence the agent receives.
func preconditionHook(t *testing.T, mode string, impact authz.StateImpact, mem memory.Memory, memScope memory.Scope, specs []authz.BoundEntitySpec, check *authz.PermissionCheck) *hooks.ToolCallAuthz {
	t.Helper()
	return hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              mode,
		Checker:           &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: checkerDeny}},
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: impact, Check: check}),
		// BOTH builders wired on every row. simpleApprovalAskBuilder is the
		// ordinary tool_call card (an ungated denial still escalates to it);
		// simpleWaiverAskBuilder is the precondition_waiver card a REFUSED verdict
		// now raises. Wiring the waiver builder on every row — including the
		// Undetermined/Unevaluatable rows that must NOT raise it — is what lets
		// those rows prove the split fails closed: a mis-split would come back with
		// a non-nil Approval instead of a quiet Deny.
		BuildApprovalAsk: simpleApprovalAskBuilder,
		BuildWaiverAsk:   simpleWaiverAskBuilder,
		ExplainPrecondition: func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
			return authz.ExplainPreconditionDenial(ctx, mem, memScope, specs, *perm.Check, args)
		},
	})
}

func evalCall(h *hooks.ToolCallAuthz, id string) pipeline.Decision {
	return h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name: "inspect",
			Args: json.RawMessage(`{"sha":"` + id + `"}`),
		},
	})
}

// TestToolCallAuthz_PreconditionExplainsTheEmptySlot is the task's table:
// verdict × enforcement mode, plus the ungated regression guard in both modes.
//
// # The two permissive rows are the entire reason EnforceAlways is stamped
//
// Under toolCalls.mode: permissive the hook returns Decision{} for a denied
// readonly BEFORE approval is even considered — the deny is logged and the call
// PROCEEDS. Remove the EnforceAlways stamp and that permissive readonly bypass
// fires before either precondition gate resolves: the undetermined row loses its
// Deny and the refused row loses its waiver card, both landing on a bare
// Verdict=Allow. A precondition would then be disableable by a setting about
// something else, which is the control this task exists to make undisableable.
//
// # Every row is discriminated by words only its own path emits
//
// A Deny is not evidence here. The same call is denied by an ordinary
// permission check, by unparseable args, and by a refused precondition, and
// this plan has now found nine tests green for a reason unrelated to their
// name — one of them because an unrelated rule produced the same rejection
// REASON. So each gated row requires the AUTHOR's sentence and refuses the
// CHECKER's, and each ungated row requires the checker's and refuses both
// authored ones.
func TestToolCallAuthz_PreconditionExplainsTheEmptySlot(t *testing.T) {
	cases := []struct {
		name string
		mode string
		// gated declares the slot's requires[]; false is the every-existing-class
		// shape whose behaviour must be untouched.
		gated bool
		// impact is the call's state impact; empty means Readonly. External is
		// the row where the verdict CHANGES which card is raised: needsApproval
		// consults res.Precondition BEFORE the External arm, so a gated External
		// Refused routes to the WAIVER card (not the ordinary tool_call one an
		// ungated External denial raises) — approving a tool_call card there would
		// waive the gate silently through the grant path.
		impact authz.StateImpact
		// seed writes the facts recorded so far. A recorded true is Refused;
		// nothing recorded about THIS commit is Undetermined, which is spelled
		// by seeding a DIFFERENT commit so the row cannot pass merely because
		// the store is empty.
		seed func(t *testing.T, mem memory.Memory, memScope memory.Scope)
		// wantReason is the sentence the agent must be handed, or "" when the
		// call must not be denied at all.
		wantReason string
		// wantApproval is whether an ordinary tool_call card must be raised (an
		// ungated denial escalating to a human).
		wantApproval bool
		// wantWaiver, when non-empty, is the RefusalMessage substring the
		// precondition_waiver card must carry — the Refused verdict's one appeal.
		wantWaiver string
		why        string
	}{
		{
			name:  "undetermined + enforcing: Deny carrying the author's hint, and no human is asked",
			mode:  "enforcing",
			gated: true,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				// A fact about a DIFFERENT commit of the same type. The store is
				// non-empty and the read succeeds; the gated commit simply has
				// no observation, which is the undetermined case and must not be
				// confused with "nothing works".
				seedFact(t, mem, memScope, "0000000000", false)
			},
			wantReason: preHint,
			why:        "a fact nobody has observed is answerable only by the agent, so the hint goes back as the tool result",
		},
		{
			name:  "undetermined + permissive: STILL Deny — a precondition is not disableable by toolCalls.mode",
			mode:  "permissive",
			gated: true,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				seedFact(t, mem, memScope, "0000000000", false)
			},
			wantReason: preHint,
			why:        "without EnforceAlways the permissive branch returns Decision{} and the call proceeds",
		},
		{
			name:  "refused + enforcing: a precondition_waiver card carrying the author's refusal",
			mode:  "enforcing",
			gated: true,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				seedFact(t, mem, memScope, gatedID, true)
			},
			wantWaiver: preRefusal,
			why:        "a Refused verdict is the one precondition a human may waive; the card explains what consenting to it means",
		},
		{
			name:  "refused + permissive: STILL a waiver card — a precondition is not disableable by toolCalls.mode",
			mode:  "permissive",
			gated: true,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				seedFact(t, mem, memScope, gatedID, true)
			},
			wantWaiver: preRefusal,
			why:        "without EnforceAlways the permissive readonly bypass fires, the fork is cloned, and no card is ever raised",
		},
		{
			name:  "no precondition declared + enforcing: unchanged — the denial still routes to a human",
			mode:  "enforcing",
			gated: false,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				// The REFUSING fact, deliberately. If the recomputation ran on a
				// slot that declares no gate, this row would deny with the
				// authored refusal instead of asking a human.
				seedFact(t, mem, memScope, gatedID, true)
			},
			wantApproval: true,
			why:          "every class that predates preconditions must keep escalating an ordinary denial",
		},
		{
			name:  "no precondition declared + permissive: unchanged — the denial is logged and the call proceeds",
			mode:  "permissive",
			gated: false,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				seedFact(t, mem, memScope, gatedID, true)
			},
			why: "permissive's existing readonly bypass must survive; only a precondition overrides it",
		},
		{
			name:   "refused + EXTERNAL: a precondition_waiver card, NOT the ordinary tool_call one",
			mode:   "enforcing",
			gated:  true,
			impact: authz.External,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				seedFact(t, mem, memScope, gatedID, true)
			},
			wantWaiver: preRefusal,
			why:        "the Refused verdict routes to the WAIVER builder before the External arm is reached — approving a tool_call card would waive the gate silently through the grant path, so the card must be the waiver that explains the consent",
		},
		{
			name:   "no precondition declared + EXTERNAL: unchanged — the card still goes to a human",
			mode:   "enforcing",
			gated:  false,
			impact: authz.External,
			seed: func(t *testing.T, mem memory.Memory, memScope memory.Scope) {
				// The REFUSING fact again: a recomputation that ran on an
				// ungated slot would suppress this card instead of raising it,
				// which is the loss no Deny/Allow verdict makes visible.
				seedFact(t, mem, memScope, gatedID, true)
			},
			wantApproval: true,
			why:          "External is the one impact that pauses unconditionally, so a mis-scoped precondition removes a route the agent cannot take itself",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			memScope := memory.Scope{Kind: "session", ID: "ns/" + t.Name()}
			tc.seed(t, mem, memScope)

			spec := ungatedSlotSpec()
			if tc.gated {
				spec = gatedSlotSpec(t)
			}
			impact := tc.impact
			if impact == "" {
				impact = authz.Readonly
			}
			h := preconditionHook(t, tc.mode, impact, mem, memScope, []authz.BoundEntitySpec{spec},
				&authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"})

			dec := evalCall(h, gatedID)

			// A Refused precondition raises the WAIVER card — Kind
			// precondition_waiver, carrying the gate-authored RefusalMessage. This
			// is checked BEFORE wantApproval so a waiver can never be mistaken for
			// the ordinary tool_call card an ungated denial escalates to.
			if tc.wantWaiver != "" {
				require.NotNil(t, dec.Approval, tc.why)
				assert.Equal(t, pipeline.Allow, dec.Verdict, "a waiver card carries the gate; the verdict stays Allow")
				assert.Equal(t, categories.PreconditionWaiver, dec.Approval.Kind,
					"a refused precondition raises a precondition_waiver card, not a tool_call one — approving the latter would waive the gate through the grant path")
				assert.Contains(t, dec.Approval.Summary, tc.wantWaiver,
					"the waiver card carries the gate-authored refusal, the only sentence a human decides from")
				assert.NotContains(t, dec.Approval.Summary, checkerDeny,
					"the waiver shows the author's refusal, not the checker's grant-nobody-will-write sentence")
				return
			}

			if tc.wantApproval {
				require.NotNil(t, dec.Approval, tc.why)
				assert.Equal(t, pipeline.Allow, dec.Verdict, "an approval ask carries the gate; the verdict stays Allow")
				assert.Equal(t, "tool_call", dec.Approval.Kind,
					"an ordinary denial escalates to a tool_call card, never the waiver")
				return
			}
			assert.Nil(t, dec.Approval,
				"an Undetermined or Unevaluatable verdict is answerable only by the agent, not by a human clicking approve; no card is raised")

			if tc.wantReason == "" {
				assert.Equal(t, pipeline.Allow, dec.Verdict, tc.why)
				assert.Empty(t, dec.Reason, tc.why)
				return
			}
			require.Equal(t, pipeline.Deny, dec.Verdict, tc.why)
			assert.Contains(t, dec.Reason, tc.wantReason, tc.why)
			// The author's sentence REPLACES the checker's, rather than being
			// appended to it. Without this the row would pass on a Deny that
			// merely happened to mention the hint somewhere.
			assert.NotContains(t, dec.Reason, checkerDeny,
				"the SpiceDB denial points the agent at a grant nobody will write; the author's message is what it can act on")
		})
	}
}

// TestToolCallAuthz_PreconditionUnevaluatableFailsClosed pins the third value
// of what used to be a two-valued nil.
//
// "No gate is declared over this type" and "a gate is declared and could not be
// evaluated" were the same nil return, so the caller stamped no EnforceAlways
// for either — and under toolCalls.mode: permissive that meant a transient
// fact-store failure on a GATED type let the call through with one log line.
// A gate that cannot read its facts is a gate that said no; both rows below are
// permissive on purpose, because enforcing mode denies either way and would
// prove nothing about the override.
//
// The nil memory reaches the same branch a dead store does with no new fixture.
// It is a claim about the STRUCT, not about any shipped session: the runner
// wires this dep over whatever memory its Loop has, and Loop.Mem is an optional
// field, so a Loop can be constructed without one. The session binary always
// assigns it — the branch is reachable by construction, and it is the dead
// store, not the nil, that this stands in for.
func TestToolCallAuthz_PreconditionUnevaluatableFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// gated declares the slot's requires[]. The ungated row is the scope
		// guard: the fail-closed must reach classes that declared a gate and no
		// others, or every pre-precondition class starts denying on a store it
		// never used.
		gated    bool
		wantDeny bool
		why      string
	}{
		{
			name:     "gated type, no fact store, permissive: DENY — a gate that could not be evaluated said no",
			gated:    true,
			wantDeny: true,
			why:      "returning nil here is indistinguishable from 'no gate declared', and permissive then proceeds",
		},
		{
			name:  "ungated type, no fact store, permissive: proceeds — the fail-closed is scoped to a declared gate",
			gated: false,
			why:   "slotRules returns before any store is touched, so a class that declared no gate is untouched by its absence",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := ungatedSlotSpec()
			if tc.gated {
				spec = gatedSlotSpec(t)
			}
			// A true nil memory.Memory, not a typed-nil pointer in an
			// interface: the branch under test is `mem == nil`, and a typed nil
			// would sail past it and panic on the first method call instead.
			h := preconditionHook(t, "permissive", authz.Readonly, nil, memory.Scope{Kind: "session", ID: "ns/" + t.Name()},
				[]authz.BoundEntitySpec{spec},
				&authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"})

			dec := evalCall(h, gatedID)

			if !tc.wantDeny {
				assert.Equal(t, pipeline.Allow, dec.Verdict, tc.why)
				assert.Empty(t, dec.Reason, tc.why)
				assert.Nil(t, dec.Approval, "an ungated denial under permissive is logged and proceeds; it raises no card")
				return
			}
			require.Equal(t, pipeline.Deny, dec.Verdict, tc.why)
			assert.Contains(t, dec.Reason, unevaluatable,
				"the agent must be told the gate could not be evaluated, in those words")
			// It must not claim a VERDICT. Handing over the author's hint would
			// send the agent off to record a fact when the store that would hold
			// it is the thing that is broken; handing over the refusal would
			// report a rule as having decided something it never read.
			assert.NotContains(t, dec.Reason, preHint,
				"'could not look' is not Undetermined, and must not borrow its hint")
			assert.NotContains(t, dec.Reason, preRefusal,
				"'could not look' is not Refused, and must not borrow its refusal")
			assert.NotContains(t, dec.Reason, checkerDeny,
				"the message is replaced, not appended to; the SpiceDB sentence explains nothing here")
			assert.Nil(t, dec.Approval,
				"nobody can approve their way past a store that will not read, and the card would waive a rule whose verdict is unknown")
		})
	}
}

// preconditionLogHook is a hook whose precondition explanation reaches either a
// COMPUTED verdict or an unevaluatable answer, depending on the store it is
// given, logging into buf.
//
// A nil mem reaches ExplainPreconditionDenial's `mem == nil` branch and is the
// unevaluatable fixture; a seeded store produces a real verdict over a real
// recorded fact. Both are needed, and that is the whole design of this file's
// verdict test: PreconditionDenial.Unevaluatable SELECTS between two messages,
// so a fixture that can only produce one of them cannot show the choice is
// conditional on anything at all.
//
// The logger is captured rather than left on the default, because what is under
// test are LOG LINES — that is the whole surface on which a precondition denial
// describes itself to an operator.
func preconditionLogHook(t *testing.T, buf *bytes.Buffer, mem memory.Memory, memScope memory.Scope, force func(string) (string, bool)) *hooks.ToolCallAuthz {
	t.Helper()
	return hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              "permissive",
		Logger:            slog.New(slog.NewTextHandler(buf, nil)),
		Checker:           &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: checkerDeny}},
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"}}),
		BuildApprovalAsk:  simpleApprovalAskBuilder,
		ForceApproval:     force,
		ExplainPrecondition: func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
			// mem is passed through as given. A true nil memory.Memory, not a
			// typed-nil pointer in an interface: the branch it reaches is
			// `mem == nil`, and a typed nil would sail past it.
			return authz.ExplainPreconditionDenial(ctx, mem, memScope,
				[]authz.BoundEntitySpec{gatedSlotSpec(t)}, *perm.Check, args)
		},
	})
}

// refusedFactStore is the fixture whose gate actually DECIDES: the fork fact is
// recorded as true, so the predicate evaluates and comes back Refused.
func refusedFactStore(t *testing.T) (memory.Memory, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/" + t.Name()}
	seedFact(t, mem, memScope, gatedID, true)
	return mem, memScope
}

// undeterminedFactStore is the OTHER fixture whose gate decides — and it decides
// the answer an operator is most easily lied to about.
//
// The store is live and non-empty (a fact about a different commit of the same
// type is recorded), the predicate compiles, the read succeeds; the commit under
// test simply has no observation, so the rule comes back Undetermined with
// Unevaluatable UNSET. That combination is what separates the two consumers'
// decided branches from a constant: with only refusedFactStore in the file,
// every decided line this test ever saw said "refused", so hardcoding both
// branches to that word left the package green.
func undeterminedFactStore(t *testing.T) (memory.Memory, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/" + t.Name()}
	// A DIFFERENT commit, deliberately: an empty store would reach the same
	// verdict, and then the row could not tell "no fact about this commit" from
	// "no facts at all", which is the distinction the hint is written for.
	seedFact(t, mem, memScope, "0000000000", false)
	return mem, memScope
}

// logLineWith returns the single captured log line containing marker.
//
// Line-scoped on purpose. The buffer holds the explanation line AND the
// pin-drift line, and on a decided verdict both carry a verdict key — so a
// whole-buffer Contains for "verdict=refused" is satisfied by the explanation
// line and says nothing about the label the drift line chose, which is the
// value under test.
func logLineWith(t *testing.T, logs, marker string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	require.Failf(t, "no captured log line contains the marker", "marker %q; the log was:\n%s", marker, logs)
	return ""
}

// TestToolCallAuthz_PreconditionLogNamesAVerdictOnlyWhenOneExists pins the
// FIELD, at the only two places it is read, in BOTH of its directions.
//
// PreconditionDenial.Unevaluatable does not decide the Deny — the EnforceAlways
// stamp is unconditional on a non-nil explanation, which is why the fail-closed
// survives with the field flipped and why the security row is unaffected by it.
// What the field decides is whether an OPERATOR is told the truth about what
// happened, and both of its consumers exist for exactly that:
//
//   - explainPrecondition picks between "explains this denial" (with the
//     verdict) and "could NOT be evaluated" (with none);
//   - preconditionVerdictLabel picks between the verdict's own name and
//     "unevaluatable", so the pin-drift suppression line says which it was.
//
// Every one of those four sentences gets an assertion, because a field that
// SELECTS between two messages is only pinned by asserting on each message. One
// assertion plus a mutation of the PRODUCER walks both consumers down the same
// path and proves nothing about the branch: collapse either consumer to a
// constant and a one-sided test stays green while a decided verdict is reported
// to an operator as a gate nobody could read — the mirror-image lie.
//
// # The decided branch is exercised on BOTH verdicts, not just the one
//
// Pinning the unevaluatable branch against a single decided fixture leaves the
// decided arm collapsible to that fixture's answer: hardcode both consumers'
// else arms to "refused" and, with only refusedFactStore in the file, the whole
// of ./pkg/authz/... stays green. So each consumer is driven over an
// UNDETERMINED fixture too — the verdict this label's own doc comment calls the
// dangerous one to misreport, because an operator told "refused" concludes a
// rule decided against the call when the truth is that a fact has not been
// recorded and the agent could go record it.
//
// The assertions are on the LOG, not on the struct: reading the field back
// would prove the field, while these lines are the reason the field exists.
func TestToolCallAuthz_PreconditionLogNamesAVerdictOnlyWhenOneExists(t *testing.T) {
	const (
		explanationLine = "authz: a slot precondition"
		driftLine       = "escalation suppressed"
	)

	t.Run("unevaluatable: the explanation log claims no verdict and carries no verdict key at all", func(t *testing.T) {
		var buf bytes.Buffer
		h := preconditionLogHook(t, &buf, nil, memory.Scope{Kind: "session", ID: "ns/" + t.Name()}, nil)

		require.Equal(t, pipeline.Deny, evalCall(h, gatedID).Verdict,
			"the fixture is only meaningful while the call is actually refused")

		logged := buf.String()
		assert.Contains(t, logged, "could NOT be evaluated",
			"an operator has to be able to tell a gate that refused from a gate nobody could read")
		assert.NotContains(t, logged, "explains this denial",
			"nothing explained this denial: no rule was ever evaluated, so no rule can be named as the cause")
		assert.NotContains(t, logged, "verdict=",
			"Undetermined is the ZERO Verdict, so ANY verdict key on this path reports a verdict that was never computed")
	})

	t.Run("refused: the explanation log names the rule as the cause and reports the verdict it computed", func(t *testing.T) {
		var buf bytes.Buffer
		mem, memScope := refusedFactStore(t)
		h := preconditionLogHook(t, &buf, mem, memScope, nil)

		require.Equal(t, pipeline.Deny, evalCall(h, gatedID).Verdict,
			"the fixture is only meaningful while the call is actually refused")

		line := logLineWith(t, buf.String(), explanationLine)
		assert.Contains(t, line, "explains this denial",
			"a rule DID decide this: an operator reading it as unevaluatable would go looking for a broken store that is working fine")
		assert.Contains(t, line, "verdict=refused",
			"the verdict was computed, so it is reported; withholding it is the same lie in the other direction")
		assert.NotContains(t, line, "could NOT be evaluated",
			"the two sentences are exclusive — a line that says both tells an operator nothing")
	})

	t.Run("undetermined: the explanation log reports THAT verdict, not the other decided one", func(t *testing.T) {
		var buf bytes.Buffer
		mem, memScope := undeterminedFactStore(t)
		h := preconditionLogHook(t, &buf, mem, memScope, nil)

		require.Equal(t, pipeline.Deny, evalCall(h, gatedID).Verdict,
			"the fixture is only meaningful while the call is actually refused")

		line := logLineWith(t, buf.String(), explanationLine)
		assert.Contains(t, line, "explains this denial",
			"a rule DID run here — it read a live store and could not decide — so this is the decided-branch sentence, not the unevaluatable one")
		assert.Contains(t, line, "verdict=undetermined",
			"this arm must report the verdict it was GIVEN; an arm that names one verdict for every decided denial is a constant wearing a field's name")
		assert.NotContains(t, line, "verdict=refused",
			"'refused' says a rule decided against this commit, when nothing has been observed about it and the agent's next move is to go observe it")
	})

	t.Run("unevaluatable: the pin-drift suppression log labels the denial unevaluatable, not undetermined", func(t *testing.T) {
		var buf bytes.Buffer
		h := preconditionLogHook(t, &buf, nil, memory.Scope{Kind: "session", ID: "ns/" + t.Name()},
			func(string) (string, bool) {
				return "hooks-test drift: the backing image digest moved", true
			})

		dec := evalCall(h, gatedID)
		require.Equal(t, pipeline.Deny, dec.Verdict)
		assert.Nil(t, dec.Approval,
			"a drift escalation must not raise a card over a gate whose verdict is unknown")

		line := logLineWith(t, buf.String(), driftLine)
		assert.Contains(t, line, "verdict=unevaluatable",
			"the operator whose escalation was suppressed is owed the real reason, and 'no verdict' is the reason")
		assert.NotContains(t, line, "verdict=undetermined",
			"undetermined reads as 'some fact has not been observed yet', which is a claim about a rule that never ran")
	})

	t.Run("refused: the pin-drift suppression log names the verdict that suppressed it", func(t *testing.T) {
		var buf bytes.Buffer
		mem, memScope := refusedFactStore(t)
		h := preconditionLogHook(t, &buf, mem, memScope, func(string) (string, bool) {
			return "hooks-test drift: the backing image digest moved", true
		})

		dec := evalCall(h, gatedID)
		require.Equal(t, pipeline.Deny, dec.Verdict)
		assert.Nil(t, dec.Approval,
			"a drift escalation must not raise a card over a refused precondition either; approving it would waive the gate")

		line := logLineWith(t, buf.String(), driftLine)
		assert.Contains(t, line, "verdict=refused",
			"a label that reports every suppression as unevaluatable hides the one case an operator could actually act on")
		assert.NotContains(t, line, "verdict=unevaluatable",
			"the rule read its facts and decided; calling that 'no verdict' sends the reader after a store that is not broken")
	})

	t.Run("undetermined: the pin-drift suppression log names THAT verdict, not the other decided one", func(t *testing.T) {
		var buf bytes.Buffer
		mem, memScope := undeterminedFactStore(t)
		h := preconditionLogHook(t, &buf, mem, memScope, func(string) (string, bool) {
			return "hooks-test drift: the backing image digest moved", true
		})

		dec := evalCall(h, gatedID)
		require.Equal(t, pipeline.Deny, dec.Verdict)
		assert.Nil(t, dec.Approval,
			"a drift escalation must not raise a card over an unrecorded fact either; approving it would waive a gate that never decided anything")

		line := logLineWith(t, buf.String(), driftLine)
		assert.Contains(t, line, "verdict=undetermined",
			"the suppressed operator is owed the actual reason, and here it is a missing observation — the one reason somebody can still act on")
		assert.NotContains(t, line, "verdict=refused",
			"a label that reports every decided suppression as 'refused' sends an operator looking for a rule that ruled, when the fix is to record the fact")
		assert.NotContains(t, line, "verdict=unevaluatable",
			"the store read fine; blaming it hides that the gate is one observation away from deciding")
	})
}

// TestToolCallAuthz_PreconditionRecordsTheDecisionItHandsTheAgent pins the
// ORDER of the recomputation and the durable record.
//
// The authzdecision the runner writes is the evidentiary trail — and
// steelthread's gateRefused reads a recorded denial's message as "exactly the
// bytes the model was handed". Recompute after recording and the audit says the
// call was refused for a reason the agent was never told, which is the kind of
// disagreement nobody notices until they are reading a transcript months later.
func TestToolCallAuthz_PreconditionRecordsTheDecisionItHandsTheAgent(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/recorded"}
	seedFact(t, mem, memScope, gatedID, true)

	var recorded authz.Result
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              "enforcing",
		Checker:           &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: checkerDeny}},
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"}}),
		BuildApprovalAsk:  simpleApprovalAskBuilder,
		RecordDecision: func(_ context.Context, _ string, _ authz.Permission, res authz.Result, _ authz.Inputs) {
			recorded = res
		},
		ExplainPrecondition: func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
			return authz.ExplainPreconditionDenial(ctx, mem, memScope, []authz.BoundEntitySpec{gatedSlotSpec(t)}, *perm.Check, args)
		},
	})

	dec := evalCall(h, gatedID)
	require.Equal(t, pipeline.Deny, dec.Verdict)

	assert.Equal(t, preRefusal, recorded.Message,
		"the recorded decision must carry the same sentence the agent was handed, not the check's")
	// What this pins is the RESULT handed to RecordDecision, not the durable
	// row: this package has no writer. The override has to be on the Result for
	// the writer to have anything to record, and what the writer then does with
	// it — prefer it over the class's declared enforceMode — is pinned where the
	// row is actually built, by pkg/agent/runner's recordedEnforceMode test.
	assert.Equal(t, authz.EnforceAlways, recorded.EnforceOverride,
		"the recorded Result must carry the override that made the denial final")
	if assert.NotNil(t, recorded.Precondition, "the record must name the cause as a slot precondition") {
		assert.Equal(t, precondition.Refused, recorded.Precondition.Verdict)
	}
}

// TestToolCallAuthz_PreconditionPinDriftCannotRaiseACard closes the ONE route
// by which this plan could still have reached BuildApprovalAsk.
//
// ForceApproval — the dependency-pinning "approve" drift mode — sets
// needsApproval to true regardless of what the check decided, and it is
// consulted after the precondition recomputation. Without the suppression it
// would raise a tool_call card for a precondition-refused call, and approving
// one WRITES the slot binding: BindApproved waives preconditions by design,
// because approving a card IS the waiver. So a human answering a question about
// a changed dependency digest would silently grant the fork review nobody asked
// them about — the unappealable-gate boundary breached by a gate about
// something else entirely.
func TestToolCallAuthz_PreconditionPinDriftCannotRaiseACard(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/pin-drift"}
	seedFact(t, mem, memScope, gatedID, true) // a fork: Refused

	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              "enforcing",
		Checker:           &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: checkerDeny}},
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"}}),
		BuildApprovalAsk:  simpleApprovalAskBuilder,
		ForceApproval: func(string) (string, bool) {
			return "hooks-test drift: the backing image digest moved", true
		},
		ExplainPrecondition: func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
			return authz.ExplainPreconditionDenial(ctx, mem, memScope, []authz.BoundEntitySpec{gatedSlotSpec(t)}, *perm.Check, args)
		},
	})

	dec := evalCall(h, gatedID)

	assert.Nil(t, dec.Approval, "a pin-drift escalation must not become a precondition waiver")
	require.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, preRefusal, "the precondition is what refuses the call, so it is what the agent is told")

	// The counterfactual, so the row above cannot pass merely because drift
	// escalation is broken: the SAME drift, with no gate declared, still raises
	// its card — and the card is raised BY THE DRIFT.
	//
	// The checker ALLOWS here, deliberately. With a denial, needsApproval
	// returns true on its own (a denied readonly with a Check is exactly the
	// ordinary escalation), so the assertion below would hold with ForceApproval
	// removed entirely and would prove only that the approval path works. An
	// allowed check makes needsApproval return false at its first line, leaving
	// ForceApproval as the single thing that can produce a card.
	//
	// The justification is captured rather than discarded for the second half
	// of the same argument: a card is not evidence that DRIFT raised it, but a
	// card carrying the [PIN DRIFT: …] marker is — nothing else in this hook
	// writes that prefix.
	var gotJustification string
	ungated := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              "enforcing",
		Checker:           &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}},
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: authz.Readonly, Check: &authz.PermissionCheck{ResourceType: gatedType, Permission: "read", ResourceIDTemplate: "{sha}"}}),
		BuildApprovalAsk: func(_ context.Context, in pipeline.Input, _ map[string]any, _ authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			gotJustification = justification
			return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve " + in.Tool.Name}, nil
		},
		ForceApproval: func(string) (string, bool) {
			return "hooks-test drift: the backing image digest moved", true
		},
		ExplainPrecondition: func(ctx context.Context, perm authz.Permission, args map[string]any) *authz.PreconditionDenial {
			return authz.ExplainPreconditionDenial(ctx, mem, memScope, []authz.BoundEntitySpec{ungatedSlotSpec()}, *perm.Check, args)
		},
	})
	assert.NotNil(t, ungated.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "inspect", Args: json.RawMessage(`{"sha":"` + gatedID + `"}`)},
	}).Approval, "pin-drift escalation itself must still work where no precondition governs the call")
	assert.Contains(t, gotJustification, "[PIN DRIFT:",
		"the card must be the DRIFT's card; without the marker it is some other escalation and proves nothing about drift reaching the ask")
}

// TestToolCallAuthz_PreconditionLooksFactsUpByTheRawID is RULING P-2 at the
// dispatch end: the bind-time filter and this explanation must derive the same
// lookup id, or the filter holds candidate X while the agent is told about
// candidate Y.
//
// Facts are keyed by the RAW provider id; a check's ResourceIDTransforms exist
// to make that value legal as a SpiceDB object id (`spicedb_escape` is there
// because `#` is illegal in one). Look the fact up post-transform and it cannot
// match — and the miss reads as "nothing observed", so the wrong hint would be
// handed back forever with nothing reporting a fault.
func TestToolCallAuthz_PreconditionLooksFactsUpByTheRawID(t *testing.T) {
	const rawID = "demo-org/demo-repo#6"

	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "ns/raw-id"}
	seedFact(t, mem, memScope, rawID, true) // a fork: Refused, if it is found at all

	escaped, err := authz.ApplyTransforms(rawID, []string{"spicedb_escape"})
	require.NoError(t, err)
	require.NotEqual(t, rawID, escaped,
		"the fixture is only meaningful while the transform actually changes the id")

	h := preconditionHook(t, "enforcing", authz.Readonly, mem, memScope, []authz.BoundEntitySpec{gatedSlotSpec(t)},
		&authz.PermissionCheck{
			ResourceType:         gatedType,
			Permission:           "read",
			ResourceIDTemplate:   "{sha}",
			ResourceIDTransforms: []string{"spicedb_escape"},
		})

	dec := evalCall(h, rawID)

	// The raw-id lookup finds the fork fact → Refused → the waiver card, carrying
	// the author's refusal. A lookup by the ESCAPED id would find no fact, report
	// Undetermined, and hand back the hint as a tool result (a Deny, no card) —
	// so both the card's presence AND its refusal text prove the raw id was used.
	require.NotNil(t, dec.Approval, "the fork fact is found only by the raw id; found, it is Refused and raises the waiver")
	assert.Equal(t, categories.PreconditionWaiver, dec.Approval.Kind)
	assert.Contains(t, dec.Approval.Summary, preRefusal,
		"a lookup by the ESCAPED id finds no fact, reports undetermined, and would hand back the hint instead")
	assert.NotContains(t, dec.Approval.Summary, preHint)
}
