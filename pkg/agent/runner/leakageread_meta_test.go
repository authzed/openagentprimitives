package runner

// The read-side gate on the MODEL's own memory search.
//
// pipeline_wiring_poolreads_test.go drives the declaration by calling the hook
// directly, which proves the declaration is right and proves nothing about
// whether anything ever calls it. These tests go through dispatchToolUses — the
// path the model's tool_use actually takes — because that is where the gap was:
// search_memory is a Stateless meta tool, so the containment pipeline skips it,
// and every hook the declaration exists for was unreachable on the one call
// that matters.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// readGateRecorder captures everything the read-side hooks record. Mutex-guarded
// because dispatchToolUses runs each tool_use on its own goroutine.
type readGateRecorder struct {
	mu      sync.Mutex
	minted  []memory.PtTagMintRequest
	tainted []infoleakagetaint.TaintRecord
	audited []infoleakageaudit.AuditRecord
	// asked is every subject-set the audience gate expanded, as
	// "<type>:<id>#<permission>" — the audience it computed for each resource
	// the result was attributed to.
	asked []string
}

func (r *readGateRecorder) mints() []memory.PtTagMintRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]memory.PtTagMintRequest(nil), r.minted...)
}

func (r *readGateRecorder) taints() []infoleakagetaint.TaintRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]infoleakagetaint.TaintRecord(nil), r.tainted...)
}

func (r *readGateRecorder) audienceLookups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// auditKinds is what the audit sink was asked to persist, as kinds — the record
// each hook leaves behind when it has no verdict to return.
func (r *readGateRecorder) auditKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.audited))
	for _, a := range r.audited {
		out = append(out, a.Kind)
	}
	return out
}

// readGateLoop builds a dispatch-ready Loop with the leakage wiring the runner
// binary sets, the recorders standing in for the three durable sinks, and a
// lifecycle log so the deny bookkeeping is observable.
func readGateLoop(t *testing.T, mode string, tools ...tool.Tool) (*Loop, *readGateRecorder) {
	t.Helper()
	rec := &readGateRecorder{}
	l := &Loop{
		Tools:         tools,
		SessionKey:    memory.NamespacedName{Namespace: "ns", Name: "s1"},
		LeakageConfig: &spiceboxv1alpha1.InformationLeakagePolicy{Mode: mode},
		ToolAuthMode:  ToolAuthModeDisabled,
	}
	l.PtTagMint = func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.minted = append(rec.minted, req)
		return "pt_x", nil
	}
	l.TaintMemoryAppend = func(_ context.Context, r infoleakagetaint.TaintRecord) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.tainted = append(rec.tainted, r)
		return nil
	}
	l.AuditMemoryAppend = func(_ context.Context, r infoleakageaudit.AuditRecord) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.audited = append(rec.audited, r)
		return nil
	}
	// The audience half. No ChannelKindImpl, so the resolved audience is empty
	// and nothing can be leaked to — which is what makes this fixture about
	// WHETHER the gate ran and over WHICH resources, not about its verdict.
	l.SpiceDBLookupSubjects = func(_ context.Context, resource, permission string) ([]string, error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.asked = append(rec.asked, resource+"#"+permission)
		return []string{"user:reader"}, nil
	}
	return l, rec
}

// dispatchSearch dispatches one search_memory tool_use through the real
// dispatch path, over a session whose memory seam returns the given entries —
// so the bytes the hooks see are the bytes the real tool produced.
func dispatchSearch(t *testing.T, l *Loop, entries ...memory.ScoredEntry) tool.Result {
	t.Helper()
	sess := &tool.SessionContext{
		Namespace: l.SessionKey.Namespace,
		Name:      l.SessionKey.Name,
		Mem:       &poolSearcher{entries: entries},
	}
	uses := []llm.ToolUseBlock{{
		ID:    "toolu_s",
		Name:  meta.SearchMemoryToolName,
		Input: json.RawMessage(`{"text":"anything"}`),
	}}
	results, _ := l.dispatchToolUses(
		memory.WithSystemApproval(context.Background(), "test"), uses, sess, 0, 0, nil, nil)
	require.Len(t, results, 1, "one tool_use → one result")
	return results[0]
}

