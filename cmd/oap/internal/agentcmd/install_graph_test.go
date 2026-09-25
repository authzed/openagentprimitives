package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

const (
	graphFixtureRoot  = "test-coordinator"
	graphFixtureChild = "reviewer"
)

type graphInstallFixtureOptions struct {
	rootManifest  string
	childManifest string
	childObjects  string
}

func graphInstallFixture(t *testing.T, opts graphInstallFixtureOptions) string {
	t.Helper()
	root := t.TempDir()
	child := filepath.Join(root, "dependencies", graphFixtureChild)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "manifests"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(child, "manifests"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
requires:
  agents:
    - path: dependencies/reviewer
`+opts.rootManifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: test-coordinator
spec:
  description: Root graph fixture.
  subagents:
    - reviewer
`), 0o644))
	childManifest := `
oapFormatVersion: "1"
agent:
  version: "1.0.0"
` + opts.childManifest
	require.NoError(t, os.WriteFile(filepath.Join(child, "oap.yaml"), []byte(childManifest), 0o644))
	childObjects := `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: reviewer
spec:
  description: Child graph fixture.
` + opts.childObjects
	require.NoError(t, os.WriteFile(filepath.Join(child, "manifests", "agent.yaml"), []byte(childObjects), 0o644))
	return root
}

func graphQuestionManifest() string {
	return `questions:
  - name: voice
    type: string
    prompt: Voice
    required: true
    binding:
      - target: AgentClass/reviewer#spec.description
`
}

func TestAgentInstallMetadataNameAnswerUsesLegacyRootNaming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{name: "answered name", want: "custom"},
		{name: "explicit prefix", extra: []string{"--name", "prefix"}, want: "prefix-custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forceNonInteractiveStdin(t)
			kb := fakeBundle(t)
			dir := graphInstallFixture(t, graphInstallFixtureOptions{rootManifest: `questions:
  - name: root-name
    type: string
    prompt: Root name
    binding:
      - target: AgentClass/test-coordinator#metadata.name
`})
			args := append([]string{"--set", "root-name=custom"}, tc.extra...)
			out, err := runAgentInstall(t, graphGlobals(kb), dir, args...)
			require.NoErrorf(t, err, "out=%s", out)
			graphAgentClass(t, kb, tc.want)
			child := tc.want + "-reviewer"
			if tc.extra != nil {
				child = "prefix-reviewer"
			}
			graphAgentClass(t, kb, child)
		})
	}
}

func graphGlobals(kb *kube.Bundle) *apcmd.Globals {
	return &apcmd.Globals{
		Context:  "kind-graph-fixture",
		BundleFn: func() (*kube.Bundle, error) { return kb, nil },
	}
}

func graphAgentClass(t *testing.T, kb *kube.Bundle, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("AgentClass")
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: name}, got))
	return got
}

func TestAgentInstallNestedSetInstallsChild(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: graphQuestionManifest()})

	out, err := runAgentInstall(t, graphGlobals(kb), dir, "--set", "agents.reviewer.voice=theatrical")

	require.NoErrorf(t, err, "nested --set must install the complete graph; out=%s", out)
	assert.Contains(t, out, "Installed test-coordinator")
	assert.Contains(t, out, "└─ reviewer → test-coordinator-reviewer")
	child := graphAgentClass(t, kb, "test-coordinator-reviewer")
	assert.Equal(t, "theatrical", child.Object["spec"].(map[string]any)["description"])
	root := graphAgentClass(t, kb, graphFixtureRoot)
	roster, found, rosterErr := unstructured.NestedStringSlice(root.Object, "spec", "subagents")
	require.NoError(t, rosterErr)
	require.True(t, found)
	assert.Equal(t, []string{"test-coordinator-reviewer"}, roster)
}

func TestAgentInstallNestedValuesInstallsChild(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: graphQuestionManifest()})
	values := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(values, []byte(`agents:
  reviewer:
    voice: sepulchral
`), 0o644))

	out, err := runAgentInstall(t, graphGlobals(kb), dir, "--values", values)

	require.NoErrorf(t, err, "nested --values must install the complete graph; out=%s", out)
	child := graphAgentClass(t, kb, "test-coordinator-reviewer")
	assert.Equal(t, "sepulchral", child.Object["spec"].(map[string]any)["description"])
}

