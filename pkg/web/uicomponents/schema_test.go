package uicomponents_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/registry"
)

func TestEveryRegisteredComponentEmitsAParseableSchema(t *testing.T) {
	for _, c := range registry.All() {
		t.Run(c.Type, func(t *testing.T) {
			raw, err := c.Schema()
			require.NoError(t, err, "schema emission must not fail for a registered component")

			var doc map[string]any
			require.NoError(t, json.Unmarshal(raw, &doc), "the schema must be valid JSON")
			assert.NotEmpty(t, doc, "the schema must not be an empty document")

			// Every registered v1 component's props struct declares at least one
			// field, so a real reflection must always surface a non-empty
			// "properties" map. This is the check that would actually catch a
			// Schema() collapsed to something like {"type":"object"} — a doc
			// that is non-empty (so the weaker check above passes) but tells the
			// agent nothing about what it may write.
			props, ok := doc["properties"]
			require.True(t, ok, "schema must declare a properties map")
			propsMap, ok := props.(map[string]any)
			require.True(t, ok, "properties must be a JSON object")
			assert.NotEmpty(t, propsMap, "component %q has a non-empty props struct; its schema must list at least one property", c.Type)

			// Name correctness, for every component, not just ap:markdown below:
			// the expected set is derived from the SAME source of truth Schema()
			// itself reflects over (c.Props's own json tags), so this is not a
			// second hand-maintained list to drift from the first. Without this,
			// a mutation that scrambled every component's emitted property names
			// (e.g. always emitting the receiver's own field names but under the
			// wrong component, or a stale cached schema) would only be caught by
			// the single ap:markdown-specific assertion in
			// TestSchemaDescribesADeclaredProp — invisible for the other 18 types,
			// and gone entirely if that test were ever deleted or ap:markdown left
			// the vocabulary.
			assert.ElementsMatch(t, jsonFieldNames(c.Props), keysOfAny(propsMap),
				"component %q: emitted schema properties must be exactly its props struct's json field names", c.Type)
		})
	}
}

// jsonFieldNames returns the top-level json field names of a props struct
// value, in the same terms Schema()'s reflector derives property keys from:
// exported fields only, tag name before the first comma, "-" (opt-out)
// skipped. It intentionally does not recurse into nested struct/slice-element
// types (e.g. TableProps.Columns' element type TableColumn) — those surface
// as nested schemas under the same top-level property name, not as
// additional top-level properties.
func jsonFieldNames(props any) []string {
	t := reflect.TypeOf(props)
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported field: invisible to both json and the reflector
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSchemaPublishesTheLegalValuesOfAConstrainedProp is the tripwire for the
// jsonschema struct tags in components.go. Without them every constrained prop
// reaches the agent as a bare {"type":"string"} — it is told "gap is a string",
// writes gap:"large", the platform accepts it (validateProps is type-shaped by
// design), and the renderer falls back to "md" with nothing anywhere reporting
// that the value was not one the vocabulary has.
//
// The rows are the shapes worth pinning, not every constrained prop: a string
// enum, a numeric range, an enum on a field NESTED in a slice element (where
// the reflector has to descend), and the two variant lists that must not
// silently converge (a button has six cva variants, a badge four).
func TestSchemaPublishesTheLegalValuesOfAConstrainedProp(t *testing.T) {
	cases := []struct {
		name      string
		component string
		// path locates the property within the emitted document: the top-level
		// prop name, plus "items" hops for a slice-element field.
		path []string
		enum []any
		min  float64
		max  float64
	}{
		{
			name:      "ap:stack gap: the four spacing steps, not any string",
			component: "ap:stack", path: []string{"gap"},
			enum: []any{"none", "sm", "md", "lg"},
		},
		{
			name:      "ap:chart kind: the three chart shapes the renderer dispatches on",
			component: "ap:chart", path: []string{"kind"},
			enum: []any{"line", "bar", "area"},
		},
		{
			name:      "ap:table column align: an enum nested inside a slice element",
			component: "ap:table", path: []string{"columns", "items", "align"},
			enum: []any{"left", "right", "center"},
		},
		{
			name:      "ap:button variant: all six cva variants the renderer accepts",
			component: "ap:button", path: []string{"variant"},
			enum: []any{"default", "secondary", "destructive", "outline", "ghost", "link"},
		},
		{
			name:      "ap:badge variant: four — badge.tsx declares no ghost or link",
			component: "ap:badge", path: []string{"variant"},
			enum: []any{"default", "secondary", "destructive", "outline"},
		},
		{
			name:      "ap:grid columns: bounded to the 1..12 range the renderer clamps to",
			component: "ap:grid", path: []string{"columns"},
			min: 1, max: 12,
		},
		{
			name:      "ap:heading level: bounded to the 1..4 range the renderer clamps to",
			component: "ap:heading", path: []string{"level"},
			min: 1, max: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := registry.Get(tc.component)
			require.True(t, ok, "component must be registered")
			raw, err := c.Schema()
			require.NoError(t, err)

			var doc map[string]any
			require.NoError(t, json.Unmarshal(raw, &doc))
			node := propertySchema(t, doc, tc.path)

			if tc.enum != nil {
				assert.Equal(t, tc.enum, node["enum"],
					"the agent must be told the legal values, in order, not merely the type")
				return
			}
			assert.Equal(t, tc.min, node["minimum"], "the schema must publish the lower bound")
			assert.Equal(t, tc.max, node["maximum"], "the schema must publish the upper bound")
		})
	}
}

