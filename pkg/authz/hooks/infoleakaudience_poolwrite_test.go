package hooks_test

// The cross-write refusal.
//
// The slot gate (pkg/memory/httpsrv) proves a session may write to a pool at
// all. This is the other half: a session that has READ from one resource must
// not carry that data into another resource's pool, where a wider audience
// would see it. The destination's audience is whoever holds view_memory on it,
// derived live; the session's permitted set is the intersection across
// everything it has read. Anything in the first and not the second is a leak,
// and it is judged BEFORE the write is dispatched.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// poolWriteToolName is a stand-in for the observation-recording tool. The hook
// learns the name from its declaration lookup and nothing else, so the tests
// name it here rather than importing the tool package.
const poolWriteToolName = "record_observation"

// taint builds the session's accumulated read set: one record per resource the
// session has read, under the permission its readers are drawn from.
func taint(objType, objID, permission string) []infoleakagetaint.TaintRecord {
	return []infoleakagetaint.TaintRecord{{
		ResourceType: objType,
		ResourceID:   objID,
		Permission:   permission,
		ToolName:     "search_memory",
	}}
}

// subjects builds a LookupSubjects over a fixed "<type>:<id>" -> subjects map.
// A resource absent from the map expands to nobody, which is how an inert
// destination is expressed.
func subjects(m map[string][]string) func(context.Context, string, string) ([]string, error) {
	return func(_ context.Context, resource, _ string) ([]string, error) {
		return m[resource], nil
	}
}

// poolWriteHook builds the audience hook with a write declaration naming the
// `resource` argument.
//
// ResolveAudience is deliberately NIL in every case here. The pool write gate
// must not depend on a channel resolver being wired: a session driven by
// kubectl with no channel binding writes into pools like any other, and a
// bypass keyed on the channel would skip the gate carrying the whole
// cross-contamination refusal.
func poolWriteHook(
	t *testing.T,
	mode string,
	sessionTaint []infoleakagetaint.TaintRecord,
	lookup func(context.Context, string, string) ([]string, error),
) *hooks.InfoLeakAudience {
	t.Helper()
	return hooks.NewInfoLeakAudience(poolWriteDeps(t, mode, sessionTaint, lookup))
}

// poolWriteDeps is poolWriteHook's deps, exposed so a case can add one field
// — the approval builder — without restating the declaration closure.
func poolWriteDeps(
	t *testing.T,
	mode string,
	sessionTaint []infoleakagetaint.TaintRecord,
	lookup func(context.Context, string, string) ([]string, error),
) hooks.InfoLeakAudienceDeps {
	t.Helper()
	return hooks.InfoLeakAudienceDeps{
		Mode: mode,
		LookupWrites: func(name string) *hooks.ToolWritesDecl {
			if name != poolWriteToolName {
				return nil
			}
			return &hooks.ToolWritesDecl{DestinationArg: "resource"}
		},
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return sessionTaint, nil
		},
		LookupSubjects: lookup,
	}
}

// recordingSubjects is subjects() that also records every resource whose
// audience was expanded, in order. The FIRST entry is the destination the gate
// actually judged — which is the only way to tell "refused the right pool" from
// "refused some pool".
func recordingSubjects(m map[string][]string, seen *[]string) func(context.Context, string, string) ([]string, error) {
	return func(_ context.Context, resource, _ string) ([]string, error) {
		*seen = append(*seen, resource)
		return m[resource], nil
	}
}

// poolWriteHookWithLookupError is the same hook whose subject expansion fails,
// so the destination's audience is UNKNOWN rather than empty.
func poolWriteHookWithLookupError(t *testing.T, mode string, sessionTaint []infoleakagetaint.TaintRecord) *hooks.InfoLeakAudience {
	t.Helper()
	return poolWriteHook(t, mode, sessionTaint, func(context.Context, string, string) ([]string, error) {
		return nil, fmt.Errorf("spicedb: unavailable")
	})
}

