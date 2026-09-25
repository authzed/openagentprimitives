package meta_test

// The determinism property over meta-tool results: IDENTICAL INPUTS MUST
// ENCODE TO IDENTICAL BYTES.
//
// # Why this file exists
//
// Twice now, one change apart, a meta tool has handed the model a collection
// built by ranging a Go map. Go randomizes map iteration, so two identical
// operations produced the same SET in a different SEQUENCE:
//
//   - the html/css sanitizer's warnings, in artifact_prepare's result and on
//     ArtifactRender.status.warnings;
//   - a revision's tag list, in artifact_prepare / artifact_await / artifact_history.
//
// Neither was visible to any test in the repo, and neither was visible to a
// single run of anything: the set was always right. Both were found by
// capturing a real session and REPLAYING it, which is an expensive and lucky
// way to learn that a pure function is not a function. The second one was
// predicted in writing after the first — "another unordered set in a meta
// result would need the same two-sided fix with nothing to flag it" — and
// nothing flagged it.
//
// So the guard is the property itself rather than a sort applied to the two
// fields that happened to bite. A scenario runs its tool determinismTrials
// times against unchanging state and requires every encoded result to be
// byte-identical. That is exactly the evidence a replay produced, available in
// a unit test, without a capture and without luck.
//
// It has already earned its keep: search_memory flattened MergedSearchResult.
// PerProvider — a map — into the model-facing `dropped_filters`, which is the
// same defect in a third tool, in a family nobody had looked at.
//
// # What a scenario must prove
//
// Stability alone is trivially satisfiable: a tool that returns a constant
// error passes while asserting nothing about the class. So every scenario also
// has to show it REACHED a multi-element collection, via nonVacuous. Most
// scenarios NAME the fields they are about (requireNamedArraysOfAtLeastTwo),
// because a result carrying several collections satisfies a generic
// "some array has two elements" check even after the one that mattered has
// quietly gone empty — which is exactly what happened when this gate was first
// written and mutation-tested. A scenario whose fixture stops producing its
// collection now fails as loudly as one that goes nondeterministic.
//
// # What it does NOT catch
//
// A collection emitted in a stable but WRONG order. The property here is
// "twice the same", not "in the defined order": if a producer emitted from a
// fixed slice in an order nobody wanted, every run would agree and this file
// would pass. Which order is right is a claim, and claims live with their
// producer — channelassets' warning-order tests, artifacts' tag-order tests,
// search_memory's provider-name test. This file is the net under all of them.
//
// # Why the coverage list is derived
//
// TestMetaToolDeterminism_ClassifiesEveryMetaTool walks every meta tool from
// allMetaTools — itself checked against the package's own source by
// TestMetaTools_EveryConstructorIsSwept — and requires each to be either
// scenario-covered or listed in determinismUncovered with a reason. A tool
// added later cannot slip past by being forgotten; someone has to look at its
// result and say which it is. That is the reminder the last two bugs did not
// have.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// determinismTrials is how many times a scenario is run. A two-element
// collection emitted in map order agrees with itself half the time, so one
// extra run is not enough to distinguish "stable" from "lucky": at 20 runs a
// nondeterministic pair slips through with probability 2^-19, about two in a
// million. The whole file costs well under a second.
const determinismTrials = 20

// determinismScenario is one tool, one fixture, one call.
type determinismScenario struct {
	// exec performs the tool call. It is invoked determinismTrials times and
	// must rebuild whatever state it needs each time, so that a difference
	// between two results can only come from the tool.
	exec func(t *testing.T) tool.Result
	// nonVacuous asserts the result actually reached a multi-element
	// collection. nil takes requireJSONArrayOfAtLeastTwo, which fits every
	// tool whose result is a JSON document; a tool whose result is prose
	// supplies its own.
	nonVacuous func(t *testing.T, content string)
}

// requireJSONArrayOfAtLeastTwo fails unless content is JSON holding an array
// with two or more elements somewhere in it. One element cannot be out of
// order, so a scenario that produced one would be asserting nothing.
func requireJSONArrayOfAtLeastTwo(t *testing.T, content string) {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal([]byte(content), &doc),
		"the scenario's result must be JSON for the default non-vacuity check; "+
			"a prose result needs its own nonVacuous")
	require.GreaterOrEqual(t, longestArray(doc), 2,
		"the scenario produced no collection of two or more elements, so it "+
			"could not have observed an ordering at all: %s", content)
}

