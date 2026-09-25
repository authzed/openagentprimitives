package workshopmcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// bannedInfraTerms is the Copy rule's literal denylist (design spec §2.4 /
// task-3-brief.md): a plain-language render must contain none of these,
// case-insensitively.
var bannedInfraTerms = []string{"kind:", "apiVersion:", "kubectl"}

// assertNoInfraVocabulary fails t if summary contains any banned term.
func assertNoInfraVocabulary(t *testing.T, summary string) {
	t.Helper()
	lower := strings.ToLower(summary)
	for _, term := range bannedInfraTerms {
		assert.NotContains(t, lower, strings.ToLower(term), "render_summary output must not use infrastructure vocabulary: %q", summary)
	}
}

// TestHandleRenderSummary_CandidateManifest_NoInfraVocabulary renders a
// not-yet-applied candidate (the approval-card path) and asserts the
// sentence is plain language.
func TestHandleRenderSummary_CandidateManifest_NoInfraVocabulary(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-demo123", c)

	manifest := candidateManifest("ws-demo123", "demo-tool", "search the customer directory")
	// Give it one allowlisted action so the "may use" branch is exercised too.
	spec := manifest["spec"].(map[string]any)
	spec["tools"] = []any{map[string]any{"name": "search_customers"}}

	res := callTool(t, s.handleRenderSummary, renderArgs{Manifest: manifest})
	require.False(t, res.IsError)
	body := decodeResultBody(t, res)
	summary, ok := body["summary"].(string)
	require.True(t, ok)
	require.NotEmpty(t, summary)
	assert.Contains(t, summary, "demo-tool")
	assert.Contains(t, summary, "search the customer directory")
	assert.Contains(t, summary, "search_customers")
	assertNoInfraVocabulary(t, summary)
}

// TestHandleRenderSummary_AppliedCR_NoInfraVocabulary renders an
// already-applied CR looked up by kind+name (the running-summary path) —
// covers AgentClass and SpiceboxToolspec too, since MCPServer's shape is
// already exercised above.
func TestHandleRenderSummary_AppliedCR_NoInfraVocabulary(t *testing.T) {
	ns := "ws-demo123"
	agentClass := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			DisplayName: "Demo Helper",
			Description: "answers questions about the customer directory",
		},
	}
	toolspec := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cli", Namespace: ns},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name: "demo-cli", Version: "v1", Intent: "look things up on disk",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "demo-toolkit"},
			AllowSubcommands: []string{"list", "show"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(agentClass, toolspec).Build()
	s := newApplyTestServer(ns, c)

	cases := []struct {
		name     string
		kind     string
		crName   string
		contains []string
	}{
		{
			name: "AgentClass renders as a plain-language agent description", kind: "AgentClass", crName: "demo-agent",
			contains: []string{"Demo Helper", "answers questions about the customer directory"},
		},
		{
			name: "SpiceboxToolspec renders as a plain-language ability description", kind: "SpiceboxToolspec", crName: "demo-cli",
			contains: []string{"demo-cli", "look things up on disk", "list", "show"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, s.handleRenderSummary, renderArgs{Kind: tc.kind, Name: tc.crName})
			require.False(t, res.IsError)
			body := decodeResultBody(t, res)
			summary, ok := body["summary"].(string)
			require.True(t, ok)
			for _, want := range tc.contains {
				assert.Contains(t, summary, want)
			}
			assertNoInfraVocabulary(t, summary)
		})
	}
}

// TestHandleRenderSummary_RequiresManifestOrKindName proves the tool refuses
// a call carrying neither form of input rather than guessing.
func TestHandleRenderSummary_RequiresManifestOrKindName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	s := newApplyTestServer("ws-demo123", c)

	res := callTool(t, s.handleRenderSummary, renderArgs{})
	require.True(t, res.IsError)
}
