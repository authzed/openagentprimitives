package labelextract_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp/labelextract"
)

func TestEvaluate_SingleBlockNoForEach(t *testing.T) {
	block := labelextract.Block{
		Tuple: labelextract.Tuple{
			ResourceType: `"crm_company"`,
			ID:           `args.id`,
			Name:         `result.name`,
		},
	}
	got, err := labelextract.Evaluate(block,
		map[string]any{"id": "42"},
		map[string]any{"name": "Acme Corp"},
	)
	require.NoError(t, err)
	assert.Equal(t, []labelextract.Extracted{
		{ResourceType: "crm_company", ID: "42", Name: "Acme Corp"},
	}, got)
}

func TestEvaluate_ForEachOverResults(t *testing.T) {
	block := labelextract.Block{
		When:    `args.objectType == "companies" && has(result.results)`,
		ForEach: `result.results`,
		Tuple: labelextract.Tuple{
			ResourceType: `"crm_company"`,
			ID:           `item.properties.hs_object_id`,
			Name:         `item.properties.name`,
		},
	}
	got, err := labelextract.Evaluate(block,
		map[string]any{"objectType": "companies"},
		map[string]any{"results": []any{
			map[string]any{"properties": map[string]any{"hs_object_id": "100", "name": "Acme"}},
			map[string]any{"properties": map[string]any{"hs_object_id": "200", "name": "Beta"}},
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, []labelextract.Extracted{
		{ResourceType: "crm_company", ID: "100", Name: "Acme"},
		{ResourceType: "crm_company", ID: "200", Name: "Beta"},
	}, got)
}

func TestEvaluate_WhenFalseSkips(t *testing.T) {
	block := labelextract.Block{
		When:    `args.objectType == "contacts"`,
		ForEach: `result.results`,
		Tuple:   labelextract.Tuple{ResourceType: `"crm_company"`, ID: `item.id`, Name: `item.name`},
	}
	got, err := labelextract.Evaluate(block,
		map[string]any{"objectType": "companies"},
		map[string]any{"results": []any{map[string]any{"id": "1", "name": "X"}}},
	)
	require.NoError(t, err)
	assert.Empty(t, got, "When=false should yield no labels")
}

func TestEvaluate_MissingFieldsSkipsTuple(t *testing.T) {
	block := labelextract.Block{
		ForEach: `result.rows`,
		Tuple:   labelextract.Tuple{ResourceType: `"crm_company"`, ID: `item.id`, Name: `item.name`},
	}
	got, err := labelextract.Evaluate(block,
		map[string]any{},
		map[string]any{"rows": []any{
			map[string]any{"id": "1", "name": "Alpha"},
			map[string]any{"id": "2"},               // missing name → skipped
			map[string]any{"id": "", "name": "Bad"}, // empty id → skipped
			map[string]any{"id": "3", "name": "Gamma"},
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, []labelextract.Extracted{
		{ResourceType: "crm_company", ID: "1", Name: "Alpha"},
		{ResourceType: "crm_company", ID: "3", Name: "Gamma"},
	}, got)
}

func TestEvaluate_NameSanitization(t *testing.T) {
	longName := ""
	for i := 0; i < 220; i++ {
		longName += "a"
	}
	block := labelextract.Block{
		Tuple: labelextract.Tuple{
			ResourceType: `"crm_company"`,
			ID:           `"1"`,
			Name:         `result.name`,
		},
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newlines collapse", "Acme\nCorp", "Acme Corp"},
		{"CR collapse", "Acme\rCorp", "Acme Corp"},
		{"CRLF collapse", "Acme\r\nCorp", "Acme Corp"},
		{"truncates at MaxNameChars-1 + ellipsis", longName, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := labelextract.Evaluate(block, nil, map[string]any{"name": tc.in})
			require.NoError(t, err)
			if tc.name == "truncates at MaxNameChars-1 + ellipsis" {
				// Just check the length and the trailing ellipsis; the
				// truncated body of 'a's plus the '…' marker is what we
				// validate, not exact equality with `tc.want` (left
				// empty intentionally as the assertion is structural).
				assert.Equal(t, labelextract.MaxNameChars, len([]rune(got[0].Name)),
					"truncated name should be MaxNameChars runes total")
				assert.True(t, strings.HasSuffix(got[0].Name, "…"),
					"truncated name should end with ellipsis")
				return
			}
			assert.Equal(t, tc.want, got[0].Name)
		})
	}
}

func TestEvaluate_BadCEL(t *testing.T) {
	block := labelextract.Block{
		Tuple: labelextract.Tuple{
			ResourceType: `"crm_company"`,
			ID:           `args.id`,
			Name:         `args.! totally broken`, // syntax error
		},
	}
	_, err := labelextract.Evaluate(block, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tuple.name compile")
}

// TestEvaluate_ForEachMacro pins the case that a raw v.Value().([]any)
// assertion cannot serve: a list MACRO (filter, map) builds a new list whose
// Value() is []ref.Val, not []any, so asserting the native type aborted the
// whole block with "forEach: expected list, got []ref.Val". Both macros are
// exercised because they build their result lists by different paths, and a
// conversion that handled only one would still look correct here.
func TestEvaluate_ForEachMacro(t *testing.T) {
	results := []any{
		map[string]any{"properties": map[string]any{"hs_object_id": "100", "name": "Acme"}},
		map[string]any{"properties": map[string]any{"hs_object_id": "200"}}, // no name
		map[string]any{"properties": map[string]any{"hs_object_id": "300", "name": "Gamma"}},
	}

	cases := []struct {
		name    string
		forEach string
		tuple   labelextract.Tuple
		want    []labelextract.Extracted
	}{
		{
			name:    "filter macro: skips the record missing a property, labels the rest",
			forEach: `result.results.filter(r, has(r.properties.name))`,
			tuple: labelextract.Tuple{
				ResourceType: `"crm_company"`,
				ID:           `item.properties.hs_object_id`,
				Name:         `item.properties.name`,
			},
			want: []labelextract.Extracted{
				{ResourceType: "crm_company", ID: "100", Name: "Acme"},
				{ResourceType: "crm_company", ID: "300", Name: "Gamma"},
			},
		},
		{
			name:    "map macro: projects each record, labels every projection",
			forEach: `result.results.map(r, r.properties)`,
			tuple: labelextract.Tuple{
				ResourceType: `"crm_company"`,
				ID:           `item.hs_object_id`,
				Name:         `has(item.name) ? item.name : ""`,
			},
			want: []labelextract.Extracted{
				{ResourceType: "crm_company", ID: "100", Name: "Acme"},
				{ResourceType: "crm_company", ID: "300", Name: "Gamma"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := labelextract.Evaluate(
				labelextract.Block{ForEach: tc.forEach, Tuple: tc.tuple},
				map[string]any{"objectType": "companies"},
				map[string]any{"results": results},
			)
			require.NoError(t, err, "a macro-built list must be accepted as a forEach source")
			assert.Equal(t, tc.want, got)
		})
	}
}
