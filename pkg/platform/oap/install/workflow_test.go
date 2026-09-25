package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

type workflowCredentialError struct{ secret string }

func (e *workflowCredentialError) Error() string {
	return "provider rejected credential " + e.secret
}

type workflowRollbackOrderClient struct {
	client.Client
	operations *[]string
	paths      map[string]string
}

func (c *workflowRollbackOrderClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "AgentClass" {
		*c.operations = append(*c.operations, "bundle:"+c.paths[obj.GetName()])
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func workflowAnswers() GraphAnswers {
	return GraphAnswers{
		Values: GraphValues{
			"repos":                               []any{"acme/widgets"},
			"root.with.dots":                      "root-answer",
			"requires.secrets.root-token.api-key": "root-secret-value",
			"capacity.cpu":                        "500m",
			"agents": map[string]any{"reviewer": map[string]any{
				"voice":                               "theatrical",
				"requires.secrets.review-token.token": "child-secret-value",
				"capacity.cpu":                        "600m",
				"agents": map[string]any{"helper": map[string]any{
					"region": "us-east", "capacity.cpu": "700m",
				}},
			}},
		},
		Sets: map[string]map[string]string{},
	}
}

// A regression here catches the production bug where Workflow always supplied
// its authored ResourceNames map to Prepare, so the post-overlay alias mapped a
// valid metadata.name answer straight back to the authored physical name.
func TestWorkflowPlansAnsweredMetadataNamesBeforeGraphIdentity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requested     string
		wantRoot      string
		wantRootClass string
		wantChild     string
	}{
		{name: "answered root is the install identity", wantRoot: "custom", wantRootClass: "custom", wantChild: "custom-worker"},
		{name: "explicit install name prefixes the answer", requested: "prefix", wantRoot: "prefix", wantRootClass: "prefix-custom", wantChild: "prefix-worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := questionBundle("worker", []oap.Question{{
				Name: "child-name", Type: oap.QString, Prompt: "Child name",
				Binding: []oap.Binding{{Target: "AgentClass/worker#metadata.name"}},
			}})
			root := questionBundle("demo", []oap.Question{{
				Name: "root-name", Type: oap.QString, Prompt: "Root name",
				Binding: []oap.Binding{{Target: "AgentClass/demo#metadata.name"}},
			}})
			attachKnownDependency(t, root, nil, child)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
			w := &Workflow{Client: c, Options: InstallOpts{Namespace: "agents", Name: tc.requested}}

			plan, err := w.Plan(context.Background(), root, GraphAnswers{Values: GraphValues{
				"root-name": "custom",
				"agents":    map[string]any{"worker": map[string]any{"child-name": "child-custom"}},
			}})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRoot, plan.RootName)
			require.Len(t, plan.Nodes, 2)
			byPath := map[string]*PreparedNode{}
			for _, node := range plan.Nodes {
				byPath[node.Input.Path.String()] = node
			}
			assert.Equal(t, tc.wantRootClass, byPath[""].Prepared.Owner.Name)
			assert.Equal(t, tc.wantChild, byPath["worker"].Prepared.Owner.Name,
				"a child answer changes its local source identity, not its private graph identity")
			assert.Equal(t, tc.wantChild, byPath["worker"].Input.ResourceNames["AgentClass/child-custom"])
			assert.Equal(t, tc.wantRoot, byPath[""].Prepared.CRs[0].GetLabels()[instance.LabelInstall])
			assert.Equal(t, tc.wantRoot, byPath["worker"].Prepared.CRs[0].GetLabels()[instance.LabelInstall])
			roster, found, err := unstructured.NestedStringSlice(byPath[""].Prepared.CRs[0].Object, "spec", "subagents")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, []string{tc.wantChild}, roster)
		})
	}
}

