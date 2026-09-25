package uicomponents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

// baseDecl is one author's Tier-0 view: a writable "panel" hook (allowlist
// wide open), one action whose declared input is "why", and a control
// driving the binding parameter "span" (deliberately NOT "why" — the
// collision is what a fragment introduces).
const baseDecl = `{
  "actions":[
    {"name":"advance","tool":"crm_advance_stage","args":{"note":{"$param":"why"}},"inputs":["why"]}
  ],
  "view":{"component":"ap:stack","children":[
    {"component":"oap:generative","props":{"name":"panel","allowedComponents":["*"]},"children":[
      {"component":"ap:stack","children":[
        {"component":"ap:select","props":{"param":"span","value":"30d","options":[{"value":"30d"}]}},
        {"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_list_leads","args":{"span":{"$param":"span"}}}}}
      ]}
    ]}
  ]}}`

func baseOptions(t *testing.T) uicomponents.Options {
	t.Helper()
	o := uicomponents.DefaultOptions()
	o.GrantedTools = map[string]bool{"crm_advance_stage": true, "crm_list_leads": true}
	// Narrower than GrantedTools on purpose: an ACTION validates against the
	// grant alone, a DATA binding against both. Keeping them different is what
	// makes a copy-paste between the two checks turn a test red.
	o.ReadonlyTools = map[string]bool{"crm_list_leads": true}
	return o
}

func mustBase(t *testing.T) uicomponents.Declaration {
	t.Helper()
	d, err := uicomponents.ParseDeclaration([]byte(baseDecl))
	require.NoError(t, err, "fixture must parse")
	require.NoError(t, uicomponents.Validate(d, baseOptions(t)), "fixture must validate before any merge")
	return d
}

// frag builds a fill fragment from raw fragment JSON — every fixture below
// fills a hook, never clears one; TestResolveViewClearCollapsesTheHookOverTheAuthorDefault
// exercises Node == nil directly.
func frag(t *testing.T, hook, raw string) uicomponents.Fragment {
	t.Helper()
	n, err := uicomponents.ParseNode([]byte(raw))
	require.NoError(t, err, "fragment fixture must parse")
	return uicomponents.Fragment{Hook: hook, Node: &n}
}