func TestAgentInstallValuesKeepsAuthoredScalarAgentsQuestionAtRoot(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{rootManifest: `questions:
  - name: agents
    type: string
    prompt: Agent mode
    required: true
    binding:
      - target: AgentClass/test-coordinator#spec.description
`})
	values := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(values, []byte("agents: coordinated\n"), 0o600))

	out, err := runAgentInstall(t, graphGlobals(kb), dir, "--values", values)

	require.NoErrorf(t, err, "a non-map agents value belongs to the authored root question; out=%s", out)
	root := graphAgentClass(t, kb, graphFixtureRoot)
	assert.Equal(t, "coordinated", root.Object["spec"].(map[string]any)["description"])
}

func TestAgentInstallNestedMissingAnswerNamesLogicalPath(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: graphQuestionManifest()})

	_, err := runAgentInstall(t, graphGlobals(kb), dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "test-coordinator > reviewer")
	assert.Contains(t, err.Error(), "voice")
	assert.Contains(t, err.Error(), "--set agents.reviewer.voice=<value>")
	assert.Error(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: graphFixtureRoot}, &unstructured.Unstructured{}),
		"a missing child answer must abort before the root is applied")
}

func TestAgentInstallNestedSecretUsesPhysicalNameAndNeverPrintsValue(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `questions:
  - name: token
    type: secret
    prompt: Token
    required: true
    secret:
      createSecret:
        name: review-token
        key: token
`})
	const secretValue = "child-secret-that-must-not-be-rendered"

	out, err := runAgentInstall(t, graphGlobals(kb), dir, "--set", "agents.reviewer.token="+secretValue)

	require.NoErrorf(t, err, "child secret must install; out=%s", out)
	var secret corev1.Secret
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS,
		Name:      "test-coordinator-reviewer-review-token",
	}, &secret))
	assert.Equal(t, secretValue, secret.StringData["token"],
		"the controller-runtime fake has no apiserver admission to convert stringData into data")
	assert.NotContains(t, out, secretValue)
	assert.NotContains(t, fmt.Sprint(err), secretValue)
}

func TestAgentInstallNestedChildCapacityFits(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childObjects: `
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: fixture-config
spec:
  resources:
    memory: "8Gi"
`})

	out, err := runAgentInstall(t, graphGlobals(kb), dir, "--fit-resources")

	require.NoErrorf(t, err, "child capacity must use the normal fitting hook; out=%s", out)
	got := &unstructured.Unstructured{}
	got.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
	got.SetKind("SpiceboxClass")
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Name: "test-coordinator-reviewer-fixture-config"}, got))
	assert.Equal(t, "2Gi", memoryField(t, got))
	assert.Contains(t, out, "reviewer")
}

