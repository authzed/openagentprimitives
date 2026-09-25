package preferences

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func js(s string) apiextv1.JSON   { return apiextv1.JSON{Raw: []byte(s)} }
func jsp(s string) *apiextv1.JSON { v := js(s); return &v }

func TestValidateSchema(t *testing.T) {
	cases := []struct {
		name    string
		schema  []v1alpha1.UserPreferenceSchema
		wantErr string // "" = valid
	}{
		{name: "valid enum with defaults: ok", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "language", Type: "enum",
			Enum:    []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de", Description: "German"}},
			Default: jsp(`"en"`),
		}}},
		{name: "duplicate name: rejected", schema: []v1alpha1.UserPreferenceSchema{
			{Name: "a", Type: "bool"}, {Name: "a", Type: "int"},
		}, wantErr: `duplicate preference "a"`},
		{name: "default fails own enum: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "language", Type: "enum",
			Enum: []v1alpha1.PreferenceEnumValue{{Value: "en"}}, Default: jsp(`"xx"`),
		}}, wantErr: `default for "language"`},
		{name: "enum members on non-enum type: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool", Enum: []v1alpha1.PreferenceEnumValue{{Value: "x"}},
		}}, wantErr: `enum is only valid for type "enum"`},
		{name: "duplicate enum value: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "enum", Enum: []v1alpha1.PreferenceEnumValue{{Value: "x"}, {Value: "x"}},
		}}, wantErr: `duplicate enum value "x"`},
		{name: "pattern on bool: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool", Pattern: "^a$",
		}}, wantErr: `pattern is only valid for string`},
		{name: "invalid regexp: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "string", Pattern: "([",
		}}, wantErr: "pattern"},
		{name: "enum type with no members: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "enum",
		}}, wantErr: "requires enum values"},
		{name: "visibility unset: ok", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool",
		}}},
		{name: "visibility self: ok", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool", Visibility: "self",
		}}},
		{name: "visibility class: ok", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool", Visibility: "class",
		}}},
		{name: "visibility bogus value: rejected", schema: []v1alpha1.UserPreferenceSchema{{
			Name: "n", Type: "bool", Visibility: "everyone",
		}}, wantErr: `visibility must be "self" or "class"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSchema(tc.schema)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateValue(t *testing.T) {
	str := v1alpha1.UserPreferenceSchema{Name: "n", Type: "string", Pattern: "^[a-z]+$"}
	enum := v1alpha1.UserPreferenceSchema{Name: "n", Type: "enum",
		Enum: []v1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "de"}}}
	list := v1alpha1.UserPreferenceSchema{Name: "n", Type: "stringList", Pattern: "^[a-z]+$"}

	cases := []struct {
		name    string
		sch     v1alpha1.UserPreferenceSchema
		value   apiextv1.JSON
		wantErr bool
	}{
		{name: "string matching pattern: ok", sch: str, value: js(`"abc"`)},
		{name: "string violating pattern: rejected", sch: str, value: js(`"ABC"`), wantErr: true},
		{name: "string given number: rejected", sch: str, value: js(`3`), wantErr: true},
		{name: "enum member: ok", sch: enum, value: js(`"de"`)},
		{name: "enum non-member: rejected", sch: enum, value: js(`"fr"`), wantErr: true},
		{name: "bool: ok", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "bool"}, value: js(`true`)},
		{name: "int given float: rejected", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "int"}, value: js(`1.5`), wantErr: true},
		{name: "int: ok", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "int"}, value: js(`42`)},
		{name: "int given quoted string: rejected", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "int"}, value: js(`"42"`), wantErr: true},
		{name: "int with trailing garbage: rejected", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "int"}, value: js(`42 xyz`), wantErr: true},
		{name: "stringList per-item pattern: rejected item", sch: list, value: js(`["ok","NO"]`), wantErr: true},
		{name: "stringList: ok", sch: list, value: js(`["ok","yes"]`)},
		{name: "string given padded null: ok", sch: str, value: js(`  null`)},
		{name: "stringList given padded null: ok", sch: list, value: js(`  null`)},
		{name: "int given padded null: ok", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "int"}, value: js(`  null`)},
		{name: "bool given padded null: ok", sch: v1alpha1.UserPreferenceSchema{Name: "n", Type: "bool"}, value: js(`  null`)},
		{name: "enum given padded null: ok", sch: enum, value: js(`  null`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateValue(tc.sch, tc.value)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