// TestMetaReadGate_TheModelsOwnSearchTagsEveryPool is the regression the
// declaration's own tests cannot catch: they call the hook, and nothing called
// the hook. Four entries across two pools, dispatched the way a model dispatches
// it, must leave one taint and one tag per POOL.
func TestMetaReadGate_TheModelsOwnSearchTagsEveryPool(t *testing.T) {
	l, rec := readGateLoop(t, "enforcing", meta.NewSearchMemory())

	res := dispatchSearch(t,
		l,
		poolEntry(t, "customer", "alpha", "obs-a1"),
		sessionEntry("obs-s1"),
		poolEntry(t, "vendor", "beta", "obs-b1"),
		poolEntry(t, "customer", "alpha", "obs-a2"),
	)

	assert.False(t, res.IsError, "a tagged result is still delivered: %s", res.Content)
	assert.Contains(t, res.Content, "obs-a1", "the model still receives the entries")

	minted := rec.mints()
	require.Len(t, minted, 2, "four entries across two pools is two tags")
	require.Len(t, minted[0].Resources, 1)
	assert.Equal(t, "customer", minted[0].Resources[0].Type)
	assert.Equal(t, "vendor", minted[1].Resources[0].Type)

	tainted := rec.taints()
	require.Len(t, tainted, 2, "and one taint record per pool")
	for _, tr := range tainted {
		assert.Equal(t, memory.PermissionViewMemory, tr.Permission,
			"a pool's audience is view_memory on the resource")
		assert.Equal(t, "toolu_s", tr.ToolUseID, "the taint names the call that read it")
	}

	// Both hooks ran, and each left its own record: read_unchecked is
	// info_leak_read's, respond_no_leak is info_leak_audience's same-turn gate
	// finding nobody outside the pools' readers in this session's audience.
	assert.Contains(t, rec.auditKinds(), "read_unchecked")
	assert.Contains(t, rec.auditKinds(), "respond_no_leak",
		"the audience gate must see the same call, not just the read gate")
	assert.Equal(t, []string{"customer:alpha#view_memory", "vendor:beta#view_memory"},
		rec.audienceLookups(),
		"and it must gate EVERY pool the read hook tagged, each under that pool's own audience")
}

// TestMetaReadGate_ASessionOnlySearchIsUnchanged is the overwhelmingly common
// case and the one a regression would hide in: no pools, so nothing to attribute
// and nothing to record. The result must be byte-identical to what the tool
// produced, with the same ABSENCE of records the path had before this gate
// existed.
func TestMetaReadGate_ASessionOnlySearchIsUnchanged(t *testing.T) {
	l, rec := readGateLoop(t, "enforcing", meta.NewSearchMemory())

	res := dispatchSearch(t, l, sessionEntry("obs-s1"), sessionEntry("obs-s2"))

	assert.False(t, res.IsError)
	assert.Equal(t, realSearchResult(t, sessionEntry("obs-s1"), sessionEntry("obs-s2")), res.Content,
		"a session-only search delivers exactly the tool's own bytes")
	assert.Empty(t, rec.mints(), "the session's own entries have no per-datum audience to mint")
	assert.Empty(t, rec.taints())
	assert.Empty(t, rec.auditKinds(), "and nothing is audited about a call that read no pool")
	assert.Empty(t, rec.audienceLookups(), "nor is any audience expanded for one")
}