func TestAgentInstallNestedChildChannelIsPlannedAndReported(t *testing.T) {
	forceNonInteractiveStdin(t)
	kb := fakeBundle(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: local
      role: both
      name: reviewer-local
      purpose: Child graph fixture channel.
`})

	out, err := runAgentInstall(t, graphGlobals(kb), dir)

	require.NoErrorf(t, err, "an unattended child channel is reported as skipped, not ignored; out=%s", out)
	assert.Contains(t, out, "reviewer-local")
	assert.Contains(t, out, "reviewer")
	assert.Contains(t, out, "channels:")
}

func TestAgentInstallGraphConfiguresChildChannel(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: fake
      role: both
      name: review-fake
      purpose: Child graph fixture channel.
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t)
	kb.Controller = &graphAPIIdentityClient{Client: kb.Controller}
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := refusingDriver{t: t}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		interactive: true, questionDriver: driver, questionTheme: theme,
		questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
		in:           strings.NewReader(""), out: &out,
	})
	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{
		Interactive: true,
		QuestionOptions: []install.ResolveOption{
			install.PresentOver(driver, theme),
		},
	})
	require.NoErrorf(t, err, "plan child channel; out=%s", out.String())

	result, err := w.Execute(context.Background(), plan)

	require.NoErrorf(t, err, "configure child channel; out=%s", out.String())
	channelName := "test-coordinator-reviewer-review-fake"
	var channel spiceboxv1alpha1.Channel
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS, Name: channelName,
	}, &channel), "the graph channel must use the ownership-safe tracked client")
	assert.Equal(t, graphFixtureRoot, channel.Labels[instance.LabelInstall])
	assert.Equal(t, capacityFixtureNS, channel.Labels[instance.LabelInstallNamespace])
	assert.Equal(t, graphFixtureChild, channel.Annotations[install.AnnotationDependencyPath])
	var credential corev1.Secret
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS, Name: channelName + "-creds",
	}, &credential))
	assert.Equal(t, graphFixtureRoot, credential.Labels[instance.LabelInstall])
	assert.Equal(t, capacityFixtureNS, credential.Labels[instance.LabelInstallNamespace])
	assert.Equal(t, graphFixtureChild, credential.Annotations[install.AnnotationDependencyPath])
	var childChannels int
	for _, node := range result.Nodes {
		if node.Path.String() == graphFixtureChild {
			childChannels = len(node.Channels)
		}
	}
	assert.Equal(t, 1, childChannels)

	deleted, err := install.UninstallGraph(context.Background(), kb.Controller, graphFixtureRoot, capacityFixtureNS)
	require.NoError(t, err)
	assert.Equal(t, 4, deleted, "root, child, child Channel, and child credential Secret belong to one graph lifecycle")
	assert.Error(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS, Name: channelName,
	}, &spiceboxv1alpha1.Channel{}))
	assert.Error(t, kb.Controller.Get(context.Background(), client.ObjectKey{
		Namespace: capacityFixtureNS, Name: channelName + "-creds",
	}, &corev1.Secret{}))
}

func TestAgentInstallGraphCredentialSecretOwnershipPreflight(t *testing.T) {
	const secretName = "test-coordinator-reviewer-review-fake-creds"
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: fake
      role: both
      name: review-fake
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		labels    map[string]string
		wantError bool
	}{
		{name: "another graph", labels: map[string]string{instance.LabelInstall: "other-root", instance.LabelInstallNamespace: capacityFixtureNS}, wantError: true},
		{name: "project managed without graph owner", labels: nil, wantError: true},
		{name: "same graph reinstall", labels: map[string]string{instance.LabelInstall: graphFixtureRoot, instance.LabelInstallNamespace: capacityFixtureNS}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: secretName, Namespace: capacityFixtureNS, UID: types.UID("existing-secret-uid"), ResourceVersion: "7",
				Labels: tc.labels, Annotations: map[string]string{wizardrun.InstalledByAnnotation: kube.InstalledByValue},
			}, Data: map[string][]byte{"token": []byte("preserve-me")}}
			kb := fakeBundle(t, secret)
			kb.Controller = &graphAPIIdentityClient{Client: kb.Controller}
			theme := tui.NewTheme(tui.Caps{})
			driver := refusingDriver{t: t}
			w := newCLIWorkflow(cliWorkflowConfig{
				ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
				root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
				interactive: true, questionDriver: driver, questionTheme: theme,
				questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
				in:           strings.NewReader(""), out: io.Discard,
			})

			plan, planErr := w.Plan(context.Background(), root, install.GraphAnswers{Interactive: true})
			if tc.wantError {
				require.Error(t, planErr)
				assert.Contains(t, planErr.Error(), secretName)
				assert.Contains(t, planErr.Error(), "cannot be adopted")
				assert.Nil(t, plan)
				var got corev1.Secret
				require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: secretName}, &got))
				assert.Equal(t, []byte("preserve-me"), got.Data["token"])
				assert.Equal(t, tc.labels, got.Labels)
				deleted, uninstallErr := install.UninstallGraph(context.Background(), kb.Controller, graphFixtureRoot, capacityFixtureNS)
				require.NoError(t, uninstallErr)
				assert.Zero(t, deleted)
				require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: secretName}, &corev1.Secret{}))
				return
			}

			require.NoError(t, planErr)
			_, execErr := w.Execute(context.Background(), plan)
			require.NoError(t, execErr)
			var got corev1.Secret
			require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: secretName}, &got))
			assert.Equal(t, graphFixtureRoot, got.Labels[instance.LabelInstall])
			assert.Equal(t, []byte("ok"), got.Data["placeholder"])
		})
	}
}

func TestAgentInstallGraphKnownChannelConflictRefusesDuringReadOnlyPlanning(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: fake
      role: both
      name: review-fake
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	channelName := "test-coordinator-reviewer-review-fake"
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: channelName, Namespace: capacityFixtureNS},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "fake", Role: spiceboxv1alpha1.ChannelRoleBoth, AgentClass: "fixture-foreign-owner",
		},
	}
	kb := fakeBundle(t, existing)
	recording := &graphMutationClient{Client: kb.Controller}
	kb.Controller = recording
	theme := tui.NewTheme(tui.Caps{})
	driver := refusingDriver{t: t}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		interactive: true, questionDriver: driver, questionTheme: theme,
		questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
		in:           strings.NewReader(""), out: io.Discard,
	})

	plan, planErr := w.Plan(context.Background(), root, install.GraphAnswers{Interactive: true})

	require.Error(t, planErr, "a known nonrecoverable channel verdict must not yield an executable graph plan")
	assert.Nil(t, plan)
	assert.Contains(t, planErr.Error(), channelName)
	assert.Zero(t, recording.mutations, "read-only planning may not apply any graph resource")
	var got spiceboxv1alpha1.Channel
	require.NoError(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: channelName}, &got))
	assert.Equal(t, "fixture-foreign-owner", got.Spec.AgentClass, "the conflicting Channel must remain unchanged")
}

func TestAgentInstallGraphLaterNodeFailureRollsBackChildChannel(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: fake
      role: both
      name: review-fake
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t)
	recording := &graphMutationClient{Client: kb.Controller, failCreateName: graphFixtureRoot, failCreateErr: errors.New("injected root failure")}
	kb.Controller = recording
	theme := tui.NewTheme(tui.Caps{})
	driver := refusingDriver{t: t}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		interactive: true, questionDriver: driver, questionTheme: theme,
		questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
		in:           strings.NewReader(""), out: io.Discard,
	})
	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{Interactive: true})
	require.NoError(t, err)

	_, err = w.Execute(context.Background(), plan)
	require.ErrorContains(t, err, "injected root failure")
	channelName := "test-coordinator-reviewer-review-fake"
	_, dynamicErr := kb.Dynamic.Resource(channelGVR).Namespace(capacityFixtureNS).
		Get(context.Background(), channelName, metav1.GetOptions{})
	assert.Error(t, dynamicErr, "a graph failure must not leave the legacy dynamic-client Channel behind")
	_, dynamicErr = kb.Dynamic.Resource(secretGVR).Namespace(capacityFixtureNS).
		Get(context.Background(), channelName+"-creds", metav1.GetOptions{})
	assert.Error(t, dynamicErr, "a graph failure must not leave the legacy dynamic-client credential behind")
	assert.Error(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: channelName}, &spiceboxv1alpha1.Channel{}))
	assert.Error(t, kb.Controller.Get(context.Background(), client.ObjectKey{Namespace: capacityFixtureNS, Name: channelName + "-creds"}, &corev1.Secret{}))
}

type graphPromptKind struct{ fakekind.Kind }

func (graphPromptKind) Name() string                { return "graph-prompt" }
func (graphPromptKind) Wizard() channelkinds.Wizard { return graphPromptWizard{} }

type graphPromptWizard struct{}

func (graphPromptWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return []oap.Question{{Name: "choice", Type: oap.QString, Prompt: "Choice"}}, nil
}
func (graphPromptWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}
func (graphPromptWizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}
func (graphPromptWizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	name := answers[wizardkeys.KeyChannelName]
	secretName := name + "-creds"
	return channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: in.Namespace}},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "graph-prompt", AgentClass: answers[wizardkeys.KeyAgentClass],
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			},
		},
		Summary: []channelkinds.SummaryNote{{Label: "Choice", Value: answers["choice"]}},
	}, nil
}

func registerGraphPromptKind(t *testing.T) {
	t.Helper()
	before := registry.All()
	registry.Register(graphPromptKind{})
	t.Cleanup(func() {
		registry.Reset()
		for _, kind := range before {
			registry.Register(kind)
		}
	})
}

type mutationAwareDriver struct {
	inner         tui.Driver
	mutations     *graphMutationClient
	afterMutation bool
}

func (d *mutationAwareDriver) Present(ctx context.Context, screenID string, group *huh.Group) error {
	if d.mutations.mutations > 0 {
		d.afterMutation = true
	}
	return d.inner.Present(ctx, screenID, group)
}

func TestAgentInstallGraphCollectsChannelQuestionsBeforeFirstMutation(t *testing.T) {
	registerGraphPromptKind(t)
	dir := graphInstallFixture(t, graphInstallFixtureOptions{rootManifest: `  channels:
    - kind: graph-prompt
      role: both
      name: fixture-prompt
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t)
	recording := &graphMutationClient{Client: kb.Controller}
	kb.Controller = recording
	theme := tui.NewTheme(tui.Caps{})
	driver := &mutationAwareDriver{
		inner:     tui.Plain(strings.NewReader("chosen-before-apply\n"), io.Discard, theme),
		mutations: recording,
	}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb, root: root,
		sourcePath: dir, source: &installSource{kind: installSourceKindFile}, interactive: true,
		questionDriver: driver, questionTheme: theme,
		questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
		in:           strings.NewReader(""), out: io.Discard,
	})

	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{Interactive: true})
	require.NoError(t, err)
	_, err = w.Execute(context.Background(), plan)
	require.NoError(t, err)
	assert.False(t, driver.afterMutation, "no wizard screen may be presented after the first graph resource is applied")
}

