package install

import (
	"context"
	"os"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

func knownGraph(t *testing.T) *oap.Bundle {
	t.Helper()
	helper := questionBundle("helper", []oap.Question{{
		Name: "region", Type: oap.QString, Prompt: "Region",
		Binding: []oap.Binding{{Target: "AgentClass/helper#spec.region"}},
	}})
	reviewer := questionBundle("reviewer", []oap.Question{{
		Name: "voice", Type: oap.QString, Prompt: "Voice",
		Binding: []oap.Binding{{Target: "AgentClass/reviewer#spec.voice"}},
	}})
	reviewer.Manifest.Requires.Secrets = []oap.RequiredSecret{{Name: "review-token", Keys: []string{"token"}}}
	attachKnownDependency(t, reviewer, oap.DependencyPath{"reviewer"}, helper)

	root := questionBundle("captain", []oap.Question{{
		Name: "repos", Type: oap.QResourceList, Prompt: "Repositories",
		Binding: []oap.Binding{{Target: "AgentClass/captain#spec.repos"}},
	}, {
		Name: "root.with.dots", Type: oap.QString, Prompt: "Dotted root key",
		Binding: []oap.Binding{{Target: "AgentClass/captain#spec.dotted"}},
	}})
	root.Manifest.Requires.Secrets = []oap.RequiredSecret{{Name: "root-token", Keys: []string{"api-key"}}}
	attachKnownDependency(t, root, nil, reviewer)
	require.NoError(t, oap.ValidateDependencyGraph(root))
	return root
}

func questionBundle(name string, questions []oap.Question) *oap.Bundle {
	return &oap.Bundle{
		Manifest: &oap.Manifest{
			OapFormatVersion: "1",
			Agent:            oap.Agent{Name: name, Version: "1.0.0"},
			Questions:        questions,
		},
		Manifests: []byte("apiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentClass\nmetadata:\n  name: " + name + "\nspec:\n  systemPrompt:\n    inline: fixture\n"),
	}
}

func attachKnownDependency(t *testing.T, parent *oap.Bundle, parentPath oap.DependencyPath, child *oap.Bundle) {
	t.Helper()
	packed, err := oap.Pack(child)
	require.NoError(t, err)
	descriptor := oap.RequiredAgent{
		Name: child.Manifest.Agent.Name, Version: child.Manifest.Agent.Version,
		MediaType: oap.DependencyMediaType, Digest: digest.FromBytes(packed).String(),
	}
	path := append(append(oap.DependencyPath(nil), parentPath...), child.Manifest.Agent.Name)
	parent.Manifest.Requires.Agents = append(parent.Manifest.Requires.Agents, descriptor)
	parent.Dependencies = append(parent.Dependencies, &oap.Dependency{
		Descriptor: descriptor, Path: path, Bundle: child, Packed: packed,
	})
	parent.Manifests = append(parent.Manifests, []byte("  subagents:\n  - "+child.Manifest.Agent.Name+"\n")...)
}

func TestProjectValuesForDependency(t *testing.T) {
	values := GraphValues{
		"repos": []any{"acme/widgets"},
		"agents": map[string]any{
			"reviewer": map[string]any{
				"voice":  "theatrical",
				"agents": map[string]any{"helper": map[string]any{"region": "us-east"}},
			},
		},
	}
	root, err := ProjectValues(values, nil, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, []any{"acme/widgets"}, root["repos"])
	assert.NotContains(t, root, "agents")

	child, err := ProjectValues(values, oap.DependencyPath{"reviewer"}, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, "theatrical", child["voice"])
	assert.NotContains(t, child, "agents")

	grandchild, err := ProjectValues(values, oap.DependencyPath{"reviewer", "helper"}, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, "us-east", grandchild["region"])
}

func TestProjectValuesLoadsNestedYAML(t *testing.T) {
	path := t.TempDir() + "/values.yaml"
	require.NoError(t, os.WriteFile(path, []byte("repos:\n- acme/widgets\nagents:\n  reviewer:\n    voice: theatrical\n    agents:\n      helper:\n        region: us-east\n"), 0o600))

	values, err := LoadGraphValues(path)
	require.NoError(t, err)
	child, err := ProjectValues(values, oap.DependencyPath{"reviewer", "helper"}, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, "us-east", child["region"])
}

func TestProjectValuesKeepsAuthoredScalarAgentsQuestionAtRoot(t *testing.T) {
	root := knownGraph(t)
	root.Manifest.Questions = append(root.Manifest.Questions, oap.Question{
		Name: "agents", Type: oap.QString, Prompt: "Agent mode",
		Binding: []oap.Binding{{Target: "AgentClass/captain#spec.agentMode"}},
	})

	projected, err := ProjectValues(GraphValues{"agents": "coordinated"}, nil, root)
	require.NoError(t, err)
	assert.Equal(t, "coordinated", projected["agents"],
		"a non-map value belongs to the authored question; only a map is the dependency namespace")
}

func TestProjectValuesRejectsInvalidTreeBeforeProjection(t *testing.T) {
	tests := []struct {
		name    string
		values  GraphValues
		wantErr string
	}{
		{name: "unknown child", values: GraphValues{"agents": map[string]any{"werewolf": map[string]any{"voice": "quiet"}}}, wantErr: "werewolf"},
		{name: "unknown question", values: GraphValues{"agents": map[string]any{"reviewer": map[string]any{"costume": "cape"}}}, wantErr: "costume"},
		{name: "agents is not a map", values: GraphValues{"agents": "reviewer"}, wantErr: "must be a map"},
		{name: "nested agents is not a map", values: GraphValues{"agents": map[string]any{"reviewer": map[string]any{"agents": []any{"helper"}}}}, wantErr: "must be a map"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ProjectValues(tc.values, nil, knownGraph(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestProjectValuesAcceptsReservedQuestionsPerNode(t *testing.T) {
	values := GraphValues{
		"capacity.captain.cpu":                "500m",
		"requires.secrets.root-token.api-key": "root-private",
		"agents": map[string]any{"reviewer": map[string]any{
			"capacity.reviewer.memory":            "1Gi",
			"requires.secrets.review-token.token": "child-private",
		}},
	}
	root, err := ProjectValues(values, nil, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, "500m", root["capacity.captain.cpu"])
	child, err := ProjectValues(values, oap.DependencyPath{"reviewer"}, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, "1Gi", child["capacity.reviewer.memory"])
}

func TestProjectValuesErrorsNeverContainSecretValues(t *testing.T) {
	secret := "sk-graph-value-that-must-not-leak"
	_, err := ProjectValues(GraphValues{
		"agents": map[string]any{"reviewer": map[string]any{"unknown-secret": secret}},
	}, nil, knownGraph(t))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, strings.ToLower(err.Error()), "graph-value-that-must-not-leak")
}

func TestGraphQuestionsParseSetKeepsLocalNames(t *testing.T) {
	sets, err := ParseGraphSet(map[string]string{
		"root.with.dots":                       "root-value",
		"agents.reviewer.voice":                "theatrical",
		"agents.reviewer.agents.helper.region": "us-east",
		"agents.reviewer.capacity.cpu":         "500m",
	}, knownGraph(t))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"root.with.dots": "root-value"}, sets[""])
	assert.Equal(t, map[string]string{"voice": "theatrical", "capacity.cpu": "500m"}, sets["reviewer"])
	assert.Equal(t, map[string]string{"region": "us-east"}, sets["reviewer > helper"])
}

func TestGraphQuestionsParseSetKeepsRootQuestionBeginningWithAgents(t *testing.T) {
	root := knownGraph(t)
	root.Manifest.Questions = append(root.Manifest.Questions, oap.Question{
		Name: "agents.legacy.mode", Type: oap.QString, Prompt: "Legacy root option",
		Binding: []oap.Binding{{Target: "AgentClass/captain#spec.legacyMode"}},
	})
	sets, err := ParseGraphSet(map[string]string{"agents.legacy.mode": "compatible"}, root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"agents.legacy.mode": "compatible"}, sets[""])
}

func TestGraphQuestionsParseSetKeepsDependencyQuestionBeginningWithAgents(t *testing.T) {
	root := knownGraph(t)
	child := root.Dependencies[0].Bundle
	child.Manifest.Questions = append(child.Manifest.Questions, oap.Question{
		Name: "agents.legacy.mode", Type: oap.QString, Prompt: "Legacy child option",
		Binding: []oap.Binding{{Target: "AgentClass/reviewer#spec.legacyMode"}},
	})
	sets, err := ParseGraphSet(map[string]string{
		"agents.reviewer.agents.legacy.mode": "compatible",
	}, root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"agents.legacy.mode": "compatible"}, sets["reviewer"])
}

func TestProjectValuesNestedErrorUsesCompleteRootPath(t *testing.T) {
	_, err := ProjectValues(GraphValues{
		"agents": map[string]any{"reviewer": map[string]any{"unknown": "value"}},
	}, nil, knownGraph(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "captain > reviewer")
}

func TestGraphQuestionsProbePhysicalSecretNames(t *testing.T) {
	root := knownGraph(t)
	names, err := instance.BuildGraphNames("captain", root)
	require.NoError(t, err)
	rootResourceNames, err := rootResourceNamesForInstall(root, "")
	require.NoError(t, err)
	physical := names["reviewer"] + "-review-token"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: physical},
	}).Build()

	groups, notices, err := ExpandQuestionGroups(context.Background(), c, root, "agents", names, rootResourceNames)
	require.NoError(t, err)
	assert.Empty(t, notices)
	require.Len(t, groups, 3)
	assert.Equal(t, oap.DependencyPath(nil), groups[0].Path)
	assert.Equal(t, oap.DependencyPath{"reviewer"}, groups[1].Path)
	assert.Equal(t, oap.DependencyPath{"reviewer", "helper"}, groups[2].Path)
	assert.Equal(t, []string{"voice"}, graphQuestionNames(groups[1].Questions),
		"the physical private Secret exists, so the child credential must not be asked")
}