// TestMetaReadGate_AnUndeclaredMetaToolIsUntouched: the pass is keyed to the
// resolved declaration, so every other meta tool keeps the path it has today —
// no hooks, no records, and in particular NOT the coarse unknown_provenance
// floor a non-meta undeclared tool gets.
func TestMetaReadGate_AnUndeclaredMetaToolIsUntouched(t *testing.T) {
	mt := &countingMetaTool{name: "recall_history", content: "third-party text"}
	l, rec := readGateLoop(t, "enforcing", mt)

	res := dispatchMeta(t, l, "recall_history", "tu-1", 0)

	assert.Equal(t, "third-party text", res.Content)
	assert.False(t, res.IsError)
	assert.Equal(t, 1, mt.executed())
	assert.Empty(t, rec.mints())
	assert.Empty(t, rec.taints())
	assert.Empty(t, rec.auditKinds())
	assert.Empty(t, rec.audienceLookups())
}

// TestMetaReadGate_APostDenyWithholdsTheResult: a refusal that hands back the
// data anyway is not a gate. An entry whose scope the declaration cannot
// attribute denies in enforcing mode; the model must get the refusal, never the
// pool entries that came with it — and the deny must land in the same dispatch
// bookkeeping the gated path uses, which is observable as the HookDeny{Post}
// the loop records.
func TestMetaReadGate_APostDenyWithholdsTheResult(t *testing.T) {
	l, rec := readGateLoop(t, "enforcing", meta.NewSearchMemory())
	lifeMem := buildLifecycleMemory()
	l.LifecycleMemory = lifeMem
	appendLifecycleEvent(t, lifeMem, l.SessionKey, lifecyclecore.RunnerClaimed{})

	res := dispatchSearch(t,
		l,
		poolEntry(t, "customer", "alpha", "obs-a1"),
		memory.ScoredEntry{
			Entry: memory.Entry{
				Scope: memory.Scope{Kind: "session", ID: "ns/other-session"},
				Kind:  "observation", ID: "obs-x",
			},
			Score: 1, Source: "inmem",
		},
	)

	assert.True(t, res.IsError, "an unattributable result is refused")
	assert.Contains(t, res.Content, "could not be attributed")
	assert.NotContains(t, res.Content, "obs-a1",
		"the withheld result must not carry the entries it was refused for")
	assert.Empty(t, rec.mints(), "nothing is tagged out of a result we cannot account for")
	assert.Empty(t, rec.taints())

	ctx := memory.WithSystemApproval(context.Background(), "test")
	events, err := lifecycle.Events(ctx, lifeMem,
		memory.Scope{Kind: "session", ID: l.SessionKey.Namespace + "/" + l.SessionKey.Name})
	require.NoError(t, err)
	var postDenies int
	for _, ev := range events {
		if hd, ok := ev.(lifecyclecore.HookDeny); ok && hd.Post {
			postDenies++
		}
	}
	assert.Equal(t, 1, postDenies,
		"the refusal must count as a post-deny, exactly as the gated path's does")
}

// TestMetaReadGate_ModeIsRespected: an operator who set logging mode did so
// expecting nothing to change, and one who set disabled expects the gate not to
// run at all. Both are the hooks' own rules — this asserts they survive the trip
// through the new path rather than being re-decided by it.
func TestMetaReadGate_ModeIsRespected(t *testing.T) {
	unattributable := []memory.ScoredEntry{
		poolEntry(t, "customer", "alpha", "obs-a1"),
		{
			Entry: memory.Entry{
				Scope: memory.Scope{Kind: "session", ID: "ns/other-session"},
				Kind:  "observation", ID: "obs-x",
			},
			Score: 1, Source: "inmem",
		},
	}

	t.Run("logging: the result is delivered intact and the refusal is recorded", func(t *testing.T) {
		l, rec := readGateLoop(t, "logging", meta.NewSearchMemory())

		res := dispatchSearch(t, l, unattributable...)

		assert.False(t, res.IsError, "logging mode changes nothing")
		assert.Contains(t, res.Content, "obs-a1")
		assert.Contains(t, rec.auditKinds(), "unattributable_result",
			"logging mode's whole point is that the event is visible")
	})

	t.Run("disabled: the gate does not run", func(t *testing.T) {
		l, rec := readGateLoop(t, "disabled", meta.NewSearchMemory())

		res := dispatchSearch(t, l, unattributable...)

		assert.False(t, res.IsError)
		assert.Contains(t, res.Content, "obs-a1")
		assert.Empty(t, rec.auditKinds())
		assert.Empty(t, rec.mints())
	})
}