type graphMutationClient struct {
	client.Client
	mutations      int
	records        []string
	failCreateName string
	failCreateErr  error
}

// controller-runtime's fake client does not assign API-server object UIDs.
// Tracked channel rollback intentionally refuses an incomplete identity, so
// graph tests that exercise that production path supply the identity the real
// apiserver would return.
type graphAPIIdentityClient struct{ client.Client }

func (c *graphAPIIdentityClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("fixture-" + graphMutationObject(obj)))
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *graphMutationClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.mutations++
	c.records = append(c.records, "create:"+graphMutationObject(obj))
	if obj.GetName() == c.failCreateName {
		return c.failCreateErr
	}
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("fixture-" + graphMutationObject(obj)))
	}
	return c.Client.Create(ctx, obj, opts...)
}
func (c *graphMutationClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.mutations++
	c.records = append(c.records, "update:"+graphMutationObject(obj))
	return c.Client.Update(ctx, obj, opts...)
}
func (c *graphMutationClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.mutations++
	c.records = append(c.records, "patch:"+graphMutationObject(obj))
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *graphMutationClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.mutations++
	c.records = append(c.records, "delete:"+graphMutationObject(obj))
	return c.Client.Delete(ctx, obj, opts...)
}

func graphMutationObject(obj client.Object) string {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		switch obj.(type) {
		case *corev1.Secret:
			kind = "Secret"
		case *spiceboxv1alpha1.PublicEndpoint:
			kind = "PublicEndpoint"
		case *spiceboxv1alpha1.AgentClass:
			kind = "AgentClass"
		default:
			kind = fmt.Sprintf("%T", obj)
		}
	}
	return kind + "/" + obj.GetNamespace() + "/" + obj.GetName()
}

