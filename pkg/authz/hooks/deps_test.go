package hooks_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractToolIDArg(t *testing.T) {
	cases := []struct {
		name  string
		input string
		field string
		want  string
		isErr bool
	}{
		{name: "enveloped args.id present", input: `{"operation_id":"x","args":{"id":"L-140"}}`, field: "id", want: "L-140"},
		{name: "top-level fallback (non-enveloped)", input: `{"id":"L-7"}`, field: "id", want: "L-7"},
		{name: "missing field returns empty no error", input: `{"args":{"other":"v"}}`, field: "id", want: ""},
		{name: "malformed JSON errors (fail-closed signal)", input: `{not json`, field: "id", isErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hooks.ExtractToolIDArg([]byte(tc.input), tc.field)
			if tc.isErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractToolResultID(t *testing.T) {
	got, err := hooks.ExtractToolResultID(`{"id":"uuid-1"}`, "id")
	require.NoError(t, err)
	assert.Equal(t, "uuid-1", got)

	empty, err := hooks.ExtractToolResultID("", "id")
	require.NoError(t, err)
	assert.Equal(t, "", empty)

	_, err = hooks.ExtractToolResultID(`{bad`, "id")
	require.Error(t, err)
}