// poolWriteCall is the tool call under test: a write naming its destination in
// the `resource` argument.
func poolWriteCall(destination, text string) pipeline.Input {
	args, err := json.Marshal(map[string]string{"resource": destination, "text": text})
	if err != nil {
		panic(err)
	}
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name:  poolWriteToolName,
			Args:  args,
			UseID: "toolu_write_1",
		},
	}
}

// The core refusal: a session that read pool A cannot write to pool B when
// B's audience contains anyone A's does not.
func TestACrossPoolWriteIsRefused(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory), // session read dossier:d-1
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"}, // bo cannot see d-1
		}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "some text"))

	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "ledger:l-9")
	assert.Contains(t, dec.Reason, "user:bo", "name who would newly see it")
}

// TestAPoolWriteNeverAsksAnApproverEvenWithTheApprovalPathWired is the test
// whose absence let an implementation that matched the plan line-for-line be
// wrong against the spec.
//
// The spec rules that a cross-pool write is refused, fail-closed, WITH NO
// PROMPT — a leakage_share card was on the table and was declined. The test
// above passes either way when nothing is wired to escalate to, because the
// gate then fails closed for an unrelated reason; a real cluster wires
// BuildApprovalAsk, and there the two behaviours differ completely.
//
// What makes the card wrong is not its copy. Approving one runs
// leakagePostApprove -> LeakageGrantWriter over the TAINT's resources, so a
// yes would not permit this write — it would widen who may read the resource
// the session read FROM, which nobody chose and which the card does not say.
func TestAPoolWriteNeverAsksAnApproverEvenWithTheApprovalPathWired(t *testing.T) {
	asks := 0
	d := poolWriteDeps(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"},
		}))
	d.BuildApprovalAsk = func(context.Context, []string, []infoleakagetaint.TaintRecord, string) (*pipeline.ApprovalAsk, error) {
		asks++
		return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
	}
	h := hooks.NewInfoLeakAudience(d)

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "some text"))

	assert.Equal(t, pipeline.Deny, dec.Verdict, "a cross-pool write is refused, not negotiated")
	assert.Nil(t, dec.Approval, "no card may be raised for a pool destination")
	assert.Zero(t, asks, "the approval builder must not even be consulted")
	assert.Contains(t, dec.Reason, "ledger:l-9")
	assert.Contains(t, dec.Reason, "user:bo")
}

// TestAChannelReplyStillAsksAnApproverWhenOneIsWired is the other direction,
// and it is half the point: MayAskAnApprover distinguishes two destinations,
// so a version that simply stopped escalating everywhere would satisfy the
// test above and quietly remove the human from every channel leak.
func TestAChannelReplyStillAsksAnApproverWhenOneIsWired(t *testing.T) {
	asks := 0
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		ResolveAudience: func(context.Context) ([]string, channelkinds.Capability, error) {
			return []string{"user:ann", "user:bo"}, channelkinds.CapabilityFull, nil
		},
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return taint("dossier", "d-1", memory.PermissionViewMemory), nil
		},
		LookupSubjects: subjects(map[string][]string{"dossier:d-1": {"user:ann"}}),
		BuildApprovalAsk: func(context.Context, []string, []infoleakagetaint.TaintRecord, string) (*pipeline.ApprovalAsk, error) {
			asks++
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
	})

	dec := h.Eval(context.Background(), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is what the dossier says"},
	})

	require.NotNil(t, dec.Approval, "a channel leak still goes to the data owner")
	assert.Equal(t, "leakage_share", dec.Approval.Kind)
	assert.Equal(t, 1, asks)
}

// The same write is allowed when the destination's audience is a subset.
func TestAWriteToAPoolWhoseAudienceIsAlreadyPermittedIsAllowed(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann", "user:bo"},
			"ledger:l-9":  {"user:ann"},
		}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "some text"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Equal(t, []string{"respond_no_leak"}, auditKinds(dec),
		"the gate RAN and allowed — a bare Allow is also what a gate that never ran returns")
}

// auditKinds is the Kind of every record a decision carries, in order. It is
// what distinguishes "the gate ran and found no leak" from "the gate did not
// run", which the zero Verdict cannot.
func auditKinds(dec pipeline.Decision) []string {
	out := make([]string, 0, len(dec.Audit))
	for _, a := range dec.Audit {
		out = append(out, a.Kind)
	}
	return out
}