func TestAgentInstallGraphPlanDoesNotMutateForOnDemandWebhookBeforeFailingChild(t *testing.T) {
	t.Setenv("NGROK_AUTHTOKEN", "planned-token-that-must-stay-private")
	dir := graphInstallFixture(t, graphInstallFixtureOptions{
		rootManifest: `  channels:
    - kind: github
      role: input
      name: fixture-github
    - kind: fake
      role: output
      name: fixture-output
`,
		childManifest: graphQuestionManifest(),
	})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	wiring := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)
	kb := wiring.kube
	recording := &graphMutationClient{Client: kb.Controller}
	kb.Controller = recording
	kb.Typed = k8sfake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: cloud.OperatorDeploymentName, Namespace: cloud.WebdServiceNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "operator", Env: []corev1.EnvVar{{Name: cloud.ClusterKindEnvVar, Value: cloud.KeyDesktop}},
		}}}}},
	})
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb, root: root,
		sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		in: strings.NewReader(""), out: io.Discard,
	})
	originalChannels := w.Hooks.PlanChannels
	w.Hooks.PlanChannels = func(ctx context.Context, node install.NodeContext) (install.PlannedChannels, error) {
		if node.Path.String() == graphFixtureChild {
			return install.PlannedChannels{}, fmt.Errorf("child planning failed")
		}
		return originalChannels(ctx, node)
	}

	_, err = w.Plan(context.Background(), root, install.GraphAnswers{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), graphFixtureChild)
	assert.Zero(t, recording.mutations, "Workflow.Plan may read Kubernetes, but must never Create/Patch/Update/Delete")
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{Name: "webd"}, &spiceboxv1alpha1.PublicEndpoint{}))
}