// requireNamedArraysOfAtLeastTwo builds a non-vacuity check that names the
// EXACT fields whose ordering the scenario exists to observe.
//
// The generic longest-array check is not enough on its own where a result
// carries several collections: a fixture that quietly stopped producing
// multiple TAGS still satisfied it, because the same result held two REVISIONS.
// The scenario would have gone on passing while covering none of what it was
// written for. Naming the fields makes that failure loud.
func requireNamedArraysOfAtLeastTwo(fields ...string) func(*testing.T, string) {
	return func(t *testing.T, content string) {
		t.Helper()
		requireJSONArrayOfAtLeastTwo(t, content)
		var doc any
		require.NoError(t, json.Unmarshal([]byte(content), &doc))
		for _, f := range fields {
			require.GreaterOrEqual(t, longestArrayUnder(doc, f), 2,
				"the scenario must make %q hold two or more elements — that is the "+
					"collection it was written to observe an ordering of: %s", f, content)
		}
	}
}

// longestArrayUnder reports the length of the longest array found at any
// occurrence of the given object key anywhere in doc.
func longestArrayUnder(doc any, field string) int {
	best := 0
	switch v := doc.(type) {
	case []any:
		for _, e := range v {
			if n := longestArrayUnder(e, field); n > best {
				best = n
			}
		}
	case map[string]any:
		for k, e := range v {
			if k == field {
				if arr, ok := e.([]any); ok && len(arr) > best {
					best = len(arr)
				}
			}
			if n := longestArrayUnder(e, field); n > best {
				best = n
			}
		}
	}
	return best
}

// longestArray reports the length of the longest JSON array anywhere in doc.
func longestArray(doc any) int {
	switch v := doc.(type) {
	case []any:
		best := len(v)
		for _, e := range v {
			if n := longestArray(e); n > best {
				best = n
			}
		}
		return best
	case map[string]any:
		best := 0
		for _, e := range v {
			if n := longestArray(e); n > best {
				best = n
			}
		}
		return best
	default:
		return 0
	}
}

