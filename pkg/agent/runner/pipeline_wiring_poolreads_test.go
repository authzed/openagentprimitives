package runner

// The memory search tool reads several resources in one call, and this is where
// the runner says so: the plural read declaration is built-in wiring, never a
// CRD field, because reading a result into the resources it came from needs
// that result's format.
//
// These tests drive the REAL declaration against a REAL search result produced
// by the REAL tool, so a change at any of the three — the envelope, the split,
// or the refs — shows up here rather than in a fixture nobody re-derives.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// poolSearcher is the two-method memory seam search_memory runs against.
type poolSearcher struct{ entries []memory.ScoredEntry }

func (p *poolSearcher) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}
func (p *poolSearcher) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{Entries: p.entries}, nil
}

func poolEntry(t *testing.T, objType, objID, id string) memory.ScoredEntry {
	t.Helper()
	sc, err := memory.ResourceScope(objType, objID)
	require.NoError(t, err)
	return memory.ScoredEntry{
		Entry:  memory.Entry{Scope: sc, Kind: "observation", ID: id, Content: json.RawMessage(`{"note":"` + id + `"}`)},
		Score:  1,
		Source: "inmem",
	}
}

func sessionEntry(id string) memory.ScoredEntry {
	return memory.ScoredEntry{
		Entry: memory.Entry{
			Scope:   memory.Scope{Kind: "session", ID: "ns/s1"},
			Kind:    "observation",
			ID:      id,
			Content: json.RawMessage(`{"note":"` + id + `"}`),
		},
		Score:  1,
		Source: "inmem",
	}
}