func TestWorkflowInteractiveSkippedMetadataNamesAreNotAskedAgain(t *testing.T) {
	child := questionBundle("worker", []oap.Question{
		{
			Name: "child-name", Type: oap.QString, Prompt: "Child name", Required: boolPtr(false),
			Binding: []oap.Binding{{Target: "AgentClass/worker#metadata.name"}},
		},
		{
			Name: "child-description", Type: oap.QString, Prompt: "Child description",
			Binding: []oap.Binding{{Target: "AgentClass/worker#spec.description"}},
		},
	})
	root := questionBundle("demo", []oap.Question{
		{
			Name: "root-name", Type: oap.QString, Prompt: "Root name", Required: boolPtr(false),
			Binding: []oap.Binding{{Target: "AgentClass/demo#metadata.name"}},
		},
		{
			Name: "root-description", Type: oap.QString, Prompt: "Root description",
			Binding: []oap.Binding{{Target: "AgentClass/demo#spec.description"}},
		},
		{
			Name: "root-token", Type: oap.QSecret, Prompt: "Root token",
			Secret: &oap.SecretQuestion{CreateSecret: &oap.SecretTarget{Name: "root-token", Key: "token"}},
		},
	})
	attachKnownDependency(t, root, nil, child)
	present, out := scriptedPresentation(t, "\n\nroot description\nprivate-root-token\nchild description\n")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	w := &Workflow{Client: c, Options: InstallOpts{Namespace: "agents"}}

	plan, err := w.Plan(context.Background(), root, GraphAnswers{
		Interactive: true, QuestionOptions: []ResolveOption{present},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out.String(), "Root name"), "the skipped root identity question is complete")
	assert.Equal(t, 1, strings.Count(out.String(), "Child name"), "the skipped child identity question is complete")
	assert.Contains(t, out.String(), "Root description")
	assert.Contains(t, out.String(), "Child description")
	assert.NotContains(t, fmt.Sprintf("%#v", plan), "private-root-token", "identity planning must not retain secret input")

	_, err = w.Execute(context.Background(), plan)
	require.NoError(t, err)
	var installedRoot v1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "demo"}, &installedRoot))
	assert.Equal(t, "root description", installedRoot.Spec.Description)
	var installedChild v1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "demo-worker"}, &installedChild))
	assert.Equal(t, "child description", installedChild.Spec.Description)
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "root-token"}, &secret))
	assert.Equal(t, "private-root-token", secret.StringData["token"])
}

func TestWorkflowInteractiveAnsweredMetadataNameIsTheOnlyQuestion(t *testing.T) {
	root := questionBundle("demo", []oap.Question{{
		Name: "root-name", Type: oap.QString, Prompt: "Root name", Required: boolPtr(false),
		Binding: []oap.Binding{{Target: "AgentClass/demo#metadata.name"}},
	}})
	present, _ := scriptedPresentation(t, "custom\n")
	w := &Workflow{
		Client:  fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Options: InstallOpts{Namespace: "agents"},
	}

	plan, err := w.Plan(context.Background(), root, GraphAnswers{
		Interactive: true, QuestionOptions: []ResolveOption{present},
	})
	require.NoError(t, err)
	require.Len(t, plan.Nodes, 1)
	assert.Equal(t, "custom", plan.Nodes[0].Prepared.Owner.Name)
}

func TestWorkflowInteractiveAnsweredMetadataNameRemainsInValidationArgs(t *testing.T) {
	root := questionBundle("demo", []oap.Question{
		{
			Name: "agentName", Type: oap.QString, Prompt: "Agent name",
			Binding: []oap.Binding{{Target: "AgentClass/demo#metadata.name"}},
		},
		{
			Name: "description", Type: oap.QString, Prompt: "Description",
			Validation: `args.description.startsWith(args.agentName)`,
			Binding:    []oap.Binding{{Target: "AgentClass/demo#spec.description"}},
		},
	})
	present, out := scriptedPresentation(t, "custom\ncustom agent\n")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	w := &Workflow{Client: c, Options: InstallOpts{Namespace: "agents"}}

	plan, err := w.Plan(context.Background(), root, GraphAnswers{
		Interactive: true, QuestionOptions: []ResolveOption{present},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out.String(), "Agent name"))
	assert.Equal(t, 1, strings.Count(out.String(), "Description"))
	_, err = w.Execute(context.Background(), plan)
	require.NoError(t, err)
	var installed v1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "agents", Name: "custom"}, &installed))
	assert.Equal(t, "custom agent", installed.Spec.Description)
}