// A fresh session must be able to write. If an empty taint set yielded an
// empty permitted set, leakedTo would be the whole audience and EVERY first
// write would be refused — a conservative-looking bug that makes the feature
// useless.
//
// Named for what this gate measures — taint against audience — and not for a
// grant, which it never consults. Whether the session may write the
// destination at all is the memory door's question (httpsrv.destinationFor),
// and a name promising a grant check here would read as coverage of a gate
// that lives somewhere else entirely.
func TestAFreshSessionWithNoReadsIsNotRefusedByTheAudienceComparison(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		nil, // no taint at all
		subjects(map[string][]string{"ledger:l-9": {"user:ann", "user:bo"}}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "first thing I learned"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

// TestAFreshSessionsWriteAsksNothingOfTheAuthorizer pins the empty-taint
// short-circuit itself, which the verdict above does NOT.
//
// Deleting `if len(taint) == 0 { return }` leaves the test above green:
// filterTaintCausedByAudience also yields nothing for an empty taint set, so
// the gate arrives at the same allow one branch later. What the short-circuit
// actually decides is whether a session that has read NOTHING still pays a
// subject expansion — a SpiceDB round trip on every first write — and emits a
// no-leak audit for a comparison with no left-hand side. Assert the I/O, or
// the guard is unpinned and a later change to the causative filter restores
// the "empty permitted set ⇒ leak to everyone" trap with every test green.
func TestAFreshSessionsWriteAsksNothingOfTheAuthorizer(t *testing.T) {
	calls := 0
	h := poolWriteHook(t, "enforcing", nil, func(context.Context, string, string) ([]string, error) {
		calls++
		return []string{"user:ann", "user:bo"}, nil
	})

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "first thing I learned"))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Zero(t, calls,
		"a session that has read nothing constrains nothing; the destination need not even be expanded")
	assert.Empty(t, dec.Audit, "and there is no leak comparison to record")
}

// An empty destination audience allows the write: the audience derives live,
// so declaring view_memory later makes everything already written readable.
func TestAnEmptyDestinationAudienceAllowsTheWrite(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{"dossier:d-1": {"user:ann"}, "ledger:l-9": {}}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))
	assert.Equal(t, pipeline.Allow, dec.Verdict, "an inert pool is not a leak")
	assert.Equal(t, []string{"respond_no_leak"}, auditKinds(dec),
		"vacuously safe, not unexamined — the gate expanded the pool and found nobody")
}

// An audience we could not COMPUTE is unknown, not empty — and unknown
// refuses. This is the failure mode the empty-audience ruling creates.
func TestAnUnresolvableDestinationAudienceRefuses(t *testing.T) {
	h := poolWriteHookWithLookupError(t, "enforcing", taint("dossier", "d-1", memory.PermissionViewMemory))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))
	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"an unknown audience must never be read as an empty one")
}

// Logging mode changes nothing, as it does everywhere else.
func TestLoggingModeAuditsTheCrossWriteAndAllowsIt(t *testing.T) {
	h := poolWriteHook(t, "logging",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{"dossier:d-1": {"user:ann"}, "ledger:l-9": {"user:ann", "user:bo"}}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.NotEmpty(t, dec.Audit)
}

// A tool with no writes declaration is untouched at PreToolCall.
func TestAToolWithNoWritesDeclarationIsUntouched(t *testing.T) {
	h := poolWriteHook(t, "enforcing", taint("dossier", "d-1", memory.PermissionViewMemory), subjects(nil))
	in := poolWriteCall("ledger:l-9", "x")
	in.Tool.Name = "some_other_tool"

	dec := h.Eval(context.Background(), in)
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, dec.Audit)
}

