package builderbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

const viewJSON = `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}`

func TestSpliceCompiledView(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{"an AgentUI with actions only gains spec.view", "apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentUI\nmetadata: {name: demo-ui}\nspec:\n  displayName: Demo\n  actions:\n    - {name: go, prompt: \"Go.\"}\n", ""},
		{"an AgentUI that already carries spec.view is refused", "kind: AgentUI\nspec:\n  view: {component: ap:stack}\n", "already carries spec.view"},
		{"an AgentUI that still carries spec.slots is refused", "kind: AgentUI\nspec:\n  slots: [{name: brief, agentWritable: true}]\n", "still carries spec.slots"},
		{"an AgentUI with no spec is refused", "kind: AgentUI\nmetadata: {name: x}\n", "has no spec"},
		{"an AgentUI followed by a second document is refused", "kind: AgentUI\nspec:\n  displayName: Demo\n---\nkind: ConfigMap\nmetadata: {name: x}\n", "must be a single document"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := spliceCompiledView([]byte(tc.manifest), json.RawMessage(viewJSON))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			var obj struct {
				Spec struct {
					DisplayName string           `json:"displayName"`
					Actions     []map[string]any `json:"actions"`
					View        json.RawMessage  `json:"view"`
					Slots       json.RawMessage  `json:"slots"`
				} `json:"spec"`
			}
			require.NoError(t, yaml.Unmarshal(out, &obj))
			assert.Equal(t, "Demo", obj.Spec.DisplayName, "every other spec field survives")
			assert.Len(t, obj.Spec.Actions, 1)
			assert.JSONEq(t, viewJSON, string(obj.Spec.View))
			assert.Nil(t, obj.Spec.Slots)
		})
	}
}

func TestSpliceCompiledViewLeavesOtherKindsByteIdentical(t *testing.T) {
	in := []byte("kind: AgentClass\nmetadata: {name: demo}\nspec: {displayName: Demo}\n")
	out, err := spliceCompiledView(in, json.RawMessage(viewJSON))
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestSpliceCompiledViewRefusesMalformedView(t *testing.T) {
	_, err := spliceCompiledView([]byte("kind: AgentUI\nspec: {displayName: Demo}\n"), json.RawMessage(`{not json`))
	require.ErrorContains(t, err, "compiled view")
}

// TestCheckCompiledViewFresh pins the sidecar guard: `mage ui:compile` writes
// the hash of the source it compiled beside the view, and the Go side refuses
// a view whose sidecar no longer matches src/ui/page.tsx. Without it, editing
// the page and forgetting to recompile ships the old page with every suite
// green.
func TestCheckCompiledViewFresh(t *testing.T) {
	page := []byte("export default <ap:x />;\n")
	sum := sha256.Sum256(page)
	hexSum := hex.EncodeToString(sum[:])

	cases := []struct {
		name    string
		page    []byte
		sha     []byte
		wantErr string
	}{
		{"a sidecar written for this page passes", page, []byte(hexSum + "\n"), ""},
		{"surrounding whitespace in the sidecar is tolerated", page, []byte("  " + hexSum + " \n\n"), ""},
		{"a one-byte edit to the page is stale", append(page, ' '), []byte(hexSum + "\n"), "is stale"},
		{"an empty sidecar is stale", page, nil, "is stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCompiledViewFresh(tc.page, tc.sha)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.ErrorContains(t, err, "mage ui:compile", "the refusal names the target that fixes it")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestCompiledViewIsFreshInTheEmbeddedBundle is the one that fires in
// `mage test:unit`: it runs the same check over the committed, embedded files,
// so a page.tsx edit without `mage ui:compile` fails the Go gate rather than
// shipping a stale page.
func TestCompiledViewIsFreshInTheEmbeddedBundle(t *testing.T) {
	page, err := fs.ReadFile(srcFS, pageSourcePath)
	require.NoError(t, err, "the page source is embedded")
	sha, err := fs.ReadFile(srcFS, compiledViewShaPath)
	require.NoError(t, err, "the compiled view's sidecar is embedded (run mage ui:compile)")
	require.NoError(t, checkCompiledViewFresh(page, sha))
}