func TestAgentInstallGraphLaterResolveFailureRollsBackEndpointBeforeBundleMutation(t *testing.T) {
	root, kb, w, recording := graphEndpointWorkflow(t)
	originalResolve := w.Hooks.ResolveChannels
	w.Hooks.ResolveChannels = func(ctx context.Context, node install.NodeContext, planned install.PlannedChannels) error {
		if err := originalResolve(ctx, node, planned); err != nil {
			return err
		}
		if len(node.Path) == 0 {
			return fmt.Errorf("later root channel resolution failed")
		}
		return nil
	}

	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{})
	require.NoError(t, err)
	assert.Zero(t, recording.mutations, "planning remains Kubernetes-read-only")
	_, err = w.Execute(context.Background(), plan)
	require.ErrorContains(t, err, "later root channel resolution failed")

	assertGraphEndpointPrerequisitesGone(t, kb)
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: capacityFixtureNS, Name: graphFixtureRoot,
	}, &spiceboxv1alpha1.AgentClass{}), "no bundle-owned root resource may be applied before graph-wide resolution succeeds")
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: capacityFixtureNS, Name: graphFixtureRoot + "-" + graphFixtureChild,
	}, &spiceboxv1alpha1.AgentClass{}), "no bundle-owned child resource may be applied before graph-wide resolution succeeds")
	assert.Equal(t, []string{
		"create:Secret/agentprimitives-system/ngrok-authtoken",
		"create:PublicEndpoint//webd",
		"delete:PublicEndpoint//webd",
		"delete:Secret/agentprimitives-system/ngrok-authtoken",
	}, recording.records, "resolution may create and recover its narrow prerequisites, but no bundle-owned resource")
}

func TestAgentInstallGraphLaterApplyPreparedFailureRollsBackEndpointPrerequisites(t *testing.T) {
	root, kb, w, recording := graphEndpointWorkflow(t)
	recording.failCreateName = graphFixtureRoot
	recording.failCreateErr = errors.New("injected later ApplyPrepared failure")
	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{})
	require.NoError(t, err)

	_, err = w.Execute(context.Background(), plan)
	require.ErrorContains(t, err, "injected later ApplyPrepared failure")
	assert.Contains(t, recording.records, "create:PublicEndpoint//webd",
		"the regression requires a real resolution prerequisite to clean up")
	assert.Contains(t, recording.records, "create:Secret/agentprimitives-system/ngrok-authtoken")
	assert.Equal(t, 1, graphMutationCount(recording.records, "delete:PublicEndpoint//webd"))
	assert.Equal(t, 1, graphMutationCount(recording.records, "delete:Secret/agentprimitives-system/ngrok-authtoken"))
	assert.Less(t,
		graphMutationIndex(t, recording.records, "delete:AgentClass/"+w.Options.Namespace+"/"+graphFixtureRoot+"-"+graphFixtureChild),
		graphMutationIndex(t, recording.records, "delete:PublicEndpoint//webd"),
		"ExecuteGraph must roll back bundle resources before channel prerequisite cleanup",
	)
	assertGraphEndpointPrerequisitesGone(t, kb)
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: w.Options.Namespace, Name: graphFixtureRoot + "-" + graphFixtureChild,
	}, &spiceboxv1alpha1.AgentClass{}), "ExecuteGraph must roll back the child before channel prerequisite cleanup")
}