// realSearchResult runs the real tool over the given entries and returns the
// bytes it hands the model — the same bytes the read hook is given.
func realSearchResult(t *testing.T, entries ...memory.ScoredEntry) string {
	t.Helper()
	sess := &tool.SessionContext{Namespace: "ns", Name: "s1", Mem: &poolSearcher{entries: entries}}
	res, err := meta.NewSearchMemory().Execute(
		memory.WithSystemApproval(context.Background(), "test"),
		json.RawMessage(`{"text":"anything"}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "result: %s", res.Content)
	return res.Content
}

// leakageLoop is a Loop wired only as far as the read declaration needs.
func leakageLoop() *Loop {
	return &Loop{
		SessionKey:    memory.NamespacedName{Namespace: "ns", Name: "s1"},
		LeakageConfig: &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"},
	}
}

// mixedPoolResult: two entries from one pool, one from another, one from the
// session's own scope. Four entries, two pools.
func mixedPoolResult(t *testing.T) string {
	t.Helper()
	return realSearchResult(t,
		poolEntry(t, "customer", "alpha", "obs-a1"),
		sessionEntry("obs-s1"),
		poolEntry(t, "vendor", "beta", "obs-b1"),
		poolEntry(t, "customer", "alpha", "obs-a2"),
	)
}

// readHookOverRealWiring builds the read hook from the real deps, replacing
// only the two recorders. LookupReads — the thing under test — stays real.
func readHookOverRealWiring(t *testing.T) (*hooks.InfoLeakRead, *[]memory.PtTagMintRequest, *[]infoleakagetaint.TaintRecord) {
	t.Helper()
	minted := &[]memory.PtTagMintRequest{}
	tainted := &[]infoleakagetaint.TaintRecord{}
	d := leakageLoop().infoLeakReadDeps()
	d.MintPtTag = func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
		*minted = append(*minted, req)
		return "pt_x", nil
	}
	d.AppendTaint = func(_ context.Context, r infoleakagetaint.TaintRecord) error {
		*tainted = append(*tainted, r)
		return nil
	}
	return hooks.NewInfoLeakRead(d), minted, tainted
}

func searchCall(result string) pipeline.Input {
	return pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "search_memory", UseID: "toolu_s", Result: result},
	}
}

// TestPoolReadsWiring_OneTagPerPoolNotPerEntry is the count assertion the plan
// asks for, over a result where the two differ: four entries, two pools.
//
// A regression to per-entry minting stays functionally CORRECT — every entry
// gets a tag naming its own pool — and quietly costs a SpiceDB subject
// expansion per result row, which is why only a count catches it.
func TestPoolReadsWiring_OneTagPerPoolNotPerEntry(t *testing.T) {
	h, minted, tainted := readHookOverRealWiring(t)

	dec := h.Eval(context.Background(), searchCall(mixedPoolResult(t)))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, *minted, 2, "four entries across two pools is two tags")
	require.Len(t, *tainted, 2, "and two taint records, one per pool")
}

// TestPoolReadsWiring_TwoPoolsAreTwoSeparateMints: each call names exactly one
// resource. One call naming both would derive the INTERSECTION of the two
// audiences, so a reader of one pool could not be handed that pool's own datum
// — and nothing downstream would report it, because under-disclosure looks
// exactly like correct restriction.
func TestPoolReadsWiring_TwoPoolsAreTwoSeparateMints(t *testing.T) {
	h, minted, _ := readHookOverRealWiring(t)

	h.Eval(context.Background(), searchCall(mixedPoolResult(t)))

	require.Len(t, *minted, 2)
	for i, got := range *minted {
		require.Len(t, got.Resources, 1, "mint %d names more than one resource", i)
	}
	assert.Equal(t, "customer", (*minted)[0].Resources[0].Type)
	assert.Equal(t, "alpha", (*minted)[0].Resources[0].ID)
	assert.Equal(t, "vendor", (*minted)[1].Resources[0].Type)
	assert.Equal(t, "beta", (*minted)[1].Resources[0].ID)
}

// TestPoolReadsWiring_TheRefNamesTheAudiencePermission is the ruling that
// decides who a pool's datum may be shown to.
//
// A session reaches a pool through a SLOT GRANT — `(customer, read)`, say — but
// the pool's readers are whoever holds view_memory on the resource. Minting
// under the slot's permission would expand a different subject set, and the
// resulting tag would be neither obviously wrong nor detectably right.
func TestPoolReadsWiring_TheRefNamesTheAudiencePermission(t *testing.T) {
	h, minted, tainted := readHookOverRealWiring(t)

	h.Eval(context.Background(), searchCall(mixedPoolResult(t)))

	require.Len(t, *minted, 2)
	for _, got := range *minted {
		assert.Equal(t, memory.PermissionViewMemory, got.Resources[0].Permission,
			"a pool's audience is view_memory on the resource, never the permission the session reached it through")
	}
	require.Len(t, *tainted, 2)
	for _, rec := range *tainted {
		assert.Equal(t, memory.PermissionViewMemory, rec.Permission)
	}
}

// TestPoolReadsWiring_EachTagCarriesOnlyItsOwnPool: the content behind a tag is
// placed into a child's context when the datum is bound into a data slot, so
// alpha's tag carrying beta's entries would disclose beta to alpha's audience.
// The session's own entries belong to neither and appear in neither.
func TestPoolReadsWiring_EachTagCarriesOnlyItsOwnPool(t *testing.T) {
	h, minted, _ := readHookOverRealWiring(t)

	h.Eval(context.Background(), searchCall(mixedPoolResult(t)))

	require.Len(t, *minted, 2)
	alpha, beta := (*minted)[0].Content, (*minted)[1].Content

	assert.Contains(t, alpha, "obs-a1")
	assert.Contains(t, alpha, "obs-a2", "both of the pool's entries, in one tag")
	assert.NotContains(t, alpha, "obs-b1", "and none of the other pool's")
	assert.NotContains(t, alpha, "obs-s1", "nor the session's own")

	assert.Contains(t, beta, "obs-b1")
	assert.NotContains(t, beta, "obs-a1")
}

// TestPoolReadsWiring_ASessionOnlyResultTagsNothing: the common case. Nothing
// pool-scoped was read, so there is no per-datum audience to record; the
// session-wide floor already covers the session's own entries.
func TestPoolReadsWiring_ASessionOnlyResultTagsNothing(t *testing.T) {
	h, minted, tainted := readHookOverRealWiring(t)

	dec := h.Eval(context.Background(), searchCall(realSearchResult(t, sessionEntry("obs-s1"), sessionEntry("obs-s2"))))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, *minted)
	assert.Empty(t, *tainted)
}

// TestPoolReadsWiring_OnlyTheMemorySearchToolDeclaresPluralReads: the
// declaration is per-tool built-in wiring, not a blanket rule. Every other tool
// keeps the single-resource path — for an undeclared one, that is the coarse
// floor.
func TestPoolReadsWiring_OnlyTheMemorySearchToolDeclaresPluralReads(t *testing.T) {
	d := leakageLoop().infoLeakReadDeps()

	require.NotNil(t, d.LookupReads("search_memory"))
	assert.NotNil(t, d.LookupReads("search_memory").ResultResources)
	assert.Nil(t, d.LookupReads("query_memory"), "query_memory does not span pools; see its description")
	assert.Nil(t, d.LookupReads("get_issue"))
}

// TestPoolReadsWiring_TheAudienceHookSeesTheSamePools: the two PostToolCall
// hooks must agree about what a call read. If only the read hook learned the
// plural declaration, the same-turn gate would be blind to exactly the tool the
// read hook is tagging.
func TestPoolReadsWiring_TheAudienceHookSeesTheSamePools(t *testing.T) {
	d := leakageLoop().infoLeakAudienceDeps()

	decl := d.LookupReads("search_memory")
	require.NotNil(t, decl)
	require.NotNil(t, decl.ResultResources)

	refs, err := decl.ResultResources(mixedPoolResult(t))
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "customer", refs[0].Type)
	assert.Equal(t, "vendor", refs[1].Type)
}

// TestPoolReadsWiring_ScopeNarrowingStaysSingleResource pins the ruling that
// the Scope hook's lookup is unchanged: a plural decl would reach CheckScope
// with no resource type to narrow on.
func TestPoolReadsWiring_ScopeNarrowingStaysSingleResource(t *testing.T) {
	d := leakageLoop().scopeDeps()

	assert.Nil(t, d.LookupReads("search_memory"),
		"the Scope hook keeps the CRD-declared single-resource view")
}

// Declaring a tool takes away its coarse floor. Before the declaration existed,
// search_memory had no CRD mapping, so the read hook floored every result it
// produced: an agentsession#unknown_provenance taint whose audience is the
// session's participants. The declaration replaces that blanket answer with a
// precise one — and a precise answer must therefore cover EVERYTHING the result
// can contain, not only the two shapes we expect.
//
// Exactly one shape is safely skippable: this session's own scope, whose
// audience IS the session. Anything else — another session's entries (the
// in-process `scopes` argument still reaches the searcher), a scope claiming to
// be a pool whose id will not parse, an entry a provider left with no scope at
// all — is a result the declaration cannot attribute. Dropping those silently
// is worse than the floor it replaced: no taint, no tag, no audit, nothing.
//
// Table-driven because the cases differ only in the scope on one entry, which
// is precisely the parameter under test.
func TestPoolReadsWiring_UnattributableScopesAreRefusedNotDropped(t *testing.T) {
	cases := []struct {
		name  string
		scope memory.Scope
	}{
		{
			name:  "another session's scope: refused, because its audience is not this session's",
			scope: memory.Scope{Kind: "session", ID: "ns/other-session"},
		},
		{
			name:  "a scope claiming to be a pool whose id does not parse: refused, not treated as 'not a pool'",
			scope: memory.Scope{Kind: memory.ScopeKindResource, ID: "no-colon-here"},
		},
		{
			name:  "no scope at all (a provider that left Entry.Scope zero): refused, never read as 'the session's own'",
			scope: memory.Scope{},
		},
		{
			name:  "some other scope Kind: refused rather than assumed harmless",
			scope: memory.Scope{Kind: "user", ID: "someone@example.test"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := realSearchResult(t,
				poolEntry(t, "customer", "alpha", "obs-a1"),
				memory.ScoredEntry{
					Entry:  memory.Entry{Scope: tc.scope, Kind: "observation", ID: "obs-x"},
					Score:  1,
					Source: "inmem",
				},
			)

			h, minted, tainted := readHookOverRealWiring(t)
			dec := h.Eval(context.Background(), searchCall(result))

			assert.Equal(t, pipeline.Deny, dec.Verdict,
				"enforcing withholds a result whose provenance the declaration cannot establish")
			assert.Contains(t, dec.Reason, "could not be attributed")
			assert.Empty(t, *minted, "and nothing is tagged from a result we cannot account for")
			assert.Empty(t, *tainted)
		})
	}
}

// The logging-mode half of the same rule: nothing is withheld, and the failure
// is RECORDED. host.go persists this kind (see
// TestRunnerHost_Audit_PersistsTheAttributionFailureKinds); a silent drop here
// would leave logging mode blind to the one event it is turned on to see.
func TestPoolReadsWiring_AnUnattributableScopeIsAuditedInLoggingMode(t *testing.T) {
	result := realSearchResult(t,
		memory.ScoredEntry{
			Entry:  memory.Entry{Scope: memory.Scope{Kind: "session", ID: "ns/other-session"}, Kind: "observation", ID: "obs-x"},
			Score:  1,
			Source: "inmem",
		},
	)

	l := leakageLoop()
	l.LeakageConfig = &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "logging"}
	d := l.infoLeakReadDeps()
	var minted []memory.PtTagMintRequest
	d.MintPtTag = func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
		minted = append(minted, req)
		return "pt_x", nil
	}

	dec := hooks.NewInfoLeakRead(d).Eval(context.Background(), searchCall(result))

	assert.Equal(t, pipeline.Allow, dec.Verdict, "logging mode changes nothing")
	assert.Empty(t, minted)
	require.Len(t, dec.Audit, 1)
	assert.Equal(t, "unattributable_result", dec.Audit[0].Kind)
}

// TestPoolReadsWiring_ARefusalSurvivesAnUpstreamIsError is the join between the
// declaration and the hook, and it is why the refusal is marked rather than
// merely returned.
//
// The hook excuses a resolver error when the call FAILED, because a failed
// call's result is usually not the tool's envelope at all. `isError` is written
// by the UPSTREAM, though, so that exemption must not stretch to cover a result
// that parsed fine and named a scope the declaration refused — which it would,
// silently, if the two errors were indistinguishable. Asserted end to end
// through the real declaration, because a sentinel the wiring forgets to wrap
// is a sentinel that does nothing.
func TestPoolReadsWiring_ARefusalSurvivesAnUpstreamIsError(t *testing.T) {
	result := realSearchResult(t,
		poolEntry(t, "customer", "alpha", "obs-a1"),
		memory.ScoredEntry{
			Entry:  memory.Entry{Scope: memory.Scope{Kind: "session", ID: "ns/other-session"}, Kind: "observation", ID: "obs-x"},
			Score:  1,
			Source: "inmem",
		},
	)

	h, minted, tainted := readHookOverRealWiring(t)
	in := searchCall(result)
	in.Tool.IsError = true // the upstream says this call failed; the bytes say otherwise

	dec := h.Eval(context.Background(), in)

	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"a gate whose off-switch is a flag the other side writes is not a gate")
	assert.Contains(t, dec.Reason, "could not be attributed")
	assert.Empty(t, *minted)
	assert.Empty(t, *tainted)
}

// And the exemption still works for what it is FOR: search_memory's own error
// results are plain sentences, not envelopes, and a failed call carrying one is
// nothing to gate rather than a broken declaration.
func TestPoolReadsWiring_AFailedCallWithNoEnvelopeIsStillNothingToGate(t *testing.T) {
	h, minted, tainted := readHookOverRealWiring(t)
	in := searchCall("search_memory: no search providers configured; use query_memory for structured queries")
	in.Tool.IsError = true

	dec := h.Eval(context.Background(), in)

	assert.Equal(t, pipeline.Allow, dec.Verdict, "an unparseable result on a failed call names nothing")
	assert.Empty(t, *minted)
	assert.Empty(t, *tainted)
}

// The session's own scope is the ONE skippable shape, and it must stay
// skippable: the declaration is asked for it on every single search.
func TestPoolReadsWiring_ThisSessionsOwnScopeIsTheOneSkippableShape(t *testing.T) {
	d := leakageLoop().infoLeakReadDeps()

	refs, err := d.LookupReads("search_memory").ResultResources(
		realSearchResult(t, sessionEntry("obs-s1"), poolEntry(t, "customer", "alpha", "obs-a1")))

	require.NoError(t, err, "this session's own entries alongside a pool's are attributable")
	require.Len(t, refs, 1)
	assert.Equal(t, "customer", refs[0].Type)
}