// propertySchema walks an emitted schema down to one property's own subschema.
// Each path element is a property name, except the literal "items", which
// descends into an array's element schema.
func propertySchema(t *testing.T, doc map[string]any, path []string) map[string]any {
	t.Helper()
	node := doc
	for _, step := range path {
		if step == "items" {
			items, ok := node["items"].(map[string]any)
			require.True(t, ok, "expected an array schema with an items subschema at %v", path)
			node = items
			continue
		}
		props, ok := node["properties"].(map[string]any)
		require.True(t, ok, "expected a properties map while resolving %v", path)
		next, ok := props[step].(map[string]any)
		require.True(t, ok, "schema must describe the property %q (path %v)", step, path)
		node = next
	}
	return node
}

func TestVocabularySchemaCoversEveryRegisteredType(t *testing.T) {
	raw, err := uicomponents.VocabularySchema()
	require.NoError(t, err)

	var byType map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &byType))

	// The vocabulary schema includes only non-structural types; structural
	// types like oap:generative are never published to the agent.
	var publishedKeys []string
	for _, c := range registry.All() {
		if !c.Structural {
			publishedKeys = append(publishedKeys, c.Type)
		}
	}

	assert.ElementsMatch(t, publishedKeys, keysOf(byType),
		"the agent must be told about exactly the types it may use — no more, no fewer")
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSchemaDescribesADeclaredProp(t *testing.T) {
	c, ok := registry.Get("ap:markdown")
	require.True(t, ok)

	raw, err := c.Schema()
	require.NoError(t, err)
	assert.Contains(t, string(raw), "body",
		"a prop the agent is expected to set must appear in the published schema")

	// Structural check, not just substring: "body" must appear as an actual
	// property key under "properties", not merely somewhere in the raw JSON
	// (a description string, a $defs name, etc. could also contain the
	// substring "body" without the schema actually describing the prop).
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	props, ok := doc["properties"].(map[string]any)
	require.True(t, ok, "properties must be a JSON object")
	assert.Contains(t, props, "body",
		"ap:markdown's Body field must be published as the \"body\" property")
}

func TestVocabularySchemaOmitsStructuralTypes(t *testing.T) {
	raw, err := uicomponents.VocabularySchema()
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	_, published := out[uicomponents.GenerativeType]
	assert.False(t, published, "the agent must never be told it can write a hook")
	_, stack := out["ap:stack"]
	assert.True(t, stack, "ordinary types are still published")
}

func TestVocabularySchemaPublishesQuestionAndProgress(t *testing.T) {
	raw, err := uicomponents.VocabularySchema()
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	for _, typ := range []string{"ap:question", "ap:progress"} {
		_, ok := out[typ]
		assert.True(t, ok, "%s is agent-writable and must be published", typ)
	}
	assert.Contains(t, string(out["ap:question"]), `"enum":["text","choice"]`, "the kind enum is what tells the agent its two shapes")
}