func TestAgentInstallGraphLaterApplyChannelsFailureRollsBackEndpointPrerequisites(t *testing.T) {
	root, kb, w, recording := graphEndpointWorkflow(t)
	originalApply := w.Hooks.ApplyChannels
	w.Hooks.ApplyChannels = func(ctx context.Context, node install.NodeContext, planned install.PlannedChannels) error {
		if err := originalApply(ctx, node, planned); err != nil {
			return err
		}
		if len(node.Path) == 0 {
			return errors.New("injected later ApplyChannels failure")
		}
		return nil
	}
	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{})
	require.NoError(t, err)

	_, err = w.Execute(context.Background(), plan)
	require.ErrorContains(t, err, "injected later ApplyChannels failure")
	assert.Contains(t, recording.records, "create:PublicEndpoint//webd",
		"the regression requires a real resolution prerequisite to clean up")
	assert.Contains(t, recording.records, "create:Secret/agentprimitives-system/ngrok-authtoken")
	assert.Equal(t, 1, graphMutationCount(recording.records, "delete:PublicEndpoint//webd"))
	assert.Equal(t, 1, graphMutationCount(recording.records, "delete:Secret/agentprimitives-system/ngrok-authtoken"))
	assert.Less(t,
		graphMutationIndex(t, recording.records, "delete:AgentClass/"+w.Options.Namespace+"/"+graphFixtureRoot),
		graphMutationIndex(t, recording.records, "delete:PublicEndpoint//webd"),
		"ExecuteGraph must roll back bundle resources before channel prerequisite cleanup",
	)
	assertGraphEndpointPrerequisitesGone(t, kb)
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: w.Options.Namespace, Name: graphFixtureRoot,
	}, &spiceboxv1alpha1.AgentClass{}), "the failing node's bundle resources must be rolled back first")
}

func TestAgentInstallGraphSuccessfulApplyRetainsEndpointPrerequisites(t *testing.T) {
	root, kb, w, recording := graphEndpointWorkflow(t)
	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{})
	require.NoError(t, err)

	_, err = w.Execute(context.Background(), plan)
	require.NoError(t, err)
	assert.Zero(t, graphMutationCount(recording.records, "delete:PublicEndpoint//webd"))
	assert.Zero(t, graphMutationCount(recording.records, "delete:Secret/agentprimitives-system/ngrok-authtoken"))
	require.NoError(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Name: publicendpoint.WebdName,
	}, &spiceboxv1alpha1.PublicEndpoint{}))
	require.NoError(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: publicendpoint.NgrokAuthTokenSecret,
	}, &corev1.Secret{}))
}