// determinismUncovered names each meta tool with no scenario, and why.
//
// A reason is a claim someone made after looking at the tool's result, not a
// placeholder. The two categories are real and different: a result with no
// collection in it cannot carry this defect, while a tool that needs a live
// collaborator can — it is simply covered elsewhere, or not yet covered, and
// saying which is the point.
var determinismUncovered = map[string]string{
	// No collection in the result: a handle, a status word, an ack, or prose
	// composed from scalars. Nothing in the reply has an order to get wrong.
	"agent_work_complete":       "result is an ack with no collection",
	"await_user_message":        "result is one inbound message or an idle-exit marker",
	"set_thread_title":          "result is an ack with no collection",
	"update_status":             "result is an ack with no collection",
	"update_opening_summary":    "result is an ack with no collection; the body it writes is the caller's own single string",
	"show_attachment":           "result is an ack naming one attachment",
	"respond_to_user":           "result is a delivery ack with no collection",
	"claim_trigger_status":      "result is an ack naming one external status",
	"conclude_trigger_status":   "result is an ack naming one external status",
	"send_input":                "result is an ack naming one slot and one offered call; the datum itself never appears in it",
	"select_phase":              "result names the one selected phase",
	"complete_phase":            "result names the one completed phase",
	"set_view_params":           "result is an ack; the params it echoes come from the caller's own ordered args",
	"update_view":               "result is an ack naming the written slot",
	"show_agent_ui":             "result is an ack naming the shown view",
	"artifact_offer_view":       "result is one offer handle plus its outcome",
	"lookup_user_for_mention":   "result is one resolved mention, or a not-found",
	"request_credential_update": "result is an ack for one credential request",
	"update_plan":               "covered by the plan fixtures in update_plan_test.go; its lists come from the caller's own ordered args, never from a map",
	"sync_workspace":            "dispatches a reconcile Job; needs envtest (workspace_tools_envtest_test.go)",
	"apply_workspace":           "dispatches a reconcile Job; needs envtest (workspace_tools_envtest_test.go)",
	"ask_parent":                "result is an ack or a parking marker; the answer that resumes the session is one message",
	"request_input":             "result is an ack naming the one slot it asked for",
	"derive_tag":                "result is a single string: the new tag id, its nonce, and wrap instructions; no collection",
	"return_result":             "result is an ack with no collection. Its refusal is the gate agent_work_complete shares, and that unmet list is ordered by the class's own completionRequirements slice, never by a map",
	"record_observation":        "result is an ack naming the stored entry's server-assigned id and its (already-proved) destination scope; no collection",
	"get_preferences":           "the result's collections (Snapshot.Keys, each key's Enum) are exactly what PreferencesReader.Current (or ForRef, for a named user) returned already resolved and ordered — preferences.Resolve walks the class's own declared schema slice for Keys/Enum and explicitly sorts Violations; this tool ranges no map of its own before marshaling",
	"set_preference":            "result is an ack naming one key's resolved value/source, or a not-saved/locked message; its one list-shaped error (unknown key) enumerates Snapshot.Keys in the same schema-declared order as get_preferences, never a map",

	// Collections exist, but reaching them needs a live collaborator this
	// package has no unit fixture for. Named so the gap is visible rather than
	// absent.
	"query_knowledge":      "needs a KG provider; no unit fixture in this package yet",
	"read_thread_history":  "needs a NATS request/reply peer; no unit fixture in this package yet",
	"read_channel_history": "needs a NATS request/reply peer; no unit fixture in this package yet",
	"read_view":            "needs an AgentUI-backed uiview runtime; no unit fixture in this package yet",
	"artifact_await":       "shares artifact_prepare's result type AND its encoder, and the encoder is what this guard pins; covered through artifact_prepare",
	"delegate":             "needs a SubagentRequest driven to a terminal phase; no unit fixture in this package yet. Its one collection is the child's returned artifact handles, whose order is the child's own `artifacts` argument (delegate_artifacts_test.go), never a map",
	"reply_to_subagent":    "shares delegate's waitForNext result path, artifact rendering included; covered through delegate",
}

// determinismScenarios is the executed half, keyed by the tool's LLM-facing
// name.
func determinismScenarios(t *testing.T) map[string]determinismScenario {
	t.Helper()
	return map[string]determinismScenario{
		"artifact_prepare": {
			exec:       execArtifactPrepare,
			nonVacuous: requireNamedArraysOfAtLeastTwo("tags", "warnings"),
		},
		"artifact_history": {
			exec:       execArtifactHistory,
			nonVacuous: requireNamedArraysOfAtLeastTwo("revisions", "tags"),
		},
		"search_memory": {
			exec:       execSearchMemory,
			nonVacuous: requireNamedArraysOfAtLeastTwo("dropped_filters", "entries"),
		},
		"query_memory": {
			exec:       execQueryMemory,
			nonVacuous: requireNamedArraysOfAtLeastTwo("entries"),
		},
		"introspect_tool": {exec: execIntrospectUnknown, nonVacuous: requireListsAtLeastTwoNames},
		"load_skill":      {exec: execLoadSkillUnknown, nonVacuous: requireListsAtLeastTwoNames},
	}
}

// requireListsAtLeastTwoNames is the non-vacuity check for the two tools whose
// reply is prose: both answer an unknown name by listing what IS available,
// and that list is built from a map.
func requireListsAtLeastTwoNames(t *testing.T, content string) {
	t.Helper()
	require.GreaterOrEqual(t, strings.Count(content, "\n  "), 2,
		"the scenario must make the tool list two or more names, or it observes no ordering: %s", content)
}