// TestAPoolWriteIsGatedWithNoChannelAudienceResolverWired pins where the
// no-resolver bypass lives.
//
// `if h.d.ResolveAudience == nil { return Decision{} }` is a statement about
// the CHANNEL — no resolver, no channel gate — so it belongs to
// enforceAudienceForChannel. Put it in the shared gate instead and it
// short-circuits a POOL destination too, for every session with no channel
// binding: fail-open, in the gate carrying the whole cross-contamination
// refusal.
//
// Every test in this file leaves ResolveAudience nil, so all of them would go
// green on a move back; this one says WHY out loud.
func TestAPoolWriteIsGatedWithNoChannelAudienceResolverWired(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"},
		}))

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))
	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"a session with no channel resolver still has its pool writes gated")
}

// TestAChannelReplyWithNoAudienceResolverIsStillAllowed is the other side of
// the relocation: the channel's own no-resolver bypass must survive it
// unchanged, and must not become a refusal (or a panic on the nil resolver
// func) now that a destination is built before the check.
func TestAChannelReplyWithNoAudienceResolverIsStillAllowed(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return taint("dossier", "d-1", memory.PermissionViewMemory), nil
		},
		LookupSubjects: subjects(map[string][]string{"dossier:d-1": {"user:ann"}}),
	})

	dec := h.Eval(context.Background(), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is what I found"},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict,
		"no channel audience resolver ⇒ no channel gate, exactly as before")
}

// TestTheGateResolvesTheDestinationTheToolWillRead is the property the whole
// refusal rests on: the gate and the executor must be reading the same field.
//
// The dispatcher unwraps the {operation_id, _reason, args} envelope ONLY when
// operation_id is present; a meta tool unmarshals the top level into its own
// struct. Resolve it by any other rule and the gate judges one destination
// while the write goes to another.
func TestTheGateResolvesTheDestinationTheToolWillRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{
			name: "flat args, as a meta tool is called: the top level is what the tool unmarshals",
			args: `{"resource":"ledger:l-9","text":"x"}`,
			want: "ledger:l-9",
		},
		{
			name: "a full envelope: the dispatcher unwraps it, so the inner value is what the tool sees",
			args: `{"operation_id":"op","_reason":"r","args":{"resource":"ledger:l-9","text":"x"}}`,
			want: "ledger:l-9",
		},
		{
			name: "an args object with no operation_id is not an envelope: the top level still wins",
			args: `{"resource":"ledger:l-9","text":"x","args":{"unrelated":"y"}}`,
			want: "ledger:l-9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			h := poolWriteHook(t, "enforcing",
				taint("dossier", "d-1", memory.PermissionViewMemory),
				recordingSubjects(map[string][]string{
					"dossier:d-1": {"user:ann"},
					"ledger:l-9":  {"user:ann", "user:bo"},
				}, &seen))
			in := poolWriteCall("unused", "x")
			in.Tool.Args = json.RawMessage(tc.args)

			dec := h.Eval(context.Background(), in)

			require.NotEmpty(t, seen, "the gate must have expanded a destination")
			assert.Equal(t, tc.want, seen[0], "the gate judged a different pool than the tool will write to")
			assert.Equal(t, pipeline.Deny, dec.Verdict)
		})
	}
}

// TestAStrayArgsObjectCannotAimTheGateAtADifferentPool is the payload that
// broke it, verbatim.
//
// `{"resource":"ledger:l-9","text":"…","args":{"resource":"dossier:d-1"}}` — a
// sibling `args` object with no operation_id. Read by the rule the surrounding
// hook uses for READ declarations, the gate resolves dossier:d-1, a pool the
// session has already read and may therefore write to, and allows; the tool
// unmarshals the top level and writes to ledger:l-9, where user:bo can read it.
// The model composes that object itself, so the whole cross-write protection
// was bypassable from inside the thing it protects against.
//
// It is refused rather than silently resolved to the top-level value, because
// "the gate guesses correctly" holds only while two unwrap rules in two
// packages stay identical forever. A call naming its destination twice, with
// two different answers, is not one to guess about.
func TestAStrayArgsObjectCannotAimTheGateAtADifferentPool(t *testing.T) {
	var seen []string
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		recordingSubjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"},
		}, &seen))
	in := poolWriteCall("unused", "x")
	in.Tool.Args = json.RawMessage(`{"resource":"ledger:l-9","text":"...","args":{"resource":"dossier:d-1"}}`)

	dec := h.Eval(context.Background(), in)

	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"a call that names its destination twice, differently, must not be allowed")
	assert.NotContains(t, seen, "dossier:d-1",
		"the gate must never judge the pool named by the decoy half")
}