// hookByName requires the declaration's view carries a hook with this name —
// used both on a base fixture (before any merge) and on a resolved View's
// Declaration (after one).
func hookByName(t *testing.T, d uicomponents.Declaration, name string) uicomponents.Hook {
	t.Helper()
	for _, h := range uicomponents.Hooks(d) {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no hook named %q", name)
	return uicomponents.Hook{}
}

// nodeAt returns the node a Hook.Path describes, relative to root — the same
// index-path language BindingPath and ActionRef.Path use.
func nodeAt(t *testing.T, root uicomponents.Node, path []int) uicomponents.Node {
	t.Helper()
	n := root
	for _, i := range path {
		require.Less(t, i, len(n.Children), "path %v is out of range in the view", path)
		n = n.Children[i]
	}
	return n
}

// viewWithHooks is the fixture every ResolveView test starts from: "phase"
// admits only ap:steps, "brief" admits everything, "intake" admits
// markdown+form but its author default is an ap:card — the asymmetry the
// spec accepts.
func viewWithHooks(t *testing.T) uicomponents.Declaration {
	t.Helper()
	d, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"phase","allowedComponents":["ap:steps"]},
	   "children":[{"component":"ap:steps","props":{"steps":[{"label":"Intake","state":"active"}]}}]},
	  {"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}},
	  {"component":"oap:generative","props":{"name":"intake","allowedComponents":["ap:markdown","ap:form"]},
	   "children":[{"component":"ap:card","props":{"title":"Describe it"},"children":[{"component":"ap:markdown","props":{"body":"tell me"}}]}]}
	]}}`))
	require.NoError(t, err)
	return d
}

func hookNode(t *testing.T, v uicomponents.View, name string) uicomponents.Node {
	t.Helper()
	for _, h := range uicomponents.Hooks(v.Declaration) {
		if h.Name != name {
			continue
		}
		n := *v.Declaration.View
		for _, i := range h.Path {
			n = n.Children[i]
		}
		return n
	}
	t.Fatalf("no hook %q", name)
	return uicomponents.Node{}
}

func TestResolveView(t *testing.T) {
	cases := []struct {
		name          string
		fragments     func(t *testing.T) []uicomponents.Fragment
		wantComposed  []string
		wantRejected  string // substring of the single expected rejection reason; "" = none
		wantPanelComp string // the component type the "panel" hook ends up rendering
	}{
		{
			name: "a legal fragment replaces the hook and marks it agent-composed",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:markdown","props":{"body":"agent copy"}}`)}
			},
			wantComposed:  []string{"panel"},
			wantPanelComp: "ap:markdown",
		},
		{
			// THE CARRY-FORWARD. The fragment is well-formed and its component
			// is registered; what makes it illegal is a whole-declaration fact
			// that only exists AFTER the merge — "why" is a declared action
			// input, and validateActions requires inputs disjoint from
			// ParamNames. Nothing short of re-validating the merged document
			// catches this.
			name: "a fragment introducing a parameter that collides with an action input is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:select","props":{"param":"why","value":"x","options":[{"value":"x"}]}}`)}
			},
			wantComposed:  nil,
			wantRejected:  "collides with a binding parameter",
			wantPanelComp: "ap:stack",
		},
		{
			name: "a fragment binding a tool outside the grant is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_delete_lead"}}}`)}
			},
			wantComposed:  nil,
			wantRejected:  "not granted",
			wantPanelComp: "ap:stack",
		},
		{
			name: "a fragment binding a GRANTED but non-readonly tool is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"crm_advance_stage"}}}`)}
			},
			wantComposed:  nil,
			wantRejected:  "may only read",
			wantPanelComp: "ap:stack",
		},
		{
			name: "a fragment naming an undeclared action is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:button","props":{"label":"Go","action":"nope"}}`)}
			},
			wantComposed:  nil,
			wantRejected:  "undeclared action",
			wantPanelComp: "ap:stack",
		},
		{
			name: "a fragment naming a hook the declaration does not have is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "ghost", `{"component":"ap:markdown","props":{"body":"x"}}`)}
			},
			wantComposed:  nil,
			wantRejected:  "unknown hook",
			wantPanelComp: "ap:stack",
		},
		{
			name: "a fragment naming an unregistered component is rejected",
			fragments: func(t *testing.T) []uicomponents.Fragment {
				return []uicomponents.Fragment{frag(t, "panel", `{"component":"ap:doesnotexist"}`)}
			},
			wantComposed:  nil,
			wantRejected:  "unknown component type",
			wantPanelComp: "ap:stack",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := uicomponents.ResolveView(mustBase(t), tc.fragments(t), baseOptions(t))

			assert.Equal(t, tc.wantComposed, v.AgentComposed)
			if tc.wantRejected == "" {
				assert.Empty(t, v.Rejected, "no rejection expected")
			} else {
				require.Len(t, v.Rejected, 1, "exactly one rejection expected")
				assert.Contains(t, v.Rejected[0].Reason, tc.wantRejected)
				assert.NotEmpty(t, v.Rejected[0].Hook, "a rejection with no hook cannot be logged usefully")
			}

			// Whatever happened, the returned declaration must be renderable.
			require.NoError(t, uicomponents.Validate(v.Declaration, baseOptions(t)),
				"ResolveView must never return a declaration that does not validate")

			h := hookByName(t, v.Declaration, "panel")
			hn := nodeAt(t, *v.Declaration.View, h.Path)
			require.Len(t, hn.Children, 1, "the panel hook always carries exactly one child: the author default or the accepted fragment")
			assert.Equal(t, tc.wantPanelComp, hn.Children[0].Component)
		})
	}
}

func TestResolveViewKeepsSiblingsWhenOneFragmentFails(t *testing.T) {
	base, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"a","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"a0"}}]},
	  {"component":"oap:generative","props":{"name":"b","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"b0"}}]}
	]}}`))
	require.NoError(t, err)

	v := uicomponents.ResolveView(base, []uicomponents.Fragment{
		frag(t, "b", `{"component":"ap:markdown","props":{"body":"good"}}`),
		frag(t, "a", `{"component":"ap:doesnotexist"}`),
	}, uicomponents.DefaultOptions())

	assert.Equal(t, []string{"b"}, v.AgentComposed, "the good fragment must still land")
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, "a", v.Rejected[0].Hook)

	ha := hookByName(t, v.Declaration, "a")
	assert.Equal(t, "ap:text", nodeAt(t, *v.Declaration.View, ha.Path).Children[0].Component,
		"the failed hook keeps its prior content")
	hb := hookByName(t, v.Declaration, "b")
	assert.Equal(t, "ap:markdown", nodeAt(t, *v.Declaration.View, hb.Path).Children[0].Component)
}