func TestMetaToolResults_AreDeterministic(t *testing.T) {
	for name, sc := range determinismScenarios(t) {
		t.Run(name+": identical inputs encode to identical bytes", func(t *testing.T) {
			first := sc.exec(t)
			require.NotEmpty(t, first.Content, "the scenario must produce a result to compare")

			check := sc.nonVacuous
			if check == nil {
				check = requireJSONArrayOfAtLeastTwo
			}
			check(t, first.Content)

			for i := 1; i < determinismTrials; i++ {
				got := sc.exec(t)
				require.Equal(t, first.Content, got.Content,
					"run %d differed from run 0. The model reads these bytes, so two "+
						"identical operations returning two different results is a "+
						"production defect, not a test artifact — find the collection "+
						"built from a map and give it a canonical order at its source", i)
				require.Equal(t, first.IsError, got.IsError, "run %d flipped IsError", i)
			}
		})
	}
}

// TestMetaToolDeterminism_ClassifiesEveryMetaTool is the coverage gate. Every
// meta tool must be scenario-covered or listed as uncovered WITH A REASON, and
// no tool may be both.
func TestMetaToolDeterminism_ClassifiesEveryMetaTool(t *testing.T) {
	scenarios := determinismScenarios(t)
	for ctor, tl := range allMetaTools(t) {
		name := tl.Name()
		_, covered := scenarios[name]
		reason, uncovered := determinismUncovered[name]

		assert.True(t, covered || uncovered,
			"meta tool %q (%s) is not classified for the determinism guard. Look at "+
				"what its result contains: if any collection in it is built from a Go "+
				"map, give that collection a canonical order at its source and add a "+
				"scenario; if the result carries no collection, say so in "+
				"determinismUncovered.", name, ctor)
		assert.False(t, covered && uncovered,
			"meta tool %q is both scenario-covered and listed as uncovered (%q)", name, reason)
		if uncovered {
			assert.NotEmpty(t, strings.TrimSpace(reason),
				"meta tool %q is listed as uncovered with an empty reason", name)
		}
	}
}

