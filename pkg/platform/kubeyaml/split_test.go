package kubeyaml_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/kubeyaml"
)

const cm = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"

func TestSplit(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKinds []string
	}{
		{
			name:      "two resources: both returned in document order",
			in:        cm + "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: b\n",
			wantKinds: []string{"ConfigMap", "Secret"},
		},
		{
			name:      "empty document between resources: skipped",
			in:        cm + "---\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: b\n",
			wantKinds: []string{"ConfigMap", "Secret"},
		},
		{
			name:      "comment-only document: skipped",
			in:        cm + "---\n# just a comment\n",
			wantKinds: []string{"ConfigMap"},
		},
		{
			name:      "kind-less but non-empty document: skipped, never returned as an empty Kind",
			in:        cm + "---\nsomeKey: someValue\n",
			wantKinds: []string{"ConfigMap"},
		},
		{
			name:      "leading and trailing separators: no phantom documents",
			in:        "---\n" + cm + "---\n",
			wantKinds: []string{"ConfigMap"},
		},
		{
			name:      "empty input: no documents, no error",
			in:        "",
			wantKinds: nil,
		},
		{
			name:      "JSON input: decoded as one document",
			in:        `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
			wantKinds: []string{"ConfigMap"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kubeyaml.Split([]byte(tc.in))
			require.NoError(t, err)

			var kinds []string
			for _, u := range got {
				kinds = append(kinds, u.GetKind())
			}
			assert.Equal(t, tc.wantKinds, kinds)
		})
	}
}

func TestSplit_MalformedYAMLErrors(t *testing.T) {
	_, err := kubeyaml.Split([]byte("apiVersion: v1\n\tkind: ConfigMap\n"))
	require.Error(t, err, "a tab-indented document is not valid YAML and must not be silently dropped")
	assert.Contains(t, err.Error(), "kubeyaml.Split: decode:")
}