// TestMetaReadGate_ANoPoolSessionReadingAnotherScopeIsRefused pins a case that
// is deliberate, easy to mistake for a bug, and reachable ONLY in process.
//
// A session with no pools at all whose result carries an entry from some other
// scope loses the WHOLE result in enforcing mode. That is the declaration's
// ruling — a result whose provenance cannot be established must not reach the
// model — and this task is what makes it reachable on the model's own call, so
// it is pinned here rather than left as a side effect of the pool cases.
//
// It cannot happen over HTTP: handleSearch FORCES the scope list to the URL's
// session plus the pools the session provably holds a slot grant on, discarding
// the request body's `scopes` entirely (pkg/memory/httpsrv/httpsrv.go), and the
// tool's own schema says to leave the argument unset. An IN-PROCESS searcher
// honors it — which is exactly what the e2e harness has — so a scenario author
// who hands one a foreign-scoped entry will meet this refusal.
func TestMetaReadGate_ANoPoolSessionReadingAnotherScopeIsRefused(t *testing.T) {
	foreign := memory.ScoredEntry{
		Entry: memory.Entry{
			Scope: memory.Scope{Kind: "global", ID: "x"},
			Kind:  "observation", ID: "obs-g",
		},
		Score: 1, Source: "inmem",
	}

	t.Run("enforcing: the whole result is withheld, pools or no pools", func(t *testing.T) {
		l, rec := readGateLoop(t, "enforcing", meta.NewSearchMemory())

		res := dispatchSearch(t, l, sessionEntry("obs-s1"), foreign)

		assert.True(t, res.IsError)
		assert.Contains(t, res.Content, "could not be attributed")
		assert.NotContains(t, res.Content, "obs-s1",
			"the session's own entries go with it: the verdict is per RESULT, not per entry")
		assert.Empty(t, rec.mints())
	})

	t.Run("logging: delivered intact, and the refusal is recorded", func(t *testing.T) {
		l, rec := readGateLoop(t, "logging", meta.NewSearchMemory())

		res := dispatchSearch(t, l, sessionEntry("obs-s1"), foreign)

		assert.False(t, res.IsError)
		assert.Contains(t, res.Content, "obs-s1")
		assert.Contains(t, rec.auditKinds(), "unattributable_result")
	})
}

// TestMetaReadGate_PredicateIsTheResolvedDeclaration pins WHAT qualifies a call
// for this pass. The predicate asks the same lookup the hook will ask, so the
// two cannot come to different answers about whether there is anything to
// attribute; a hand-maintained tool-name list here is exactly the drift that
// would leave a future plural declaration unreachable on the model's own call,
// the same way this one was.
func TestMetaReadGate_PredicateIsTheResolvedDeclaration(t *testing.T) {
	l, _ := readGateLoop(t, "enforcing", meta.NewSearchMemory())

	for _, name := range []string{
		meta.SearchMemoryToolName,
		"query_memory",
		"respond_to_user",
		"get_issue",
		"",
	} {
		decl := l.infoLeakReadDeps().LookupReads(name)
		want := decl != nil && decl.ResultResources != nil
		assert.Equal(t, want, l.metaResultReadsApplies(name),
			"the predicate must agree with the resolved declaration for %q", name)
	}

	assert.True(t, l.metaResultReadsApplies(meta.SearchMemoryToolName),
		"the memory search tool is the one tool whose result spans resources")
	assert.False(t, l.metaResultReadsApplies("query_memory"),
		"query_memory does not span pools; see its description")
}