func workflowForeignAgentClasses(t *testing.T, root *oap.Bundle) []client.Object {
	t.Helper()
	names, err := instance.BuildGraphNames("captain", root)
	require.NoError(t, err)
	objects := make([]client.Object, 0, len(names))
	for _, name := range names {
		objects = append(objects, &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agents"}})
	}
	return objects
}

func workflowCapacityQuestion(node NodeContext) []oap.Question {
	return []oap.Question{{
		Name: "capacity.cpu", Type: oap.QString, Prompt: "CPU",
		Binding: []oap.Binding{{Target: "AgentClass/" + node.PhysicalName + "#spec.capacity"}},
	}}
}

func TestWorkflowPlansEveryNodeBeforeMutationAndExecutesChannels(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(workflowForeignAgentClasses(t, root)...).Build()}
	var operations []string
	contexts := map[string]NodeContext{}
	wizardSecret := "channel-answer-that-must-stay-private"
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents", SourceKind: "registry", SourceRef: "registry.test/captain:1", SourceDigest: "sha256:root", AdoptSecretsAllowed: true},
		Hooks: WorkflowHooks{
			ResolveImages: func(_ context.Context, node NodeContext) (map[string]string, error) {
				operations = append(operations, "images:"+node.Path.String())
				contexts[node.Path.String()] = node
				return map[string]string{"example/image:tag": "example/image@sha256:resolved"}, nil
			},
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				operations = append(operations, "capacity:"+node.Path.String())
				return workflowCapacityQuestion(node), nil, nil
			},
			PlanChannels: func(_ context.Context, node NodeContext) (PlannedChannels, error) {
				operations = append(operations, "plan-channels:"+node.Path.String())
				return NewPlannedChannels(
					[]channelplan.ChannelPlan{{Required: oap.RequiredChannel{Name: "fixture-channel", Kind: "fake"}}},
					func() []string { return []string{wizardSecret} },
					func(context.Context) error {
						operations = append(operations, "resolve-private-channels:"+node.Path.String())
						return nil
					},
					nil,
					func(context.Context) error {
						operations = append(operations, "execute-private-channels:"+node.Path.String())
						return nil
					},
					func() { operations = append(operations, "finalize-private-channels:"+node.Path.String()) },
				), nil
			},
			ResolveChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.Resolve(ctx)
			},
			AdoptDecision: func(_ context.Context, node NodeContext, conflicts []Conflict) ([]string, error) {
				operations = append(operations, "adopt:"+node.Path.String())
				keys := make([]string, 0, len(conflicts))
				for _, conflict := range conflicts {
					keys = append(keys, conflict.Key())
				}
				return keys, nil
			},
			ApplyChannels: func(_ context.Context, node NodeContext, planned PlannedChannels) error {
				operations = append(operations, "apply-channels:"+node.Path.String())
				return planned.Apply(ctx)
			},
		},
	}

	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)
	assert.Empty(t, c.Mutations(), "planning every node and decision must be read-only")
	assert.Equal(t, []string{
		"images:", "plan-channels:", "capacity:", "adopt:",
		"images:reviewer", "plan-channels:reviewer", "capacity:reviewer", "adopt:reviewer",
		"images:reviewer > helper", "plan-channels:reviewer > helper", "capacity:reviewer > helper", "adopt:reviewer > helper",
	}, operations)
	assert.Equal(t, "captain-reviewer", contexts["reviewer"].PhysicalName)
	assert.Equal(t, "agents", contexts["reviewer"].Namespace)
	assert.Same(t, root.Dependencies[0].Bundle, contexts["reviewer"].Bundle)

	rendered, err := json.Marshal(plan)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), "root-secret-value")
	assert.NotContains(t, string(rendered), "child-secret-value")
	assert.NotContains(t, string(rendered), wizardSecret)
	assert.NotContains(t, fmt.Sprintf("%#v", plan), wizardSecret)
	preparedByPath := make(map[string]*Prepared, len(plan.Nodes))
	for _, node := range plan.Nodes {
		preparedByPath[node.Input.Path.String()] = node.Prepared
		capacity, found, err := unstructured.NestedString(node.Prepared.CRs[0].Object, "spec", "capacity")
		require.NoError(t, err)
		assert.True(t, found)
		assert.NotEmpty(t, capacity)
	}
	assert.Equal(t, "registry", preparedByPath[""].Options.SourceKind)
	assert.Equal(t, "registry.test/captain:1", preparedByPath[""].Options.SourceRef)
	assert.Equal(t, "embedded", preparedByPath["reviewer"].Options.SourceKind)
	assert.Equal(t, "agents/reviewer", preparedByPath["reviewer"].Options.SourceRef)
	assert.NotEmpty(t, preparedByPath["reviewer"].Options.SourceDigest)

	result, err := w.Execute(ctx, plan)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"resolve-private-channels:reviewer > helper",
		"resolve-private-channels:reviewer",
		"resolve-private-channels:",
		"apply-channels:reviewer > helper", "execute-private-channels:reviewer > helper",
		"apply-channels:reviewer", "execute-private-channels:reviewer",
		"apply-channels:", "execute-private-channels:",
		"finalize-private-channels:reviewer > helper",
		"finalize-private-channels:reviewer",
		"finalize-private-channels:",
	}, operations[len(operations)-12:])
	require.Len(t, result.Nodes, 3)
	assert.Equal(t, oap.DependencyPath{"reviewer", "helper"}, result.Nodes[0].Path)
	assert.Equal(t, 1, result.Nodes[1].Result.SecretsCreated)
	assert.Equal(t, 1, result.Nodes[2].Result.SecretsCreated)
	assert.NotEmpty(t, c.Mutations())
	var childSecret corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "captain-reviewer-review-token"}, &childSecret))
	assert.Error(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "review-token"}, &corev1.Secret{}),
		"dependency Secrets must use their graph-planned private physical names")
}