// TestAPriorApprovalDoesNotExemptAPoolWrite: the decision set is keyed by
// (type, id) with no destination in it, so a channel approval was clearing the
// resource for a write into an unrelated pool — and clearing it SILENTLY, with
// no audit record at all.
//
// The grant such an approval writes is an infoleakage_grant, which nothing in
// view_memory consults, so the pool's live audience would not have admitted
// those subjects either. filterApproved was the entire effect. The spec accepts
// the refusal this restores as its stated cost.
func TestAPriorApprovalDoesNotExemptAPoolWrite(t *testing.T) {
	d := poolWriteDeps(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"},
		}))
	d.IsApproved = func(_ context.Context, objType, objID string) (bool, error) {
		return objType == "dossier" && objID == "d-1", nil
	}
	h := hooks.NewInfoLeakAudience(d)

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))

	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"an approval to disclose d-1 to a CHANNEL is not permission to write d-1's data into another resource's pool")
	assert.Contains(t, dec.Reason, "user:bo")
	assert.NotEmpty(t, dec.Audit, "and if it were ever allowed, it must at least be recorded")
}

// TestAWriteWhoseDestinationArgumentIsAbsentIsRefused: a declared write whose
// destination cannot be read is not a write to nowhere. Skipping it would be
// the one shape that walks past this gate untouched.
func TestAWriteWhoseDestinationArgumentIsAbsentIsRefused(t *testing.T) {
	h := poolWriteHook(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{"dossier:d-1": {"user:ann"}}))
	in := poolWriteCall("ledger:l-9", "x")
	in.Tool.Args = json.RawMessage(`{"text":"x"}`)

	dec := h.Eval(context.Background(), in)
	assert.Equal(t, pipeline.Deny, dec.Verdict)
}

// TestAWriteDestinationThatIsNotATypeAndIDIsRefused: "<type>:<id>" is the only
// shape a pool ref takes, and a half of it names no resource to expand.
//
// It follows the leakage MODE, like both of its neighbours — the absent
// argument above it and an unresolvable audience below it. The destination
// string is model-authored and so the likeliest of the three to fire; a hard
// deny here would block tool calls on a cluster running "logging" through a
// rollout, out of a checkpoint that is meant to be transparent.
func TestAWriteDestinationThatIsNotATypeAndIDIsRefused(t *testing.T) {
	for _, bad := range []string{"ledger", "ledger:", ":l-9"} {
		t.Run(bad+": enforcing denies", func(t *testing.T) {
			h := poolWriteHook(t, "enforcing",
				taint("dossier", "d-1", memory.PermissionViewMemory),
				subjects(map[string][]string{"dossier:d-1": {"user:ann"}}))

			dec := h.Eval(context.Background(), poolWriteCall(bad, "x"))
			assert.Equal(t, pipeline.Deny, dec.Verdict)
		})
		t.Run(bad+": logging audits and allows", func(t *testing.T) {
			h := poolWriteHook(t, "logging",
				taint("dossier", "d-1", memory.PermissionViewMemory),
				subjects(map[string][]string{"dossier:d-1": {"user:ann"}}))

			dec := h.Eval(context.Background(), poolWriteCall(bad, "x"))
			assert.Equal(t, pipeline.Allow, dec.Verdict, "logging mode never blocks")
			require.NotEmpty(t, dec.Audit, "but it does record")
		})
	}
}