// TestMetaReadGate_TheDenyIsAContainedPostDeny checks the phase itself, which is
// what the dispatcher's counters and lifecycle mapping switch on. A refusal
// reported as any other phase would be a refusal nothing counts.
func TestMetaReadGate_TheDenyIsAContainedPostDeny(t *testing.T) {
	l, _ := readGateLoop(t, "enforcing", meta.NewSearchMemory())
	sess := &tool.SessionContext{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}
	result := realSearchResult(t,
		memory.ScoredEntry{
			Entry: memory.Entry{
				Scope: memory.Scope{Kind: "session", ID: "ns/other-session"},
				Kind:  "observation", ID: "obs-x",
			},
			Score: 1, Source: "inmem",
		},
	)

	res, oc := l.readSidePostForMetaTool(
		memory.WithSystemApproval(context.Background(), "test"), sess,
		meta.SearchMemoryToolName, json.RawMessage(`{"text":"anything"}`), "toolu_s",
		tool.Result{Content: result}, containedOutcome{Phase: containRanOK})

	assert.Equal(t, containPostDeny, oc.Phase)
	assert.True(t, res.IsError)
	assert.Contains(t, oc.Reason, "could not be attributed")
}

// TestMetaReadGate_AnEarlierGatesVerdictStands: the executor short-circuits on a
// Deny, so a toolguard refusal at Post ends the Post leg there. Running the read
// hooks over a result toolguard already replaced would attribute the refusal
// text instead of the data — and, in enforcing mode, refuse it a second time for
// a reason that has nothing to do with what was read.
func TestMetaReadGate_AnEarlierGatesVerdictStands(t *testing.T) {
	l, rec := readGateLoop(t, "enforcing", meta.NewSearchMemory())
	sess := &tool.SessionContext{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}
	denied := tool.Result{Content: "withheld: ingress budget exhausted", IsError: true}

	res, oc := l.readSidePostForMetaTool(
		memory.WithSystemApproval(context.Background(), "test"), sess,
		meta.SearchMemoryToolName, json.RawMessage(`{"text":"anything"}`), "toolu_s",
		denied, containedOutcome{Phase: containPostDeny, Reason: "ingress budget exhausted"})

	assert.Equal(t, containPostDeny, oc.Phase)
	assert.Equal(t, "ingress budget exhausted", oc.Reason, "the earlier verdict is not restated")
	assert.Equal(t, denied, res)
	assert.Empty(t, rec.auditKinds(), "and the read hooks never saw the call")
}

// TestMetaReadGate_OnlyABuiltInDeclarationCanCarryResultResources pins the
// invariant metaResultReadsApplies rests on: a declaration built from a CRD
// ToolResourceMapping never reports plural, result-derived resources.
//
// It is what lets the predicate ask the built-in declaration alone instead of
// the full lookup chain — which matters twice over. ResultResources needs the
// result's FORMAT to split it, so a spec author who could set it would be
// deciding the audience of their own tool's output (the reason it is built-in
// at all); and the CRD half of that chain costs a live MCPServer /
// SidecarToolbox GET per call in the runner. If this test ever fails, the
// predicate is the thing to revisit, not the test.
//
// Table-driven over the mapping shapes the closures branch on, since that
// branching is exactly what is being pinned.
func TestMetaReadGate_OnlyABuiltInDeclarationCanCarryResultResources(t *testing.T) {
	cases := []struct {
		name    string
		mapping *spiceboxv1alpha1.ToolResourceMapping
	}{
		{
			name: "a reads declaration naming a single resource by arg",
			mapping: &spiceboxv1alpha1.ToolResourceMapping{
				Tool:  "get_issue",
				Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "issue", Permission: "view", IDArg: "id"},
			},
		},
		{
			name: "one naming it by a result field",
			mapping: &spiceboxv1alpha1.ToolResourceMapping{
				Tool:  "get_issue",
				Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "issue", Permission: "view", ResultIDField: "id"},
			},
		},
		{
			name: "one that bypasses the requester check",
			mapping: &spiceboxv1alpha1.ToolResourceMapping{
				Tool: "get_issue",
				Reads: &spiceboxv1alpha1.ToolReads{
					ResourceType: "issue", Permission: "view", IDArg: "id",
					BypassRequesterCheck: ptr.To(true),
				},
			},
		},
		{
			name:    "a NoTaint opt-out",
			mapping: &spiceboxv1alpha1.ToolResourceMapping{Tool: "get_issue", NoTaint: true},
		},
		{
			name:    "a mapping with no reads block at all",
			mapping: &spiceboxv1alpha1.ToolResourceMapping{Tool: "get_issue"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := readGateLoop(t, "enforcing")
			l.LookupToolMapping = func(string) *spiceboxv1alpha1.ToolResourceMapping { return tc.mapping }

			// Both read-side closures, because both dispatch on ResultResources
			// and a plural declaration reaching either one is a plural
			// declaration reaching the model's own call.
			for who, decl := range map[string]*hooks.ToolReadsDecl{
				"info_leak_read":     l.infoLeakReadDeps().LookupReads("get_issue"),
				"info_leak_audience": l.infoLeakAudienceDeps().LookupReads("get_issue"),
			} {
				if decl == nil {
					continue // an undeclared tool takes the coarse floor; nothing plural about it
				}
				assert.Nil(t, decl.ResultResources,
					"%s: a CRD mapping must not be able to declare plural result-derived resources", who)
			}

			assert.False(t, l.metaResultReadsApplies("get_issue"),
				"and so a CRD-mapped tool never qualifies for the ungated read-side pass")
		})
	}
}