func TestWorkflowQualifiesPlanningErrorsAndMutatesNothing(t *testing.T) {
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				if node.Path.String() == "reviewer > helper" {
					return nil, nil, errors.New("capacity unavailable")
				}
				return workflowCapacityQuestion(node), nil, nil
			},
		},
	}
	_, err := w.Plan(context.Background(), root, workflowAnswers())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "captain > reviewer > helper")
	assert.ErrorContains(t, err, "capacity unavailable")
	assert.Empty(t, c.Mutations())
}

func TestWorkflowAggregateDecisionsReturnsEveryMissingNodeWithoutAPlan(t *testing.T) {
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	var inspected []string
	w := &Workflow{
		Client: c, Options: InstallOpts{Namespace: "agents"}, AggregateDecisions: true,
		Hooks: WorkflowHooks{InspectNode: func(_ context.Context, node NodeContext) error {
			inspected = append(inspected, node.Path.String())
			return nil
		}},
	}

	plan, err := w.Plan(context.Background(), root, GraphAnswers{})
	require.Nil(t, plan, "a decision-bearing plan must never be executable")
	var decisions *DecisionError
	require.ErrorAs(t, err, &decisions)
	require.Len(t, decisions.Missing, 3)
	assert.Equal(t, []string{"", "reviewer", "reviewer > helper"}, []string{
		decisions.Missing[0].Path.String(), decisions.Missing[1].Path.String(), decisions.Missing[2].Path.String(),
	})
	assert.Equal(t, []string{"", "reviewer", "reviewer > helper"}, inspected,
		"the Workflow-owned traversal must inspect every node even when an earlier node needs input")
	assert.Empty(t, c.Mutations())
}