// TestMetaToolDeterminism_ClassifiesNoStranger is the other direction: a name
// in either table that is not a meta tool is a stale entry, and a stale entry
// is how a table stops describing the thing it claims to describe.
func TestMetaToolDeterminism_ClassifiesNoStranger(t *testing.T) {
	real := map[string]bool{}
	for _, tl := range allMetaTools(t) {
		real[tl.Name()] = true
	}
	for name := range determinismScenarios(t) {
		assert.True(t, real[name], "determinismScenarios names %q, which is not a meta tool", name)
	}
	for name := range determinismUncovered {
		assert.True(t, real[name], "determinismUncovered names %q, which is not a meta tool", name)
	}
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

// execArtifactPrepare renders one artifact whose CR status carries three
// sanitizer warnings in a scrambled order and whose annotations apply two tags
// on top of the service's own "latest". Both collections in the result are
// therefore multi-element and neither arrives pre-sorted.
func execArtifactPrepare(t *testing.T) tool.Result {
	t.Helper()
	scheme := newPrepareArtifactScheme(t)
	warnings := []spiceboxv1alpha1.SanitizerWarning{
		{Kind: "tag", Name: "title", Action: "removed", Count: 1},
		{Kind: "attr", Name: "lang", Action: "stripped", Count: 1},
		{Kind: "tag", Name: "meta", Action: "unwrapped", Count: 1},
	}
	stamp := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if cr, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				cr.UID = types.UID("uid-determinism")
				cr.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				cr.Status.OutputRef = "mem://ar"
				cr.Status.OutputMIME = "text/html"
				cr.Status.OutputSize = 42
				cr.Status.OutputFilename = "report.html"
				cr.Status.Warnings = warnings
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(stamp).Build()

	tl := meta.NewArtifactPrepare(meta.ArtifactPrepareConfig{
		Client:         c,
		Artifacts:      newDeterministicArtifactSvc(),
		AvailableKinds: []string{"html"},
		PollInterval:   time.Millisecond,
	})
	args := json.RawMessage(`{"kind":"html","payload":"<p>hi</p>","name":"report",` +
		`"tags":["zeta","approved"],"max_wait_seconds":5}`)
	res, err := tl.Execute(determinismCtx(), args, determinismSession(t))
	require.NoError(t, err, "artifact_prepare must not fail the call itself")
	require.False(t, res.IsError, "artifact_prepare fixture must reach a ready render: %s", res.Content)
	return res
}

// execArtifactHistory lists a two-revision artifact whose newest revision
// carries three tags, so both the revision list and its tag list are
// multi-element.
func execArtifactHistory(t *testing.T) tool.Result {
	t.Helper()
	svc := newDeterministicArtifactSvc()
	ctx := determinismCtx()
	sess := determinismSession(t)
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	headID := svc.NewArtifactID()

	for i, spec := range []struct {
		name, tags string
		uid        types.UID
	}{
		{"ar-1", "", types.UID("uid-h1")},
		{"ar-2", "zeta,approved", types.UID("uid-h2")},
	} {
		cr := &spiceboxv1alpha1.ArtifactRender{
			ObjectMeta: metav1.ObjectMeta{
				Name: spec.name, Namespace: sess.Namespace, UID: spec.uid,
				Labels:      map[string]string{artifacts.LabelArtifactID: headID},
				Annotations: map[string]string{artifacts.AnnoChangeDescription: fmt.Sprintf("rev %d", i+1)},
			},
			Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
			Status: spiceboxv1alpha1.ArtifactRenderStatus{
				Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputRef: "mem://" + spec.name,
				OutputMIME: "text/html", OutputSize: 10, OutputFilename: spec.name + ".html",
			},
		}
		if i == 0 {
			cr.Annotations[artifacts.AnnoArtifactName] = "report"
		}
		if spec.tags != "" {
			cr.Annotations[artifacts.AnnoAppliedTags] = spec.tags
		}
		_, err := svc.FinalizeRevision(ctx, scope, cr)
		require.NoError(t, err, "seeding revision %d", i+1)
	}

	tl := meta.NewArtifactHistory(meta.ArtifactHistoryConfig{Artifacts: svc})
	res, err := tl.Execute(ctx, json.RawMessage(`{"artifact":"`+headID+`"}`), sess)
	require.NoError(t, err, "artifact_history must not fail the call itself")
	require.False(t, res.IsError, "artifact_history fixture must succeed: %s", res.Content)
	return res
}

// execSearchMemory drives search_memory over a stub whose merged result names
// TWO providers, each reporting a dropped filter. The tool flattens
// MergedSearchResult.PerProvider — a Go map — into the model-facing
// `dropped_filters`, which is where the third instance of this defect lived.
func execSearchMemory(t *testing.T) tool.Result {
	t.Helper()
	sess := determinismSession(t)
	sess.Mem = &fakeMemSearcher{result: memory.MergedSearchResult{
		Entries: []memory.ScoredEntry{
			{Entry: memory.Entry{Kind: "note", ID: "n1"}, Score: 0.9, Source: "alpha"},
			{Entry: memory.Entry{Kind: "note", ID: "n2"}, Score: 0.5, Source: "beta"},
		},
		PerProvider: map[string]memory.SearchResult{
			"alpha": {DroppedFilters: []string{"tags (alpha)"}},
			"beta":  {DroppedFilters: []string{"fields (beta)"}},
			"gamma": {DroppedFilters: []string{"time (gamma)"}},
		},
	}}

	tl := meta.NewSearchMemory()
	res, err := tl.Execute(determinismCtx(), json.RawMessage(`{"text":"anything"}`), sess)
	require.NoError(t, err, "search_memory must not fail the call itself")
	require.False(t, res.IsError, "search_memory fixture must succeed: %s", res.Content)
	return res
}

// execQueryMemory queries a seeded in-memory backend, so the result carries a
// multi-entry list assembled by the real facade.
func execQueryMemory(t *testing.T) tool.Result {
	t.Helper()
	sess := determinismSession(t)
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := determinismCtx()
	scope := memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	// CreatedAt is pinned rather than taken from the clock. The backend orders
	// newest-first and breaks ties by entry ID, so a wall-clock fixture is
	// deterministic on any one run and can still differ BETWEEN runs — two
	// seeds landing in the same clock tick tie, three seeds spread over two
	// ticks do not, and which happens is up to the scheduler. That is a
	// property of the fixture, not of the tool, and leaving it in would make
	// this scenario fail for a reason it is not testing. (It did, on the first
	// run of this file: worth writing down, because a flaky guard gets deleted.)
	for i, id := range []string{"r1", "r2", "r3"} {
		content, err := json.Marshal(label.Label{
			ResourceType: "demo_resource", ResourceID: id, Label: "label for " + id,
		})
		require.NoError(t, err, "marshalling %s", id)
		_, err = mem.Put(ctx, memory.Entry{
			Scope: scope, Kind: label.KindName, ID: "label-demo-" + id,
			CreatedAt: time.Unix(int64(1_700_000_000+i), 0).UTC(),
			Tags:      []string{"trust:untrusted"}, Content: content,
		})
		require.NoError(t, err, "seeding %s", id)
	}
	sess.Mem = mem

	tl := meta.NewQueryMemory()
	res, err := tl.Execute(ctx, json.RawMessage(`{"kinds":["`+label.KindName+`"],"limit":10}`), sess)
	require.NoError(t, err, "query_memory must not fail the call itself")
	require.False(t, res.IsError, "query_memory fixture must succeed: %s", res.Content)
	return res
}

// execIntrospectUnknown asks introspect_tool for a name it does not have. The
// reply lists the valid names, and that list is built by ranging a map.
func execIntrospectUnknown(t *testing.T) tool.Result {
	t.Helper()
	byName := map[string]tool.Tool{
		"gitlike_gh": nil, "shell_run": nil, "search_web": nil, "read_doc": nil,
	}
	tl := meta.NewIntrospect(meta.IntrospectConfig{
		Resolve: func(string) (tool.Tool, bool) { return nil, false },
		Names: func() []string {
			out := make([]string, 0, len(byName))
			for n := range byName {
				out = append(out, n)
			}
			return out
		},
	})
	res, err := tl.Execute(determinismCtx(), json.RawMessage(`{"tool":"nope"}`), determinismSession(t))
	require.NoError(t, err, "introspect_tool must not fail the call itself")
	require.True(t, res.IsError, "the fixture asks for a tool that does not exist")
	// The prose reply joins with ", " rather than a newline, so the shared
	// prose check needs the same separator to count against.
	return tool.Result{Content: strings.ReplaceAll(res.Content, ", ", "\n  "), IsError: res.IsError}
}

// execLoadSkillUnknown asks load_skill for a skill it does not have. The reply
// lists the available skills, and that list is built by ranging a map.
func execLoadSkillUnknown(t *testing.T) tool.Result {
	t.Helper()
	tl := meta.NewLoadSkill(map[string]string{
		"demo-triage": "# triage", "demo-review": "# review", "demo-summarize": "# summarize",
	})
	res, err := tl.Execute(determinismCtx(), json.RawMessage(`{"skill":"nope"}`), determinismSession(t))
	require.NoError(t, err, "load_skill must not fail the call itself")
	require.True(t, res.IsError, "the fixture asks for a skill that does not exist")
	return res
}

// ---------------------------------------------------------------------------
// Shared fixture
// ---------------------------------------------------------------------------

func determinismCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "determinism-test")
}

