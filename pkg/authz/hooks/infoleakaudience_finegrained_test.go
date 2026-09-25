package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The spec's worked example, as fixtures: a document readable by tim, fred and
// sam; a Slack channel read by tim, fred and sarah. The payload is wrapped in a
// real nonce'd pt-untrusted envelope, parsed the same way production does.
var (
	documentReaders = []string{"user:tim", "user:fred", "user:sam"}
	channelAudience = []string{"user:tim", "user:fred", "user:sarah"}
	taggedPayload   = toolenvelope.WrapPt("the Q3 revenue figures", "ptnonce", "ptt-doc")
)

func fineGrainedDeps(enabled bool, readers map[string][]string) *hooks.FineGrainedDeps {
	return &hooks.FineGrainedDeps{
		Enabled: func(context.Context) bool { return enabled },
		TagReaders: func(_ context.Context, id string) ([]string, error) {
			r, ok := readers[id]
			if !ok {
				return nil, errors.New("unknown tag " + id)
			}
			return r, nil
		},
		// The audience tests exercise the reader-intersection logic, not content
		// binding, so this adapter returns the parsed region ids without a content
		// store. Content binding has its own test in the runner package.
		ParsePayloadTags: func(_ context.Context, payload string) ([]string, bool) {
			regions, covered := toolenvelope.PtRegions(payload)
			ids := make([]string, len(regions))
			for i, r := range regions {
				ids[i] = r.ID
			}
			return ids, covered
		},
	}
}

func audienceHook(t *testing.T, d hooks.InfoLeakAudienceDeps) *hooks.InfoLeakAudience {
	t.Helper()
	if d.Mode == "" {
		d.Mode = "enforcing"
	}
	if d.TaintList == nil {
		// One taint record so the coarse path has something to gate on;
		// without it evalPreResponse short-circuits before the per-datum
		// branch and these tests would pass for the wrong reason.
		d.TaintList = func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{{ResourceType: "doc", ResourceID: "d1", Permission: "viewer"}}, nil
		}
	}
	if d.ResolveAudience == nil {
		d.ResolveAudience = func(context.Context) ([]string, channelkinds.Capability, error) {
			return channelAudience, channelkinds.Capability(0), nil
		}
	}
	return hooks.NewInfoLeakAudience(d)
}

func preResponse(text string) pipeline.Input {
	return pipeline.Input{Point: pipeline.PreResponse, Response: &pipeline.ResponseInfo{Text: text}}
}

// TestPerDatumReplyLeakAsksApproval is the spec's example at the channel egress:
// sarah reads the channel and is authorized on none of the document, so the reply
// would leak. By the invariant (per-datum no more restrictive than coarse, which
// prompts on a reply leak) this ASKS APPROVAL — naming sarah to the approver —
// rather than hard-denying. With no approval builder wired it fails closed.
func TestPerDatumReplyLeakAsksApproval(t *testing.T) {
	t.Run("approval wired: prompts and names the unauthorized recipient", func(t *testing.T) {
		var gotLeakedTo []string
		h := audienceHook(t, hooks.InfoLeakAudienceDeps{
			FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders}),
			BuildApprovalAsk: func(_ context.Context, leakedTo []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
				gotLeakedTo = leakedTo
				return &pipeline.ApprovalAsk{Kind: "leakage_share", Summary: "review"}, nil
			},
		})
		dec := h.Eval(context.Background(), preResponse(taggedPayload))
		assert.NotEqual(t, pipeline.Deny, dec.Verdict, "a reply leak must prompt, not hard-deny")
		assert.NotNil(t, dec.Approval, "per-datum reply leak requests approval")
		assert.Contains(t, gotLeakedTo, "user:sarah",
			"the approver must be told who is unauthorized, or they cannot tell a misconfigured channel from a sensitive document")
	})
	t.Run("approval not wired: fails closed", func(t *testing.T) {
		h := audienceHook(t, hooks.InfoLeakAudienceDeps{
			FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders}),
		})
		dec := h.Eval(context.Background(), preResponse(taggedPayload))
		assert.Equal(t, pipeline.Deny, dec.Verdict, "no approval wiring ⇒ fail closed")
		assert.Contains(t, dec.Reason, "user:sarah")
	})
}