func TestWorkflowAggregateDecisionsReturnsEveryGraphConflictWithoutAPlan(t *testing.T) {
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(workflowForeignAgentClasses(t, root)...).Build()}
	w := &Workflow{
		Client: c, Options: InstallOpts{Namespace: "agents"}, AggregateDecisions: true,
		Hooks: WorkflowHooks{CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			return workflowCapacityQuestion(node), nil, nil
		}},
	}

	plan, err := w.Plan(context.Background(), root, workflowAnswers())
	require.Nil(t, plan, "a conflict-bearing plan must never be executable")
	var decisions *DecisionError
	require.ErrorAs(t, err, &decisions)
	require.Len(t, decisions.Conflicts, 3)
	assert.Equal(t, []string{"", "reviewer", "reviewer > helper"}, []string{
		decisions.Conflicts[0].Path.String(), decisions.Conflicts[1].Path.String(), decisions.Conflicts[2].Path.String(),
	})
	assert.Empty(t, c.Mutations())
}

func TestWorkflowAggregateDecisionsDiscoversSameNodeQuestionsCapacityAndConflict(t *testing.T) {
	root := knownGraph(t)
	foreign := workflowForeignAgentClasses(t, root)
	var rootForeign client.Object
	for _, object := range foreign {
		if object.GetName() == "captain" {
			rootForeign = object
			break
		}
	}
	require.NotNil(t, rootForeign)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rootForeign).Build()}
	w := &Workflow{
		Client: c, Options: InstallOpts{Namespace: "agents"}, AggregateDecisions: true,
		Hooks: WorkflowHooks{CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			if len(node.Path) > 0 {
				return nil, nil, nil
			}
			return workflowCapacityQuestion(node), nil, nil
		}},
	}

	plan, err := w.Plan(context.Background(), root, GraphAnswers{})
	require.Nil(t, plan, "partial decision discovery must never produce an executable plan")
	var decisions *DecisionError
	require.ErrorAs(t, err, &decisions)
	require.NotEmpty(t, decisions.Missing)
	rootMissing := decisions.Missing[0]
	assert.Empty(t, rootMissing.Path)
	var missingNames []string
	for _, question := range rootMissing.Questions {
		missingNames = append(missingNames, question.Name)
	}
	assert.Contains(t, missingNames, "repos", "the authored question remains visible")
	assert.Contains(t, missingNames, "requires.secrets.root-token.api-key", "the required Secret remains visible")
	assert.Contains(t, missingNames, "capacity.cpu", "capacity must still be derived from the partial/default bundle")
	require.NotEmpty(t, decisions.Conflicts)
	assert.Empty(t, decisions.Conflicts[0].Path)
	assert.Equal(t, "AgentClass/captain", decisions.Conflicts[0].Conflicts[0].Key())
	assert.Empty(t, c.Mutations())
}

func TestWorkflowAggregateDecisionsStillStopsAtFatalInspectionError(t *testing.T) {
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	var inspected []string
	w := &Workflow{
		Client: c, Options: InstallOpts{Namespace: "agents"}, AggregateDecisions: true,
		Hooks: WorkflowHooks{
			InspectNode: func(_ context.Context, node NodeContext) error {
				inspected = append(inspected, node.Path.String())
				if node.Path.String() == "reviewer" {
					return errors.New("security inspection failed")
				}
				return nil
			},
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
		},
	}

	plan, err := w.Plan(context.Background(), root, workflowAnswers())
	require.Nil(t, plan)
	require.ErrorContains(t, err, "security inspection failed")
	assert.Equal(t, []string{"", "reviewer"}, inspected)
	assert.Empty(t, c.Mutations())
}

