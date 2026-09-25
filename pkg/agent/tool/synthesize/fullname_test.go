package synthesize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
)

func TestFullName(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		entry  string
		want   string
	}{
		{name: "prefix joined with an underscore", prefix: "k8s", entry: "get_pods", want: "k8s_get_pods"},
		{name: "empty prefix leaves the entry name alone", prefix: "", entry: "get_pods", want: "get_pods"},
		{name: "the JOINED name is normalized, not each half", prefix: "K8s.Box", entry: "Get Pods", want: "k8s-box_get-pods"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, synthesize.FullName(tc.prefix, tc.entry))
		})
	}
}

// TestBuild_NamesToolsExactlyAsFullNamePredicts is the anti-drift guard.
// FullName exists so a caller can predict a tool's LLM-facing name without
// building it, and that is worth something only while Build derives the name
// through the same function. A second copy of "join, then normalize" drifts
// silently, and the symptom is a predicted name matching no tool at all —
// which reads as the tool being missing rather than the prediction being wrong.
func TestBuild_NamesToolsExactlyAsFullNamePredicts(t *testing.T) {
	factory, seen := makeFactory(t)
	entries := []synthesize.Entry{
		{Name: "Get Pods", Factory: factory},
		{Name: "list", Factory: factory},
	}
	_, err := synthesize.Build(fakeSource{prefix: "K8s.Box", entries: entries})
	require.NoError(t, err)

	predicted := make([]string, 0, len(entries))
	for _, e := range entries {
		predicted = append(predicted, synthesize.FullName("K8s.Box", e.Name))
	}
	assert.Equal(t, predicted, seen(), "Build must name a tool exactly what FullName predicts")
}