// TestPerDatumAllowsAReplyTheAudienceIsAuthorizedOn is the positive half. A
// gate that only ever denies would pass the test above.
func TestPerDatumAllowsAReplyTheAudienceIsAuthorizedOn(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{
		ResolveAudience: func(context.Context) ([]string, channelkinds.Capability, error) {
			return []string{"user:tim", "user:fred"}, channelkinds.Capability(0), nil
		},
		FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders}),
	})
	dec := h.Eval(context.Background(), preResponse(taggedPayload))
	assert.NotEqual(t, pipeline.Deny, dec.Verdict,
		"every member of this channel is authorized on the document, so the reply is safe")
}

// TestCapabilityOffFallsThroughToTheCoarsePath is the default-off regression.
//
// With the capability off nothing is minted, so a payload cannot legitimately
// be tag-covered — and the per-datum branch must not run even if it looks
// covered. Asserted by giving the fine-grained path an audience it WOULD
// allow while the coarse path denies: seeing the coarse answer proves which
// branch ran.
func TestCapabilityOffFallsThroughToTheCoarsePath(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{
		LookupSubjects: func(context.Context, string, string) ([]string, error) {
			return documentReaders, nil
		},
		// An audience the per-datum path WOULD allow, so if it ran we would
		// see its marker.
		FineGrained: fineGrainedDeps(false, map[string][]string{"ptt-doc": channelAudience}),
	})
	dec := h.Eval(context.Background(), preResponse(taggedPayload))
	assertNoPerDatumAudit(t, dec,
		"with the capability off the coarse path must decide, whatever it decides")
}

// assertNoPerDatumAudit checks WHICH BRANCH ran rather than what it concluded.
//
// The coarse path's verdict for a leak is an approval request, not a denial,
// so asserting Deny here would be asserting something about the coarse path
// that is not true — and a test that passed for that reason would be pinning a
// misunderstanding. The per-datum branch is identified by its own audit kinds,
// which is the fact these tests actually claim.
func assertNoPerDatumAudit(t *testing.T, dec pipeline.Decision, msg string) {
	t.Helper()
	for _, a := range dec.Audit {
		assert.NotContains(t, a.Kind, "_per_datum", "%s (saw audit %q)", msg, a.Kind)
	}
}

// TestAPartiallyTaggedPayloadFallsThroughToTheCoarsePath is the property the
// whole port rests on: a stripped or partial tag degrades to the conservative
// session-wide comparison, never to no check.
func TestAPartiallyTaggedPayloadFallsThroughToTheCoarsePath(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{
		LookupSubjects: func(context.Context, string, string) ([]string, error) {
			return documentReaders, nil
		},
		// Readers that WOULD allow, so a per-datum answer is distinguishable.
		FineGrained: fineGrainedDeps(true, map[string][]string{"ptt-doc": channelAudience}),
	})
	dec := h.Eval(context.Background(), preResponse(taggedPayload+" and an untagged tail"))
	assertNoPerDatumAudit(t, dec,
		"an uncovered tail must send this to the coarse check — over-blocking, never unchecked")
}

// --- The tool-call checkpoint (spec 1.6) ------------------------------------

func preToolCall(name, args string) pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: name, Args: json.RawMessage(args)},
	}
}

// TestToolCallToAnAuthorizedDestinationIsAllowed is the flow fine-grained
// provenance exists to permit, and the one the coarse model cannot express:
// the same document that may NOT go to the channel may go to a destination
// everyone on it is authorized for.
func TestToolCallToAnAuthorizedDestinationIsAllowed(t *testing.T) {
	fg := fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders})
	fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return []string{"user:tim", "user:fred"}, true, nil
	}
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{FineGrained: fg})

	dec := h.Eval(context.Background(), preToolCall("summarize", taggedPayload))
	assert.NotEqual(t, pipeline.Deny, dec.Verdict,
		"a strictly narrower destination is safe, and blocking it is what the coarse model got wrong")
}