func graphEndpointWorkflow(t *testing.T) (*oap.Bundle, *kube.Bundle, *install.Workflow, *graphMutationClient) {
	t.Helper()
	t.Setenv("NGROK_AUTHTOKEN", "resolution-token-that-must-stay-private")
	dir := graphInstallFixture(t, graphInstallFixtureOptions{rootManifest: `  channels:
    - kind: github
      role: input
      name: fixture-github
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	wiring := endpointWiring(t, cloud.KeyDesktop, desktopLoopbackURL, tunnelComesUp)
	kb := wiring.kube
	recording := &graphMutationClient{Client: kb.Controller}
	kb.Controller = recording
	kb.Typed = k8sfake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: cloud.OperatorDeploymentName, Namespace: cloud.WebdServiceNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "operator", Env: []corev1.EnvVar{{Name: cloud.ClusterKindEnvVar, Value: cloud.KeyDesktop}},
		}}}}},
	})
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb, root: root,
		sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		in: strings.NewReader(""), out: io.Discard,
	})
	return root, kb, w, recording
}

func assertGraphEndpointPrerequisitesGone(t *testing.T, kb *kube.Bundle) {
	t.Helper()
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{Name: publicendpoint.WebdName}, &spiceboxv1alpha1.PublicEndpoint{}),
		"the endpoint created as a resolution prerequisite must not survive graph failure")
	assert.Error(t, kb.Controller.Get(context.Background(), types.NamespacedName{
		Namespace: cloud.WebdServiceNamespace, Name: publicendpoint.NgrokAuthTokenSecret,
	}, &corev1.Secret{}), "the credential created as a resolution prerequisite must not survive graph failure")
}

func graphMutationCount(records []string, want string) int {
	count := 0
	for _, record := range records {
		if record == want {
			count++
		}
	}
	return count
}

func graphMutationIndex(t *testing.T, records []string, want string) int {
	t.Helper()
	for i, record := range records {
		if record == want {
			return i
		}
	}
	require.Failf(t, "graph mutation not found", "missing %q in %v", want, records)
	return 0
}

func TestAgentInstallGraphGivesSameChildChannelDistinctPrivateNamesAcrossRoots(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  channels:
    - kind: fake
      role: both
      name: review-fake
`, childObjects: `
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: reviewer-identity
spec:
  credentials:
    - name: channel
      type: static
      static:
        secretRef:
          name: review-fake-creds
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t)
	kb.Controller = &graphAPIIdentityClient{Client: kb.Controller}
	theme := tui.NewTheme(tui.Caps{})
	driver := refusingDriver{t: t}
	for _, rootName := range []string{"first", "second"} {
		w := newCLIWorkflow(cliWorkflowConfig{
			ctx: context.Background(), globals: graphGlobals(kb), kube: kb, root: root,
			sourcePath: dir, source: &installSource{kind: installSourceKindFile},
			options: cliInstallOptions{name: rootName}, interactive: true,
			questionDriver: driver, questionTheme: theme,
			questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
			in:           strings.NewReader(""), out: io.Discard,
		})
		plan, planErr := w.Plan(context.Background(), root, install.GraphAnswers{Interactive: true})
		require.NoError(t, planErr)
		_, executeErr := w.Execute(context.Background(), plan)
		require.NoError(t, executeErr)
	}

	for _, rootName := range []string{"first", "second"} {
		channelName := rootName + "-reviewer-review-fake"
		err := kb.Controller.Get(context.Background(), client.ObjectKey{
			Namespace: capacityFixtureNS, Name: channelName,
		}, &spiceboxv1alpha1.Channel{})
		require.NoError(t, err, channelName)
		err = kb.Controller.Get(context.Background(), client.ObjectKey{
			Namespace: capacityFixtureNS, Name: channelName + "-creds",
		}, &corev1.Secret{})
		require.NoError(t, err, channelName+"-creds")
	}

	for _, rootName := range []string{"first", "second"} {
		identity := &unstructured.Unstructured{}
		identity.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
		identity.SetKind("AgentIdentity")
		err := kb.Controller.Get(context.Background(), types.NamespacedName{
			Namespace: capacityFixtureNS, Name: rootName + "-reviewer-reviewer-identity",
		}, identity)
		require.NoError(t, err)
		credentials, found, err := unstructured.NestedSlice(identity.Object, "spec", "credentials")
		require.NoError(t, err)
		require.True(t, found)
		require.Len(t, credentials, 1)
		credential := credentials[0].(map[string]any)
		ref, found, err := unstructured.NestedString(credential, "static", "secretRef", "name")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, rootName+"-reviewer-review-fake-creds", ref)
	}
}

func TestAgentInstallNestedTargetedAdoptionRoutesEachKeyToItsNode(t *testing.T) {
	forceNonInteractiveStdin(t)
	foreign := func(name string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(spiceboxv1alpha1.SchemeGroupVersion.String())
		obj.SetKind("AgentClass")
		obj.SetNamespace(capacityFixtureNS)
		obj.SetName(name)
		return obj
	}
	kb := fakeBundle(t, foreign(graphFixtureRoot), foreign("test-coordinator-reviewer"))
	dir := graphInstallFixture(t, graphInstallFixtureOptions{})

	out, err := runAgentInstall(t, graphGlobals(kb), dir,
		"--adopt=AgentClass/test-coordinator",
		"--adopt=AgentClass/test-coordinator-reviewer")

	require.NoErrorf(t, err, "each explicit key must be judged against its graph node; out=%s", out)
	assert.Contains(t, out, "AgentClass/test-coordinator")
	assert.Contains(t, out, "AgentClass/test-coordinator-reviewer")
}