func TestGraphQuestionsAcceptsOwnershipRootNameForPhysicalSecretProbe(t *testing.T) {
	root := knownGraph(t)
	names, err := instance.BuildGraphNames("custom", root)
	require.NoError(t, err)
	rootResourceNames, err := rootResourceNamesForInstall(root, "custom")
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "custom-root-token"},
	}).Build()

	groups, _, err := ExpandQuestionGroups(context.Background(), c, root, "agents", names, rootResourceNames)
	require.NoError(t, err)
	require.NotEmpty(t, groups)
	assert.NotContains(t, graphQuestionNames(groups[0].Questions), "requires.secrets.root-token.api-key",
		"BuildGraphNames exposes the ownership root; question expansion must probe the physical root Secret")
}

func TestGraphQuestionsProbePlannedRootSecretForAmbiguousExplicitNames(t *testing.T) {
	for _, explicitName := range []string{"captain", "private-captain"} {
		t.Run(explicitName, func(t *testing.T) {
			root := knownGraph(t)
			_, names, err := graphNamesForInstall(root, explicitName)
			require.NoError(t, err)
			rootResourceNames, err := rootResourceNamesForInstall(root, explicitName)
			require.NoError(t, err)
			// Question expansion receives the ownership root for path
			// qualification separately from the exact resource name map.
			names[""] = explicitName
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: explicitName + "-root-token"},
			}).Build()

			groups, _, err := ExpandQuestionGroups(context.Background(), c, root, "agents", names, rootResourceNames)
			require.NoError(t, err)
			require.NotEmpty(t, groups)
			assert.NotContains(t, graphQuestionNames(groups[0].Questions), "requires.secrets.root-token.api-key",
				"question expansion must probe the exact root Secret name planned for the explicit install name")
		})
	}
}

func graphQuestionNames(qs []oap.Question) []string {
	names := make([]string, 0, len(qs))
	for _, q := range qs {
		names = append(names, q.Name)
	}
	return names
}