// TestToolCallToAWiderDestinationAsksApproval: the destination is an egress like
// any other, and its audience is whoever can read it. A tagged datum going to a
// destination readable by someone outside its readers (sarah) is a real leak, so
// it ASKS APPROVAL (naming sarah) rather than hard-denying — the invariant. With
// no approval builder wired it fails closed.
func TestToolCallToAWiderDestinationAsksApproval(t *testing.T) {
	newFG := func() *hooks.FineGrainedDeps {
		fg := fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders})
		fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
			return channelAudience, true, nil // includes sarah, who cannot read ptt-doc
		}
		return fg
	}
	t.Run("approval wired: prompts and names the unauthorized recipient", func(t *testing.T) {
		var gotLeakedTo []string
		h := audienceHook(t, hooks.InfoLeakAudienceDeps{
			FineGrained: newFG(),
			BuildApprovalAsk: func(_ context.Context, leakedTo []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
				gotLeakedTo = leakedTo
				return &pipeline.ApprovalAsk{Kind: "leakage_share", Summary: "review"}, nil
			},
		})
		dec := h.Eval(context.Background(), preToolCall("post_to_wiki", taggedPayload))
		assert.NotEqual(t, pipeline.Deny, dec.Verdict, "a wider destination is a leak → prompt, not hard-deny")
		assert.NotNil(t, dec.Approval)
		assert.Contains(t, gotLeakedTo, "user:sarah")
	})
	t.Run("approval not wired: fails closed", func(t *testing.T) {
		h := audienceHook(t, hooks.InfoLeakAudienceDeps{FineGrained: newFG()})
		dec := h.Eval(context.Background(), preToolCall("post_to_wiki", taggedPayload))
		assert.Equal(t, pipeline.Deny, dec.Verdict)
		assert.Contains(t, dec.Reason, "user:sarah")
	})
}

// TestUnresolvedDestinationFollowsTheLeakageMode pins the dial.
//
// An unresolved destination has an UNKNOWN audience, not an empty one — empty
// is vacuously safe, so treating unknown as empty would authorize sending
// tagged data precisely where the destination could not be identified.
//
// But it follows InformationLeakage.Mode rather than always denying, because
// an unmapped ToolReads already answers to that same setting: operators get
// one dial instead of two, and a cluster running "logging" through a rollout
// is not broken by this checkpoint alone.
func TestUnresolvedDestinationFollowsTheLeakageMode(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		wantDeny bool
	}{
		{mode: "enforcing", wantDeny: true},
		{mode: "logging", wantDeny: false},
	} {
		t.Run(tc.mode+": unresolved destination", func(t *testing.T) {
			fg := fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders})
			fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
				return nil, false, nil
			}
			h := audienceHook(t, hooks.InfoLeakAudienceDeps{Mode: tc.mode, FineGrained: fg})

			dec := h.Eval(context.Background(), preToolCall("mystery_tool", taggedPayload))
			if tc.wantDeny {
				assert.Equal(t, pipeline.Deny, dec.Verdict,
					"unknown must not resolve as 'subset of anything' when enforcing")
			} else {
				assert.NotEqual(t, pipeline.Deny, dec.Verdict,
					"logging mode records the event; it does not block the call")
			}
			require.NotEmpty(t, dec.Audit, "either way the unresolved destination must be recorded")
			assert.Equal(t, "tool_call_destination_unresolved", dec.Audit[0].Kind)
		})
	}
}

func TestAnUnresolvableDestinationIsRefusedWhenEnforcing(t *testing.T) {
	fg := fineGrainedDeps(true, map[string][]string{"ptt-doc": documentReaders})
	fg.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return nil, false, nil
	}
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{FineGrained: fg})

	dec := h.Eval(context.Background(), preToolCall("mystery_tool", taggedPayload))
	require.Equal(t, pipeline.Deny, dec.Verdict,
		"unknown must not resolve as 'subset of anything'")
	assert.Contains(t, dec.Reason, "could not be determined")
}