// TestThePoolGateRunsWhetherOrNotPerDatumIsWired pins the Eval ordering its
// comment insists on, which nothing could previously see: every other fixture
// in this file leaves FineGrained nil, so "run the pool gate only when the
// per-datum gate declined to answer" would keep the whole suite green while
// making the cross-write refusal contingent on an opt-in capability.
//
// Here the per-datum leg is wired and answers benignly — its destination
// audience is fully authorized, so it returns an allow with a no-leak record.
// The pool gate must still refuse.
func TestThePoolGateRunsWhetherOrNotPerDatumIsWired(t *testing.T) {
	d := poolWriteDeps(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {"user:ann", "user:bo"},
		}))
	fg := fineGrainedDeps(true, map[string][]string{})
	fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return []string{"user:ann"}, true, nil // nobody the taint does not already permit
	}
	d.FineGrained = fg
	h := hooks.NewInfoLeakAudience(d)

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))

	assert.Equal(t, pipeline.Deny, dec.Verdict, "the pool gate is not contingent on per-datum tagging")
	assert.Contains(t, dec.Reason, "ledger:l-9", "and it is the POOL gate's refusal, not the per-datum leg's")
}

// TestThePerDatumLegAlsoRefusesRatherThanCardingAPoolDestination closes the
// second door onto the same widening.
//
// The per-datum legs resolve a destination's audience through their own closure
// and used to escalate any leak they found, asking nothing about the
// destination. Approving that card runs LeakageGrantWriter over the TAINT's
// resources — so a yes would widen who may read the resource the session read
// FROM, which is exactly what the coarse refusal exists to prevent.
//
// The fixture makes the two resolvers disagree so the per-datum leg is reached
// at all: the pool's view_memory audience is empty (the coarse gate allows
// vacuously) while DestinationAudience reports a wider set. That is artificial,
// and it is the only way to exercise this leg in isolation — which is the point,
// since the composition that reaches it in the field is one task away.
func TestThePerDatumLegAlsoRefusesRatherThanCardingAPoolDestination(t *testing.T) {
	asks := 0
	d := poolWriteDeps(t, "enforcing",
		taint("dossier", "d-1", memory.PermissionViewMemory),
		subjects(map[string][]string{
			"dossier:d-1": {"user:ann"},
			"ledger:l-9":  {}, // coarse: no audience, no leak, allowed
		}))
	d.BuildApprovalAsk = func(context.Context, []string, []infoleakagetaint.TaintRecord, string) (*pipeline.ApprovalAsk, error) {
		asks++
		return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
	}
	fg := fineGrainedDeps(true, map[string][]string{})
	fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return []string{"user:ann", "user:bo"}, true, nil
	}
	d.FineGrained = fg
	h := hooks.NewInfoLeakAudience(d)

	dec := h.Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))

	assert.Equal(t, pipeline.Deny, dec.Verdict, "a pool destination is refused on this leg too")
	assert.Nil(t, dec.Approval, "no card may be raised for a pool destination, on any leg")
	assert.Zero(t, asks, "the approval builder must not even be consulted")
	assert.Contains(t, dec.Reason, "user:bo")
}

// TestAPoolWriteWithNoTaintReaderWiredIsRefused pins the nil-dep direction.
//
// This gate compares two things: what the session has read, and who can read
// the destination. The destination half already refuses when its subject
// lookup is unwired (poolDestination.Audience: the audience is unknown); the
// taint half used to ALLOW, which reads an unknown as an empty — and an empty
// taint set permits every write. It was the one nil-dep in this leg that
// opened rather than closed, and the asymmetry was not stated anywhere.
//
// Follows the leakage MODE, like its neighbours (the absent argument, the
// malformed destination, the unresolvable audience), so a cluster running
// "logging" through a rollout still does not get a blocked tool call.
func TestAPoolWriteWithNoTaintReaderWiredIsRefused(t *testing.T) {
	for _, mode := range []string{"enforcing", "logging"} {
		t.Run(mode, func(t *testing.T) {
			d := poolWriteDeps(t, mode, nil, subjects(map[string][]string{"ledger:l-9": {"user:bob"}}))
			d.TaintList = nil

			dec := hooks.NewInfoLeakAudience(d).Eval(context.Background(), poolWriteCall("ledger:l-9", "x"))
			if mode == "enforcing" {
				assert.Equal(t, pipeline.Deny, dec.Verdict,
					"an unwired taint reader means what this session has read is UNKNOWN, not empty")
				return
			}
			assert.Equal(t, pipeline.Allow, dec.Verdict, "logging mode never blocks")
			require.NotEmpty(t, dec.Audit, "but it does record")
		})
	}
}