// TestMetaReadGate_TheCommonMetaCallCostsNoToolMappingLookup pins the cost side
// of the same decision. LookupToolMapping is a live client.Get per MCPServer and
// per SidecarToolbox ref on an uncached client, and the ungated meta path is
// every respond_to_user, update_status and agent_work_complete a session makes.
// Routing the predicate through the full lookup chain charged all of them that
// round trip, in every leakage mode including the default (disabled).
func TestMetaReadGate_TheCommonMetaCallCostsNoToolMappingLookup(t *testing.T) {
	for _, mode := range []string{"disabled", "enforcing"} {
		t.Run("mode="+mode, func(t *testing.T) {
			mt := &countingMetaTool{name: "recall_history", content: "third-party text"}
			l, _ := readGateLoop(t, mode, mt)
			var lookups int
			l.LookupToolMapping = func(string) *spiceboxv1alpha1.ToolResourceMapping {
				lookups++
				return nil
			}

			res := dispatchMeta(t, l, "recall_history", "tu-1", 0)

			assert.Equal(t, "third-party text", res.Content)
			assert.Zero(t, lookups,
				"an ungated meta call must not consult the CRD tool mapping to learn it has no plural declaration")
		})
	}
}

// TestMetaReadGate_TheReadSideSetIsExactlyTheTwoReadSideHooks pins the one
// hand-maintained list on this path.
//
// The selection cannot be derived from the order values: a future hook ordered
// after info_leak_read would be swept in by a numeric rule, and "runs on the
// model's memory search" is precisely the judgement this file's ruling reserves
// — scope's Post leg sorts BEFORE these two and is deliberately excluded. So the
// set is named, and this is what keeps a named set honest: a rename or removal
// fails here, and a newly ADDED PostToolCall hook fails
// TestBuildPipelineRegistry_FullConfig_OrderPreserved, which points back at this
// decision.
func TestMetaReadGate_TheReadSideSetIsExactlyTheTwoReadSideHooks(t *testing.T) {
	l := &Loop{}
	l.ToolAuthMode = "enforcing"
	enableScopeAndLeakageForTest(t, l)
	reg := l.buildPipelineRegistry()

	for name := range readSidePostHooks {
		assert.Contains(t, hookNamesAt(reg, pipeline.PostToolCall), name,
			"%q is named by the ungated read-side pass but is no longer a registered PostToolCall hook", name)
	}
	assert.Equal(t,
		[]string{"info_leak_read", "info_leak_audience"},
		hookNamesAt(readSidePostRegistry(reg), pipeline.PostToolCall),
		"the ungated meta path runs the read-side hooks and nothing else — in the gated path's own order")
}
