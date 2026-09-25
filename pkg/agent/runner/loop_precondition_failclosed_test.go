package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The fixture vocabulary for this file. Each string is unique in the repo,
// because a Deny alone proves nothing about WHICH refusal produced it: the
// same call is denied by the checker, by a compile failure, and by an unread
// fact store, so every row asserts the words only its own branch emits.
const (
	failClosedType = "git_commit"
	// failClosedUngatedType is a SECOND slot on the same class, declared with no
	// requires[] at all. It is what makes "the class is broken" and "this call
	// is gated" separable: without it every row's call names the one gated type
	// and a class-wide refusal is indistinguishable from a scoped one.
	failClosedUngatedType = "wiki_page"
	failClosedID          = "ba03f5969a"
	// failClosedCEL is a predicate that compiles.
	failClosedCEL = "facts.observed.is_cross_repository == false"
	// failClosedBadCEL is one that does not. slotspec's compileRequires is the
	// only error source in the whole conversion, so this is the only way a
	// class reaches the runner with specs that will not build.
	failClosedBadCEL = "facts.observed.is_cross_repository ==="
	// The authored messages. An unevaluatable refusal must borrow NEITHER: it
	// computed no verdict, so claiming one would send the agent off to record a
	// fact or tell it a rule decided something it never read.
	failClosedHint    = "runner-test hint: nothing has observed whether this commit came from a fork; call the inspect tool first"
	failClosedRefusal = "runner-test refusal: this commit's head lives on a fork, so checking it out would run untrusted code"
	// failClosedCheckerDeny is what the CHECKER says. A gated row must not be
	// explained by it.
	failClosedCheckerDeny = "runner-test permission denied on git_commit"
	// failClosedShared is the clause every "could not compute a verdict" answer
	// carries, wherever in the wiring the failure happened.
	failClosedShared = "the gate could not be evaluated"
)

// failClosedLog is the captured log both loggers write into.
//
// Guarded, not a bare bytes.Buffer: slog.SetDefault makes this handler
// reachable from every goroutine the package's suite still has running, so an
// unrelated best-effort log line landing mid-assertion would be a data race
// under -race rather than a test failure anyone could read.
type failClosedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *failClosedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *failClosedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// failClosedDenier is a checker that refuses every call, standing in for the
// SpiceDB denial an empty slot produces.
type failClosedDenier struct{}

func (failClosedDenier) CheckToolCall(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	return authz.Result{Outcome: authz.OutcomeDenied, Message: failClosedCheckerDeny}
}

// failClosedSlot declares one slot over resourceType, gated on cel when cel is
// non-empty and ungated otherwise.
func failClosedSlot(resourceType, cel string) spiceboxv1alpha1.AuthzSlot {
	slot := spiceboxv1alpha1.AuthzSlot{
		ResourceType: resourceType,
		Description:  "the tree a checkout may land on",
		Permission:   "read",
		FillFrom:     []string{"observed"},
	}
	if cel != "" {
		slot.Requires = []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              cel,
			UndeterminedHint: failClosedHint,
			RefusalMessage:   failClosedRefusal,
		}}
	}
	return slot
}

// failClosedClass wraps a slot list into the AgentClass under test. A nil list
// is the class that declares nothing on the instance axis at all.
func failClosedClass(slots []spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	if len(slots) == 0 {
		return &spiceboxv1alpha1.AgentClass{}
	}
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Slots: slots},
		},
	}
}