func TestWorkflowRejectsUnknownGraphValuesBeforeHooks(t *testing.T) {
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	called := false
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{ResolveImages: func(context.Context, NodeContext) (map[string]string, error) {
			called = true
			return nil, nil
		}},
	}
	secret := "value-that-must-not-be-rendered"
	_, err := w.Plan(context.Background(), root, GraphAnswers{Values: GraphValues{
		"agents": map[string]any{"unknown-child": map[string]any{"token": secret}},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown-child")
	assert.NotContains(t, err.Error(), secret)
	assert.False(t, called)
	assert.Empty(t, c.Mutations())
}

func TestWorkflowValidatesReservedValuesWithAndWithoutCapacityHook(t *testing.T) {
	newAnswers := func() GraphAnswers {
		answers := workflowAnswers()
		delete(answers.Values, "capacity.cpu")
		child := answers.Values["agents"].(map[string]any)["reviewer"].(map[string]any)
		delete(child, "capacity.cpu")
		grandchild := child["agents"].(map[string]any)["helper"].(map[string]any)
		delete(grandchild, "capacity.cpu")
		answers.Values["capacity.never-generated"] = "private-capacity-value"
		return answers
	}

	tests := []struct {
		name  string
		hooks WorkflowHooks
	}{
		{name: "missing capacity hook"},
		{name: "generated questions do not include the key", hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
			w := &Workflow{Client: c, Options: InstallOpts{Namespace: "agents"}, Hooks: tc.hooks}
			_, err := w.Plan(context.Background(), knownGraph(t), newAnswers())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "capacity.never-generated")
			assert.NotContains(t, err.Error(), "private-capacity-value")
			assert.Empty(t, c.Mutations())
		})
	}
}

func TestWorkflowPreflightsEveryNodeBeforePlanningHooks(t *testing.T) {
	root := knownGraph(t)
	root.Dependencies[0].Bundle.Dependencies[0].Bundle.Manifest.Compat.MinApVersion = "99.0.0"
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	called := false
	w := &Workflow{
		Client:           c,
		ClusterAPVersion: "1.0.0",
		Options:          InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{ResolveImages: func(context.Context, NodeContext) (map[string]string, error) {
			called = true
			return nil, nil
		}},
	}
	_, err := w.Plan(context.Background(), root, workflowAnswers())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "captain > reviewer > helper")
	assert.Contains(t, err.Error(), "requires oap >= 99.0.0")
	assert.False(t, called, "all node compatibility checks must finish before collecting decisions")
	assert.Empty(t, c.Mutations())
}

func TestWorkflowRejectsIncompleteExecutionStateBeforeMutation(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			return workflowCapacityQuestion(node), nil, nil
		}},
	}
	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)
	delete(plan.workflow.nodes, "")

	_, err = w.Execute(ctx, plan)
	require.ErrorContains(t, err, "missing planned state")
	assert.Empty(t, c.Mutations(), "an incomplete plan must fail before the first apply")
}

func TestWorkflowResolveFailureRollsBackResolutionAndMutatesNoBundleResources(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	var operations []string
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
			PlanChannels: func(_ context.Context, node NodeContext) (PlannedChannels, error) {
				path := node.Path.String()
				return NewPlannedChannels(nil, nil,
					func(context.Context) error {
						operations = append(operations, "resolve:"+path)
						if path == "" {
							return errors.New("later root resolution failed")
						}
						return nil
					},
					func(context.Context) error {
						operations = append(operations, "rollback-resolve:"+path)
						return nil
					},
					func(context.Context) error {
						operations = append(operations, "apply:"+path)
						return nil
					},
				), nil
			},
			ResolveChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.Resolve(ctx)
			},
			ApplyChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.Apply(ctx)
			},
		},
	}
	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)

	_, err = w.Execute(ctx, plan)
	require.ErrorContains(t, err, "later root resolution failed")
	assert.Equal(t, []string{
		"resolve:reviewer > helper", "resolve:reviewer", "resolve:",
		"rollback-resolve:", "rollback-resolve:reviewer", "rollback-resolve:reviewer > helper",
	}, operations)
	assert.Empty(t, c.Mutations(), "all graph channel resolution must finish before the first bundle-owned mutation")
}