func TestResolveViewNeverTouchesTheActionTable(t *testing.T) {
	base := mustBase(t)
	v := uicomponents.ResolveView(base, []uicomponents.Fragment{
		frag(t, "panel", `{"component":"ap:button","props":{"label":"Advance","action":"advance"}}`),
	}, baseOptions(t))

	require.Len(t, v.Declaration.Actions, 1, "a fragment may NAME an action and can never mint one")
	assert.Equal(t, "advance", v.Declaration.Actions[0].Name)
	assert.Equal(t, "crm_advance_stage", v.Declaration.Actions[0].Tool)
	assert.Equal(t, []string{"panel"}, v.AgentComposed)
}

func TestResolveViewIsPure(t *testing.T) {
	base := mustBase(t)
	h := hookByName(t, base, "panel")
	before := nodeAt(t, *base.View, h.Path).Children[0].Component
	_ = uicomponents.ResolveView(base, []uicomponents.Fragment{
		frag(t, "panel", `{"component":"ap:markdown","props":{"body":"x"}}`),
	}, baseOptions(t))
	after := nodeAt(t, *base.View, h.Path).Children[0].Component
	assert.Equal(t, before, after,
		"ResolveView must not mutate the caller's base declaration; webd resolves per request off one cached CR")
}

// TestResolveViewIsDeterministic guards "same base + same fragments ⇒
// byte-identical output, every time": four writable hooks, two fragments
// that succeed and two that fail with DIFFERENT reasons, run repeatedly. A
// merge that ordered fragments by ranging a map (rather than by base-hook
// position) would reorder Rejected — and, on occasion, which candidate a
// later fragment builds on — across runs with high probability, since Go
// randomizes map iteration order per range.
func TestResolveViewIsDeterministic(t *testing.T) {
	base, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"a","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"a0"}}]},
	  {"component":"oap:generative","props":{"name":"b","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"b0"}}]},
	  {"component":"oap:generative","props":{"name":"c","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"c0"}}]},
	  {"component":"oap:generative","props":{"name":"d","allowedComponents":["*"]},"children":[{"component":"ap:text","props":{"text":"d0"}}]}
	]}}`))
	require.NoError(t, err)

	fragments := []uicomponents.Fragment{
		frag(t, "d", `{"component":"ap:doesnotexist-d"}`),
		frag(t, "a", `{"component":"ap:markdown","props":{"body":"good-a"}}`),
		frag(t, "c", `{"component":"ap:doesnotexist-c"}`),
		frag(t, "b", `{"component":"ap:markdown","props":{"body":"good-b"}}`),
	}

	first := uicomponents.ResolveView(base, fragments, uicomponents.DefaultOptions())
	firstJSON, err := json.Marshal(first.Declaration)
	require.NoError(t, err)

	for i := 0; i < 25; i++ {
		got := uicomponents.ResolveView(base, fragments, uicomponents.DefaultOptions())
		gotJSON, err := json.Marshal(got.Declaration)
		require.NoError(t, err)
		require.Equal(t, string(firstJSON), string(gotJSON), "run %d: merged declaration must be byte-identical", i)
		assert.Equal(t, first.AgentComposed, got.AgentComposed, "run %d: AgentComposed order must be stable", i)
		require.Equal(t, first.Rejected, got.Rejected, "run %d: Rejected order must be stable, not an accident of map iteration", i)
	}
}

// TestResolveViewRejectsAFragmentWhosePathWasInvalidatedByAnEarlierOne pins a
// consequence of re-deriving a fragment's hook from the ACCEPTED tree rather
// than from a path computed once at the start of the call: "outer" is
// applied first and replaces its two children (ap:text, then the "inner"
// hook) with a single fill node — so the "inner" hook, which lived inside
// that replaced subtree, is no longer anywhere in the accepted tree by the
// time its own fragment is considered. It must be reported the same way any
// other undeclared name would be — "unknown hook" — never resolved against
// whatever index now happens to occupy the stale path, and never a panic.
func TestResolveViewRejectsAFragmentWhosePathWasInvalidatedByAnEarlierOne(t *testing.T) {
	base, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"outer","allowedComponents":["*"]},"children":[
	    {"component":"ap:text","props":{"text":"placeholder"}},
	    {"component":"oap:generative","props":{"name":"inner","allowedComponents":["*"]}}
	  ]}
	]}}`))
	require.NoError(t, err)

	var v uicomponents.View
	require.NotPanics(t, func() {
		v = uicomponents.ResolveView(base, []uicomponents.Fragment{
			frag(t, "outer", `{"component":"ap:markdown","props":{"body":"replaced"}}`),
			frag(t, "inner", `{"component":"ap:text","props":{"text":"inner content"}}`),
		}, uicomponents.DefaultOptions())
	}, "an earlier fragment removing a nested hook must be rejected, never panic")

	assert.Equal(t, []string{"outer"}, v.AgentComposed, "outer applies first and lands cleanly")
	require.Len(t, v.Rejected, 1, "inner's now-absent hook must be a Rejection, not silently dropped or applied to the wrong node")
	assert.Equal(t, "inner", v.Rejected[0].Hook)
	assert.Equal(t, `unknown hook "inner"`, v.Rejected[0].Reason,
		"inner is re-derived from the ACCEPTED tree, which no longer has it — never the stale-path bounds error")
}