// TestExplainSlotPrecondition_UnevaluatableGateFailsClosed pins the two ways
// the WIRING can fail to produce a verdict, both of which used to fail OPEN,
// and the scope that keeps either from reaching a call nobody gated.
//
// pkg/authz closed this at eval time: a gate whose facts cannot be read comes
// back Unevaluatable and is enforced. But two failures never reach that
// function at all, and both of them live here:
//
//   - the class's predicates will not COMPILE, so there are no specs to
//     evaluate — the closure logged and returned nil;
//   - the session has no memory store, so the guard returned a nil FUNC and
//     switched the whole explanation off.
//
// Both nils are indistinguishable from "this class declares no gate", so under
// toolCalls.mode: permissive the hook logged a deny and let the call proceed —
// exactly the bypass the eval-time fix removes, reached one and two layers up.
// Every row is permissive on purpose: enforcing mode denies either way and
// would prove nothing about the override.
//
// The compile failure is class-wide and the refusal it produces is NOT: the
// last three rows carry a broken gate over one type and an ordinary ungated
// slot over another, and pin that the ungated type keeps the behaviour it had
// before preconditions existed — including, for an External call, its approval
// card, which is a human's whole route and the one thing a mis-scoped refusal
// silently deletes.
//
// Driven through the REAL hook with the REAL dep the runner wires, because the
// claim is about what a tool call does, not about what a closure returns. A
// test that only called the closure would pass while the hook ignored it.
func TestExplainSlotPrecondition_UnevaluatableGateFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// slots is the class's whole instance axis. Empty is the class that
		// declares nothing there — the only shape for which a nil FUNC is the
		// right answer, and the only one that pays nothing at all.
		slots []spiceboxv1alpha1.AuthzSlot
		// callType is the resource type the checked call names, which is not
		// always the gated one.
		callType string
		// impact is the call's state impact. External is the row where a denial
		// is a human's to answer rather than the agent's.
		impact authz.StateImpact
		// wireMemory supplies a real fact store. False is a session that has
		// none, which the Loop struct permits.
		wireMemory bool
		// wantDenyContains is the sentence the agent must be handed, or "" when
		// the call must not be denied at all.
		wantDenyContains string
		// wantCard is true when the denial must still reach a human. It is the
		// consequence a mis-scoped refusal removes, and it is not visible in a
		// Deny/Allow verdict: an escalated call's verdict is Allow.
		wantCard bool
		// wantLogContains is the line an operator must be able to find. The two
		// branches of the compile failure differ in the LOG before they differ
		// in anything the agent sees.
		wantLogContains string
		why             string
	}{
		{
			name:             "gated class whose predicate will not compile, permissive: DENY — a gate that could not be built said no",
			slots:            []spiceboxv1alpha1.AuthzSlot{failClosedSlot(failClosedType, failClosedBadCEL)},
			callType:         failClosedType,
			impact:           authz.Readonly,
			wireMemory:       true,
			wantDenyContains: "declares a slot precondition that does not compile",
			wantLogContains:  "a denied call is refused as unevaluatable",
			why:              "returning nil here says 'no gate is declared', and permissive then runs the call the class gated",
		},
		{
			name:             "gated class with no fact store, permissive: DENY — the guard must not switch the explanation off",
			slots:            []spiceboxv1alpha1.AuthzSlot{failClosedSlot(failClosedType, failClosedCEL)},
			callType:         failClosedType,
			impact:           authz.Readonly,
			wantDenyContains: "no fact store is wired for this session",
			why:              "a nil FUNC can only say 'no gate declared'; a missing store is a declared gate nobody can read",
		},
		{
			name:     "slot declared with no gate over it, no fact store, permissive: proceeds — the fail-closed is scoped to a declared GATE",
			slots:    []spiceboxv1alpha1.AuthzSlot{failClosedSlot(failClosedType, "")},
			callType: failClosedType,
			impact:   authz.Readonly,
			why:      "every class that predates preconditions declares slots, so its explanation IS wired now; it must still proceed",
		},
		{
			name:     "no slots at all, no fact store, permissive: proceeds, and the explanation is never wired",
			callType: failClosedType,
			impact:   authz.Readonly,
			why:      "a class with nothing on the instance axis can never have a precondition to explain, and pays nothing",
		},
		{
			name: "a sibling slot's compile failure, call on the UNGATED type, permissive: proceeds — the class is broken, this type is not gated",
			slots: []spiceboxv1alpha1.AuthzSlot{
				failClosedSlot(failClosedType, failClosedBadCEL),
				failClosedSlot(failClosedUngatedType, ""),
			},
			callType:        failClosedUngatedType,
			impact:          authz.Readonly,
			wireMemory:      true,
			wantLogContains: "this call's type declares none",
			why:             "the conversion fails class-wide, but requires[] is a plain spec field: refusing a type the author never gated makes an appealable denial unappealable",
		},
		{
			name: "a sibling slot's compile failure, EXTERNAL call on the UNGATED type: the human keeps their card",
			slots: []spiceboxv1alpha1.AuthzSlot{
				failClosedSlot(failClosedType, failClosedBadCEL),
				failClosedSlot(failClosedUngatedType, ""),
			},
			callType:   failClosedUngatedType,
			impact:     authz.External,
			wireMemory: true,
			wantCard:   true,
			why:        "needsApproval short-circuits on a non-nil Precondition BEFORE the External arm, so a mis-scoped refusal deletes the waiver path over a resource nobody gated",
		},
		{
			name:             "gated class whose predicate will not compile, EXTERNAL call on the GATED type: DENY, and no card",
			slots:            []spiceboxv1alpha1.AuthzSlot{failClosedSlot(failClosedType, failClosedBadCEL)},
			callType:         failClosedType,
			impact:           authz.External,
			wireMemory:       true,
			wantDenyContains: "declares a slot precondition that does not compile",
			why:              "losing the card IS correct here: nobody can approve their way past a gate whose declaration will not build, and approving would waive it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One buffer for both loggers. explainSlotPrecondition logs through
			// slog.Default and the hook logs through its own dep, and the rows
			// assert across both — the compile failure's two branches are
			// distinguishable in the log before they are distinguishable in
			// anything the agent receives. Capturing also keeps a deliberate
			// CEL syntax error out of the suite's stderr.
			var logs failClosedLog
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			prev := slog.Default()
			slog.SetDefault(logger)
			t.Cleanup(func() { slog.SetDefault(prev) })

			l := &Loop{
				AgentClass: failClosedClass(tc.slots),
				SessionKey: memory.NamespacedName{Namespace: "default", Name: "fail-closed"},
			}
			if tc.wireMemory {
				l.Mem = memory.NewLocal(inmem.NewBackend())
			}

			dep := l.explainSlotPrecondition()
			if len(tc.slots) == 0 {
				require.Nil(t, dep,
					"no slots declared is the one thing a nil func may mean, and it is what keeps a class off the instance axis free")
			} else {
				require.NotNil(t, dep,
					"a class that declared slots must keep its explanation wired whatever the session has; a nil func can only report 'no gate declared', and a session with no store is not that")
			}

			h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
				Mode:    "permissive",
				Logger:  logger,
				Checker: failClosedDenier{},
				ResolvePermission: func(string, map[string]any) (authz.Permission, error) {
					return authz.Permission{
						StateImpact: tc.impact,
						Check: &authz.PermissionCheck{
							ResourceType:       tc.callType,
							Permission:         "read",
							ResourceIDTemplate: "{sha}",
						},
					}, nil
				},
				// Wired on every row, including the ones that must not reach it:
				// a row that wrongly escalated would come back with a card
				// instead of failing quietly.
				BuildApprovalAsk: func(_ context.Context, in pipeline.Input, _ map[string]any, _ authz.Permission, _ string) (*pipeline.ApprovalAsk, error) {
					return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve " + in.Tool.Name}, nil
				},
				ExplainPrecondition: dep,
			})

			dec := h.Eval(context.Background(), pipeline.Input{
				Point: pipeline.PreToolCall,
				Tool: &pipeline.ToolCallInfo{
					Name: "inspect",
					Args: json.RawMessage(`{"sha":"` + failClosedID + `"}`),
				},
			})

			if tc.wantLogContains != "" {
				assert.Contains(t, logs.String(), tc.wantLogContains,
					"the two branches of a class-wide compile failure differ in what an operator is told, and only one of them says the call was refused")
			}

			switch {
			case tc.wantCard:
				assert.Equal(t, pipeline.Allow, dec.Verdict,
					"an escalated call's verdict is Allow; the card is what carries the refusal to a human")
				require.NotNil(t, dec.Approval, tc.why)
				assert.Empty(t, dec.Reason,
					"a card is not a denial: the reason field stays empty and the human decides")
			case tc.wantDenyContains == "":
				assert.Equal(t, pipeline.Allow, dec.Verdict, tc.why)
				assert.Empty(t, dec.Reason, tc.why)
				assert.Nil(t, dec.Approval, "an ungated readonly denial under permissive is logged and proceeds; it raises no card")
			default:
				require.Equal(t, pipeline.Deny, dec.Verdict, tc.why)
				assert.Contains(t, dec.Reason, failClosedShared,
					"the agent must be told the gate could not be evaluated, in the same words every other unevaluatable answer uses")
				assert.Contains(t, dec.Reason, tc.wantDenyContains,
					"each cause is actionably different, so each names its own")
				assert.NotContains(t, dec.Reason, failClosedHint,
					"'could not look' is not Undetermined, and must not borrow the author's hint")
				assert.NotContains(t, dec.Reason, failClosedRefusal,
					"'could not look' is not Refused, and must not borrow the author's refusal")
				assert.NotContains(t, dec.Reason, failClosedCheckerDeny,
					"the message is replaced, not appended to; the SpiceDB sentence explains nothing here")
				assert.Nil(t, dec.Approval,
					"nobody can approve their way past a gate whose verdict is unknown, and approving one waives the precondition")
			}
		})
	}
}