func TestWorkflowChannelFailureRollsBackEveryCreatedNode(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
			ApplyChannels: func(_ context.Context, node NodeContext, _ PlannedChannels) error {
				if len(node.Path) == 0 {
					return errors.New("root channel setup failed")
				}
				return nil
			},
		},
	}
	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)
	_, err = w.Execute(ctx, plan)
	require.ErrorContains(t, err, "root channel setup failed")
	for _, name := range []string{"captain", "captain-reviewer", "captain-reviewer-helper"} {
		var class v1alpha1.AgentClass
		assert.Error(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: name}, &class), name)
	}
}

func TestWorkflowRollsBackAppliedChannelsBeforeEachBundleAndPrerequisitesLast(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	var operations []string
	c := &workflowRollbackOrderClient{
		Client:     fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		operations: &operations,
		paths: map[string]string{
			"captain": "", "captain-reviewer": "reviewer",
			"captain-reviewer-helper": "reviewer > helper",
		},
	}
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
			PlanChannels: func(_ context.Context, node NodeContext) (PlannedChannels, error) {
				path := node.Path.String()
				return NewPlannedChannelsWithRollback(nil, nil,
					func(context.Context) error { return nil },
					func(context.Context) error {
						operations = append(operations, "prerequisite:"+path)
						return nil
					},
					func(context.Context) error {
						if path == "" {
							return errors.New("root channel apply failed")
						}
						return nil
					},
					func(context.Context) error {
						operations = append(operations, "channel:"+path)
						return nil
					}, nil), nil
			},
			ResolveChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error { return planned.Resolve(ctx) },
			ApplyChannels:   func(ctx context.Context, _ NodeContext, planned PlannedChannels) error { return planned.Apply(ctx) },
			RollbackAppliedChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.RollbackApplied(ctx)
			},
		},
	}
	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)
	_, err = w.Execute(ctx, plan)
	require.ErrorContains(t, err, "root channel apply failed")
	assert.Equal(t, []string{
		"channel:", "bundle:",
		"channel:reviewer", "bundle:reviewer",
		"channel:reviewer > helper", "bundle:reviewer > helper",
		"prerequisite:", "prerequisite:reviewer", "prerequisite:reviewer > helper",
	}, operations)
}

func TestWorkflowApplyFailureAggregatesAndRedactsChannelPrerequisiteRollbackErrors(t *testing.T) {
	ctx := context.Background()
	root := knownGraph(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	secret := "unsafe-prerequisite-rollback-credential"
	var rollbacks []string
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
			PlanChannels: func(_ context.Context, node NodeContext) (PlannedChannels, error) {
				path := node.Path.String()
				return NewPlannedChannels(nil, func() []string { return []string{secret} },
					func(context.Context) error { return nil },
					func(context.Context) error {
						rollbacks = append(rollbacks, path)
						if path == "" {
							return &workflowCredentialError{secret: secret}
						}
						return nil
					}, nil,
				), nil
			},
			ResolveChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.Resolve(ctx)
			},
			ApplyChannels: func(_ context.Context, node NodeContext, _ PlannedChannels) error {
				if len(node.Path) == 0 {
					return errors.New("root ApplyChannels failed")
				}
				return nil
			},
		},
	}
	plan, err := w.Plan(ctx, root, workflowAnswers())
	require.NoError(t, err)

	_, err = w.Execute(ctx, plan)
	require.ErrorContains(t, err, "root ApplyChannels failed")
	assert.ErrorContains(t, err, "rollback channel prerequisites")
	assert.Equal(t, []string{"", "reviewer", "reviewer > helper"}, rollbacks,
		"every resolved receipt is cleaned exactly once in reverse resolution order")
	assert.NotContains(t, err.Error(), secret)
	var raw *workflowCredentialError
	assert.False(t, errors.As(err, &raw), "an unsafe cleanup cause must be unreachable after redaction")
}

