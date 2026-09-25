package uicomponents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckForm_Values(t *testing.T) {
	fields := []FormField{{Name: "description", Kind: "textarea"}, {Name: "name"}}
	cases := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{name: "no values: valid", values: nil},
		{name: "values for declared fields: valid", values: map[string]string{"description": "a haiku agent", "name": "demo-haiku"}},
		{name: "a value for an undeclared field: refused by name", values: map[string]string{"colour": "blue"}, wantErr: `values["colour"] names no declared field`},
		{name: "a value over the field limit: refused", values: map[string]string{"description": string(make([]byte, MaxFormValueLen+1))}, wantErr: "values[\"description\"] is 4097 characters"},
		{name: "a line break in a single-line field: refused, naming the field", values: map[string]string{"name": "demo\nhaiku"}, wantErr: `values["name"] contains a line break; field "name" is not a textarea`},
		{name: "a line break in a textarea field: valid", values: map[string]string{"description": "a haiku agent\nthat posts each morning"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkForm(&FormProps{Fields: fields, Values: tc.values, Action: "describe_agent"})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