// TestResolveViewRejectsAnInRangeStaleHookAsUnknownToo is the sibling of the
// test above, and the one that proves the guard is re-derivation and not a
// coincidence of bounds-checking. Here "inner" sits at outer's child index
// 0, and outer's fill also produces exactly one child — so a lookup against
// inner's OLD recorded path would land IN RANGE, inside outer's brand-new
// fill content, rather than failing a bounds check. Re-deriving "inner" from
// Hooks(accepted) catches this too: the hook is gone from the accepted tree
// regardless of whether its stale index would happen to still be valid, so
// it is "unknown hook", and inner's content must never appear anywhere
// inside outer's fill.
func TestResolveViewRejectsAnInRangeStaleHookAsUnknownToo(t *testing.T) {
	base, err := uicomponents.ParseDeclaration([]byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"oap:generative","props":{"name":"outer","allowedComponents":["*"]},"children":[
	    {"component":"oap:generative","props":{"name":"inner","allowedComponents":["*"]}},
	    {"component":"ap:text","props":{"text":"placeholder"}}
	  ]}
	]}}`))
	require.NoError(t, err)

	container := uicomponents.Node{Component: "ap:stack", Children: []uicomponents.Node{
		{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(`"filled"`)}},
	}}
	innerFill := uicomponents.Node{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(`"inner content"`)}}

	v := uicomponents.ResolveView(base, []uicomponents.Fragment{
		{Hook: "outer", Node: &container},
		{Hook: "inner", Node: &innerFill},
	}, uicomponents.DefaultOptions())

	assert.Equal(t, []string{"outer"}, v.AgentComposed, "outer applies first and lands cleanly")
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, "inner", v.Rejected[0].Hook)
	assert.Equal(t, `unknown hook "inner"`, v.Rejected[0].Reason)

	outerNode := hookNode(t, v, "outer")
	require.Len(t, outerNode.Children, 1)
	require.Equal(t, "ap:stack", outerNode.Children[0].Component)
	require.Len(t, outerNode.Children[0].Children, 1, "inner's content must never have been spliced into outer's fill")
	assert.Equal(t, "ap:text", outerNode.Children[0].Children[0].Component)
	assert.Equal(t, `"filled"`, string(outerNode.Children[0].Children[0].Props["text"]))
}

func TestResolveViewSubstitutesAFillIntoTheHooksChildren(t *testing.T) {
	fill := uicomponents.Node{Component: "ap:markdown", Props: map[string]json.RawMessage{"body": json.RawMessage(`"agent copy"`)}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "brief", Node: &fill}}, uicomponents.DefaultOptions())
	assert.Empty(t, v.Rejected)
	assert.Equal(t, []string{"brief"}, v.AgentComposed)
	h := hookNode(t, v, "brief")
	require.Len(t, h.Children, 1)
	assert.Equal(t, "ap:markdown", h.Children[0].Component)
}

func TestResolveViewClearCollapsesTheHookOverTheAuthorDefault(t *testing.T) {
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "intake", Node: nil}}, uicomponents.DefaultOptions())
	assert.Empty(t, v.Rejected)
	assert.Equal(t, []string{"intake"}, v.AgentComposed, "an intentional empty is the agent's")
	assert.Empty(t, hookNode(t, v, "intake").Children, "the author default does not come back")
}

func TestResolveViewEnforcesTheAllowlistOnTheAgentOnly(t *testing.T) {
	// The author default IS an ap:card (accepted at admission); the agent
	// may not write one back.
	card := uicomponents.Node{Component: "ap:card", Children: []uicomponents.Node{{Component: "ap:markdown"}}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "intake", Node: &card}}, uicomponents.DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, "intake", v.Rejected[0].Hook)
	assert.Contains(t, v.Rejected[0].Reason, `component "ap:card" is not allowed in hook "intake"; allowed: [ap:form, ap:markdown]`)
	assert.Empty(t, v.AgentComposed)
	assert.Equal(t, "ap:card", hookNode(t, v, "intake").Children[0].Component, "the author default still stands")
}

func TestResolveViewAllowlistAppliesAtEveryDepth(t *testing.T) {
	// ap:markdown is allowed; the ap:card wrapped AROUND it is not, and the
	// check must reach it at any depth — here the disallowed node is the root
	// of the fill and the allowed one is beneath it.
	nested := uicomponents.Node{Component: "ap:markdown"}
	wrapper := uicomponents.Node{Component: "ap:card", Children: []uicomponents.Node{nested}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "intake", Node: &wrapper}}, uicomponents.DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Contains(t, v.Rejected[0].Reason, `"ap:card" is not allowed`)
}

func TestResolveViewStarAdmitsTheRegistryButNeverAHook(t *testing.T) {
	inner := uicomponents.Node{Component: uicomponents.GenerativeType, Props: map[string]json.RawMessage{"name": json.RawMessage(`"smuggled"`), "allowedComponents": json.RawMessage(`["*"]`)}}
	fill := uicomponents.Node{Component: "ap:stack", Children: []uicomponents.Node{inner}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "brief", Node: &fill}}, uicomponents.DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Contains(t, v.Rejected[0].Reason, `a fill may not contain a hook ("smuggled")`)
}

// TestResolveViewNamesAHookOnceInAgentComposed covers a caller handing two
// fragments for one hook. Both are accepted — the second's fill simply replaces
// the first's — but AgentComposed answers "did the agent write this region",
// which one name can only answer once: the browser reads the list as a set and
// the agent reads it as a list, so a repeat is invisible in one surface and
// noise in the other. uiview.apply collapses a repeat before ResolveView sees
// it; this is the guarantee for every other caller.
func TestResolveViewNamesAHookOnceInAgentComposed(t *testing.T) {
	first := uicomponents.Node{Component: "ap:markdown", Props: map[string]json.RawMessage{"body": json.RawMessage(`"first"`)}}
	second := uicomponents.Node{Component: "ap:markdown", Props: map[string]json.RawMessage{"body": json.RawMessage(`"second"`)}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{
		{Hook: "brief", Node: &first},
		{Hook: "brief", Node: &second},
	}, uicomponents.DefaultOptions())
	require.Empty(t, v.Rejected)
	assert.Equal(t, []string{"brief"}, v.AgentComposed)
	assert.Equal(t, json.RawMessage(`"second"`), hookNode(t, v, "brief").Children[0].Props["body"],
		"the later fragment is the one that stands")
}

// TestResolveViewRefusesEveryStructuralTypeInAFill proves the refusal above is
// driven by the REGISTRATION rather than by a match on oap:generative. A
// structural type is one the agent is never told about (VocabularySchema skips
// it) so a fill naming one is always a fill reaching past the vocabulary it was
// given; a second such type registered later must be outside every fill by
// virtue of being registered structural, with no edit to the allowlist walk.
func TestResolveViewRefusesEveryStructuralTypeInAFill(t *testing.T) {
	prior := registry.All()
	t.Cleanup(func() {
		registry.Reset()
		for _, c := range prior {
			registry.Register(c)
		}
	})
	registry.Register(uicomponents.Component{Type: "ap:demo_structural", Props: struct{}{}, Structural: true})

	fill := uicomponents.Node{Component: "ap:stack", Children: []uicomponents.Node{{Component: "ap:demo_structural"}}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "brief", Node: &fill}}, uicomponents.DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Contains(t, v.Rejected[0].Reason, `a fill may not contain the structural component "ap:demo_structural"`)
	assert.Empty(t, v.AgentComposed)
}

func TestResolveViewRejectsAnUnknownHook(t *testing.T) {
	n := uicomponents.Node{Component: "ap:text"}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{{Hook: "nope", Node: &n}}, uicomponents.DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, `unknown hook "nope"`, v.Rejected[0].Reason)
}

func TestResolveViewOrdersFragmentsByHookPositionAndUnknownLast(t *testing.T) {
	n := uicomponents.Node{Component: "ap:text"}
	steps := uicomponents.Node{Component: "ap:steps", Props: map[string]json.RawMessage{"steps": json.RawMessage(`[]`)}}
	v := uicomponents.ResolveView(viewWithHooks(t), []uicomponents.Fragment{
		{Hook: "zzz", Node: &n}, {Hook: "brief", Node: &n}, {Hook: "phase", Node: &steps},
	}, uicomponents.DefaultOptions())
	assert.Equal(t, []string{"brief", "phase"}, v.AgentComposed, "sorted")
	require.Len(t, v.Rejected, 1)
	assert.Equal(t, "zzz", v.Rejected[0].Hook)
}

func TestParseNodeIsStrict(t *testing.T) {
	_, err := uicomponents.ParseNode([]byte(`{"component":"ap:text","propz":{"text":"x"}}`))
	require.Error(t, err, "an unknown wire field must be rejected, never dropped")

	n, err := uicomponents.ParseNode([]byte(`{"component":"ap:text","props":{"text":"x"}}`))
	require.NoError(t, err)
	assert.Equal(t, "ap:text", n.Component)
}