// newDeterministicArtifactSvc pins the three id seams the artifact service
// mints through, so a scenario comparing two runs is comparing what the tool
// COMPOSED and not which random ids it drew.
//
// This is not the guard looking the other way. Every one of these is already a
// declared seam (NewService's newID, WithRenderNameMinter, WithRevisionIDMinter)
// precisely because a value the run itself chose cannot be reproduced by a
// later run — that is what the whole-session replay uses them for. A scenario
// that left them live would fail on the ids on its second run and never reach
// the ordering question this file exists to ask.
func newDeterministicArtifactSvc() *artifacts.Service {
	return artifacts.NewService(
		memory.NewLocal(inmem.NewBackend()),
		func() string { return "artifact-0000000000000001" },
		artifacts.WithRenderNameMinter(func(session string) string { return "ar-" + session + "-aaaaaa" }),
		// Pin the clock: CreatedAt is written into the entry artifact_history
		// hands the model, so a wall-clock stamp makes two runs that straddle a
		// tick encode different bytes. artifact_history orders by seq, not
		// CreatedAt, so a constant is sound. Mirrors the pin execQueryMemory
		// already applies for the same reason.
		artifacts.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	)
}

func determinismSession(t *testing.T) *tool.SessionContext {
	t.Helper()
	return &tool.SessionContext{Namespace: "default", Name: "demo-session"}
}