// TestUntaggedEgressArgsHitTheCoarseFloor pins R15 as reconciled by C1: untagged
// data leaving via an EGRESS tool falls to the coarse floor, and an ACTUAL leak
// there ASKS APPROVAL (not a hard deny — the invariant: per-datum is no more
// restrictive than coarse, which prompts on a leak). Approval only fires on a
// real leak; a destination within permitted is a plain allow. A tool with NO
// destination stays exempt. Missing approval wiring fails closed.
func TestUntaggedEgressArgsHitTheCoarseFloor(t *testing.T) {
	// The session read a doc readable by {tim,fred,sam}.
	docTaint := func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
		return []infoleakagetaint.TaintRecord{{ResourceType: "doc", ResourceID: "d1", Permission: "viewer"}}, nil
	}
	docReaders := func(context.Context, string, string) ([]string, error) { return documentReaders, nil }
	approvalBuilder := func(context.Context, []string, []infoleakagetaint.TaintRecord, string) (*pipeline.ApprovalAsk, error) {
		return &pipeline.ApprovalAsk{Kind: "leakage_share", Summary: "review"}, nil
	}
	leakDest := func(context.Context, string, map[string]any) ([]string, bool, error) {
		return []string{"user:sarah"}, true, nil // sarah canNOT read the tainted doc
	}

	// Leak + approval wired → ASK APPROVAL, not deny.
	fgLeak := fineGrainedDeps(true, nil)
	fgLeak.DestinationAudience = leakDest
	hLeak := audienceHook(t, hooks.InfoLeakAudienceDeps{
		FineGrained: fgLeak, TaintList: docTaint, LookupSubjects: docReaders, BuildApprovalAsk: approvalBuilder,
	})
	decLeak := hLeak.Eval(context.Background(), preToolCall("post_to_wiki", `{"body":"the revenue is 4.2M"}`))
	assert.NotEqual(t, pipeline.Deny, decLeak.Verdict, "an actual leak must prompt, not hard-deny")
	assert.NotNil(t, decLeak.Approval, "untagged egress that would leak requests approval")

	// Same leak, approval NOT wired → fail closed (a misconfig, never a silent allow).
	fgLeak2 := fineGrainedDeps(true, nil)
	fgLeak2.DestinationAudience = leakDest
	hNoAppr := audienceHook(t, hooks.InfoLeakAudienceDeps{
		FineGrained: fgLeak2, TaintList: docTaint, LookupSubjects: docReaders,
	})
	assert.Equal(t, pipeline.Deny,
		hNoAppr.Eval(context.Background(), preToolCall("post_to_wiki", `{"body":"x"}`)).Verdict,
		"a leak with no approval wiring fails closed")

	// Destination WITHIN permitted (tim can read the doc) → plain allow, NO prompt.
	fgSafe := fineGrainedDeps(true, nil)
	fgSafe.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return []string{"user:tim"}, true, nil
	}
	hSafe := audienceHook(t, hooks.InfoLeakAudienceDeps{
		FineGrained: fgSafe, TaintList: docTaint, LookupSubjects: docReaders, BuildApprovalAsk: approvalBuilder,
	})
	decSafe := hSafe.Eval(context.Background(), preToolCall("post_to_wiki", `{"body":"x"}`))
	assert.Equal(t, pipeline.Allow, decSafe.Verdict, "destination within permitted: no leak")
	assert.Nil(t, decSafe.Approval, "no leak → no prompt")

	// A tool with NO destination (a read / Stateless meta tool) → exempt.
	fgRead := fineGrainedDeps(true, nil)
	fgRead.DestinationAudience = func(context.Context, string, map[string]any) ([]string, bool, error) {
		return nil, false, nil
	}
	hRead := audienceHook(t, hooks.InfoLeakAudienceDeps{
		FineGrained: fgRead, TaintList: docTaint, LookupSubjects: docReaders,
	})
	decRead := hRead.Eval(context.Background(), preToolCall("grep", `{"pattern":"revenue"}`))
	assert.NotEqual(t, pipeline.Deny, decRead.Verdict, "a tool with no destination carries nothing out — exempt")
}

// TestToolCallCheckpointIsInertWithoutWiring keeps the new Point from changing
// behaviour for every existing session. FineGrained nil is the default.
func TestToolCallCheckpointIsInertWithoutWiring(t *testing.T) {
	h := audienceHook(t, hooks.InfoLeakAudienceDeps{})
	dec := h.Eval(context.Background(), preToolCall("post_to_wiki", taggedPayload))
	assert.Equal(t, pipeline.Decision{}, dec,
		"adding PreToolCall to Points must be inert until a deployment opts in")
}
