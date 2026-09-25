package uicomponents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// TestBindingPath documents the format. It is NOT the Go↔TS pin — a literal
// here and its twin in bindings.test.tsx can be swept together with both
// suites staying green. That pin is pkg/web/webui/agentui/ui/testdata/
// bindings.golden.json, asserted from Go by bindings_golden_internal_test.go
// and re-derived from TS by bindingsGolden.test.tsx.
func TestBindingPath(t *testing.T) {
	assert.Equal(t, "root/#rows", uicomponents.BindingPath("root", nil, "rows"))
	assert.Equal(t, "root/0.2#rows", uicomponents.BindingPath("root", []int{0, 2}, "rows"))
	assert.Equal(t, "side/1#body", uicomponents.BindingPath("side", []int{1}, "body"))
}

func TestWalkBindingsAndParamNames(t *testing.T) {
	raw := []byte(`{"slots":[
      {"name":"root","default":{"component":"ap:stack","children":[
        {"component":"ap:select","props":{"param":"span","value":"7d"}},
        {"component":"ap:table","props":{"columns":[{"key":"a"}]},
         "bindings":{"rows":{"source":"tool","ref":"crm_list_leads","args":{"since":{"$param":"span"}}}}}
      ]}},
      {"name":"side","default":{"component":"ap:markdown",
         "bindings":{"body":{"source":"artifact","ref":"artifact-abc"}}}}
    ]}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err, "fixture must parse")

	got := uicomponents.WalkBindings(decl)
	require.Len(t, got, 2, "two bound props across two regions")
	// Neither fixture slot sets agentWritable, so the shim places BOTH defaults
	// directly in the root region: root's ap:stack is view.children[0] and
	// side's markdown is view.children[1]; paths are relative to the view root.
	assert.Equal(t, "/0.1#rows", got[0].Path)
	assert.Equal(t, "", got[0].Region)
	assert.Equal(t, "rows", got[0].Prop)
	assert.Equal(t, "tool", got[0].Binding.Source)
	assert.Equal(t, "crm_list_leads", got[0].Binding.Ref)
	assert.JSONEq(t, `{"since":{"$param":"span"}}`, string(got[0].Binding.Args))
	assert.Equal(t, "/1#body", got[1].Path)
	assert.Equal(t, "artifact", got[1].Binding.Source)

	assert.Equal(t, []string{"span"}, uicomponents.ParamNames(decl),
		"the oap:select's param prop is found through the registry, not a type switch")
}

func TestWalkBindingsRegionIsTheEnclosingHook(t *testing.T) {
	raw := []byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"ap:markdown","bindings":{"body":{"source":"artifact","ref":"artifact-top"}}},
	  {"component":"oap:generative","props":{"name":"data","allowedComponents":["*"]},"children":[
	    {"component":"ap:card","children":[
	      {"component":"ap:table","props":{"columns":[{"key":"a"}]},"bindings":{"rows":{"source":"tool","ref":"crm_list_leads"}}}
	    ]}
	  ]}
	]}}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	got := uicomponents.WalkBindings(decl)
	require.Len(t, got, 2)
	assert.Equal(t, "/0#body", got[0].Path, "outside any hook: region \"\", path from the view root")
	assert.Equal(t, "", got[0].Region)
	assert.Equal(t, "data/0.0#rows", got[1].Path, "inside a hook: region = the hook's name, path from the hook node")
	assert.Equal(t, "data", got[1].Region)
}

// TestWalkBindingsRegionAppliesAtTheViewRoot pins the region rule at the ONE
// place a node can be a hook without any enclosing hook classifying it as
// one: the view's own root. `{"view":{"component":"oap:generative",…}}` is a
// legal authored page — an author can make the WHOLE page a single writable
// region — and everything under it, including a binding on a direct child of
// the root, must be attributed to that hook's region, not "".
func TestWalkBindingsRegionAppliesAtTheViewRoot(t *testing.T) {
	raw := []byte(`{"view":{"component":"oap:generative","props":{"name":"page","allowedComponents":["*"]},"children":[
	  {"component":"ap:markdown","bindings":{"body":{"source":"artifact","ref":"artifact-x"}}}
	]}}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	got := uicomponents.WalkBindings(decl)
	require.Len(t, got, 1)
	assert.Equal(t, "page/0#body", got[0].Path)
	assert.Equal(t, "page", got[0].Region)
}

// TestWalkBindingsTreatsANamelessHookAsARegion pins the answer the browser's
// hookNameOf has to mirror: what makes a node a region is its COMPONENT TYPE,
// never whether its name prop decoded. A nameless hook opens the region "" and
// re-roots its subtree's numbering, which is why the table below is "/0#rows"
// and not the "/1.0#rows" it would answer to if the node were walked as an
// ordinary container. Validate rejects such a page, so it never reaches a
// browser — but the two walks must agree on every tree, not only the admitted
// ones, because that agreement is what lets the rejection live in exactly one
// place.
func TestWalkBindingsTreatsANamelessHookAsARegion(t *testing.T) {
	raw := []byte(`{"view":{"component":"ap:stack","children":[
	  {"component":"ap:markdown","bindings":{"body":{"source":"artifact","ref":"artifact-top"}}},
	  {"component":"oap:generative","props":{"allowedComponents":["*"]},"children":[
	    {"component":"ap:table","props":{"columns":[{"key":"a"}]},"bindings":{"rows":{"source":"tool","ref":"crm_list_leads"}}}
	  ]}
	]}}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	got := uicomponents.WalkBindings(decl)
	require.Len(t, got, 2)
	assert.Equal(t, "/0#body", got[0].Path)
	assert.Equal(t, "/0#rows", got[1].Path, "a nameless hook is still a hook: region \"\", path from the hook node")
	assert.Equal(t, "", got[1].Region)
}

func TestWalkBindingsIsDeterministicAcrossPropsInOneNode(t *testing.T) {
	raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:metric",
      "bindings":{
        "value":{"source":"tool","ref":"t","args":{}},
        "delta":{"source":"tool","ref":"t","args":{}},
        "caption":{"source":"tool","ref":"t","args":{}}}}}]}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	var first []string
	for i := 0; i < 20; i++ {
		var paths []string
		for _, b := range uicomponents.WalkBindings(decl) {
			paths = append(paths, b.Path)
		}
		if i == 0 {
			first = paths
			continue
		}
		assert.Equal(t, first, paths, "map iteration order must not leak into the walk")
	}
	assert.Equal(t, []string{"/0#caption", "/0#delta", "/0#value"}, first,
		"props are sorted by name within a node")
}

func TestParamNamesIgnoresNonStringAndUnknownComponents(t *testing.T) {
	raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:stack","children":[
      {"component":"ap:daterange","props":{"param":"window"}},
      {"component":"ap:text","props":{"text":"param"}}
    ]}}]}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"window"}, uicomponents.ParamNames(decl),
		"only a component whose registration declares ParamProp contributes")
	_ = json.RawMessage(nil)
}

// TestParamKeys pins the difference between a declared parameter NAME and the
// runtime KEYS it expands to. That difference is the whole reason an author may
// not write {"$param":"window"}: the name is declared, and no key by that name
// ever exists.
func TestParamKeys(t *testing.T) {
	raw := []byte(`{"slots":[{"name":"root","default":{"component":"ap:stack","children":[
      {"component":"ap:select","props":{"param":"span","value":"7d"}},
      {"component":"ap:daterange","props":{"param":"window"}},
      {"component":"ap:select","props":{"value":"no param name declared"}},
      {"component":"ap:text","props":{"text":"drives nothing"}}
    ]}}]}`)
	decl, err := uicomponents.ParseDeclaration(raw)
	require.NoError(t, err)

	assert.Equal(t, []string{"span", "window"}, uicomponents.ParamNames(decl),
		"ParamNames stays the DECLARED names")
	assert.Equal(t, []string{"span", "window.from", "window.to"}, uicomponents.ParamKeys(decl),
		"ParamKeys expands each name through its component's registered ParamValues")
}