func TestWorkflowChannelFailureRedactsWizardAnswers(t *testing.T) {
	root := knownGraph(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	secret := "wizard-token-that-must-not-reach-errors"
	var sensitive []string
	w := &Workflow{
		Client:  c,
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{
			CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				return workflowCapacityQuestion(node), nil, nil
			},
			PlanChannels: func(context.Context, NodeContext) (PlannedChannels, error) {
				return NewPlannedChannels(nil, func() []string { return sensitive }, func(context.Context) error {
					sensitive = append(sensitive, secret)
					return &workflowCredentialError{secret: secret}
				}, nil, nil), nil
			},
			ResolveChannels: func(ctx context.Context, _ NodeContext, planned PlannedChannels) error {
				return planned.Resolve(ctx)
			},
		},
	}
	plan, err := w.Plan(context.Background(), root, workflowAnswers())
	require.NoError(t, err)
	_, err = w.Execute(context.Background(), plan)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	var raw *workflowCredentialError
	assert.False(t, errors.As(err, &raw), "redaction must make the raw credential-bearing cause unreachable")
}

func TestPlannedChannelsSerializationOmitsWizardAnswers(t *testing.T) {
	secret := "wizard-answer-that-must-not-be-serialized"
	planned := NewPlannedChannels(
		[]channelplan.ChannelPlan{{Required: oap.RequiredChannel{Name: "fixture-channel", Kind: "fake"}}},
		func() []string { return []string{secret} }, nil, nil, func(context.Context) error { return nil },
	)
	rendered, err := json.Marshal(planned)
	require.NoError(t, err)
	assert.Contains(t, string(rendered), "fixture-channel")
	assert.NotContains(t, string(rendered), secret)
	assert.NotContains(t, fmt.Sprintf("%#v", planned), secret)
}

func TestPlannedChannelsFinalizesPrivateReceiptOnlyOnce(t *testing.T) {
	finalized := 0
	planned := NewPlannedChannels(nil, nil, nil, nil, nil, func() { finalized++ })
	planned.finalizeResolve()
	planned.finalizeResolve()
	assert.Equal(t, 1, finalized)
}

func TestWorkflowPlanSerializationOmitsSecretQuestionDefaultsAndPreparedState(t *testing.T) {
	root := knownGraph(t)
	secretDefault := "bundle-authored-secret-default-that-must-stay-private"
	root.Manifest.Questions = append(root.Manifest.Questions, oap.Question{
		Name: "private-default", Type: oap.QSecret, Prompt: "Private default",
		Default: secretDefault,
		Secret:  &oap.SecretQuestion{CreateSecret: &oap.SecretTarget{Name: "default-token", Key: "token"}},
	})
	w := &Workflow{
		Client:  fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Options: InstallOpts{Namespace: "agents"},
		Hooks: WorkflowHooks{CapacityQuestions: func(_ context.Context, node NodeContext, _ []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			return workflowCapacityQuestion(node), nil, nil
		}},
	}
	plan, err := w.Plan(context.Background(), root, workflowAnswers())
	require.NoError(t, err)

	renderedPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	assert.NotContains(t, string(renderedPlan), secretDefault)
	assert.NotContains(t, string(renderedPlan), "Bundle")
	assert.NotContains(t, string(renderedPlan), "Prepared")

	var rootNode *PreparedNode
	for _, node := range plan.Nodes {
		if len(node.Input.Path) == 0 {
			rootNode = node
			break
		}
	}
	require.NotNil(t, rootNode)
	renderedNode, err := json.Marshal(rootNode)
	require.NoError(t, err)
	assert.NotContains(t, string(renderedNode), secretDefault)
	assert.NotContains(t, string(renderedNode), "Bundle")
	assert.NotContains(t, string(renderedNode), "Prepared")
	renderedDiagnostic := fmt.Sprintf("%#v", rootNode)
	assert.NotContains(t, renderedDiagnostic, secretDefault)
	assert.NotContains(t, renderedDiagnostic, "Bundle")
}
