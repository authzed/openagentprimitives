package agentcmd

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagebuild"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// The hazard this exists for: tui.Options.Inline is set per run, and
// `oap agent install` makes several. The bundle's manifest questions, the adopt
// decision and the image reconcile's build questions all present over ONE
// driver — so they cannot disagree about the alternate screen, and cannot each
// start a buffer over stdin bytes the last one swallowed.
//
// Which answer this command gives — inline, because it asks from the middle of
// its own output — is stated alongside every other oap surface's in cmd/oap's
// TestWhichCommandsAskInline. This is the separate claim that the answer is
// given once and shared.
func TestAgentInstallAsksEveryQuestionOverOnePresentation(t *testing.T) {
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := tui.DriverFor(InstallQuestionDriverParams(theme, strings.NewReader(""), &out))
	require.NotNil(t, driver, "precondition: the command resolves a driver to share")

	env, err := newInstallEnv(context.Background(), &apcmd.Globals{Context: "kind-mycluster"},
		t.TempDir(), buildSets{}, &out, driver, theme)
	require.NoError(t, err)

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.Same(t, driver, ie.driver,
		"the build questions must go over the same driver the manifest questions and the adopt decision do")
	assert.Same(t, theme, ie.theme,
		"and be styled by the theme that driver was built with")
}

func TestAgentInstallGraphSharesOnePresentationAcrossRootChildAndAdoption(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: graphQuestionManifest()})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	root.Manifest.Questions = append(root.Manifest.Questions, oap.Question{
		Name: "captain-style", Type: oap.QString, Prompt: "Captain style",
		Binding: []oap.Binding{{Target: "AgentClass/test-coordinator#spec.description"}},
	})
	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	foreign.SetKind("AgentClass")
	foreign.SetNamespace(capacityFixtureNS)
	foreign.SetName(graphFixtureRoot)
	kb := fakeBundle(t, foreign)

	// Parent question, adopt the one root conflict, then child question. A
	// second plain driver would buffer the later lines and leave the child
	// unanswered; one shared driver consumes all four lines in order.
	in := strings.NewReader("weathered\n1\n0\ntheatrical\n")
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := tui.DriverFor(InstallQuestionDriverParams(theme, in, &out))
	questionOpts := []install.ResolveOption{install.PresentOver(driver, theme)}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		interactive: true, questionDriver: driver, questionTheme: theme,
		questionOpts: questionOpts, in: in, out: &out,
	})

	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{
		Interactive: true, QuestionOptions: questionOpts,
	})

	require.NoErrorf(t, err, "one driver must carry all prompts; out=%s", out.String())
	prepared := map[string]*install.Prepared{}
	for _, node := range plan.Nodes {
		prepared[node.Input.Path.String()] = node.Prepared
	}
	require.Len(t, prepared[""].Adopted, 1)
	assert.Equal(t, "AgentClass/test-coordinator", prepared[""].Adopted[0])
	rootDescription, _, rootErr := unstructured.NestedString(prepared[""].CRs[0].Object, "spec", "description")
	require.NoError(t, rootErr)
	assert.Equal(t, "weathered", rootDescription)
	childDescription, _, childErr := unstructured.NestedString(prepared[graphFixtureChild].CRs[0].Object, "spec", "description")
	require.NoError(t, childErr)
	assert.Equal(t, "theatrical", childDescription)
}

type presentImageEnv struct{}

func (presentImageEnv) Resolve(_ context.Context, ref string) (string, bool, error) {
	return ref + "@sha256:fixture", true, nil
}
func (presentImageEnv) Deliver(context.Context, oap.ImageBuild, imagebuild.Inputs, string) (string, error) {
	return "", nil
}
func (presentImageEnv) Confirm(string) bool           { return false }
func (presentImageEnv) Secret(string) (string, error) { return "", nil }
func (presentImageEnv) Path(string) (string, error)   { return "", nil }

func TestAgentInstallGraphReconcilesChildImagesWithSharedPresentationAndChildDirectory(t *testing.T) {
	dir := graphInstallFixture(t, graphInstallFixtureOptions{childManifest: `requires:
  images:
    - ref: registry.test/translator:1
`})
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t)
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := tui.DriverFor(InstallQuestionDriverParams(theme, strings.NewReader(""), &out))
	var gotDir string
	var gotDriver tui.Driver
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		questionDriver: driver, questionTheme: theme, in: strings.NewReader(""), out: &out,
		installEnv: func(_ context.Context, _ *apcmd.Globals, bundleDir string, _ buildSets, _ io.Writer, d tui.Driver, _ *tui.Theme) (imagebuild.Env, error) {
			gotDir, gotDriver = bundleDir, d
			return presentImageEnv{}, nil
		},
	})
	child := root.Dependencies[0].Bundle

	rewrites, err := w.Hooks.ResolveImages(context.Background(), install.NodeContext{
		Path: oap.DependencyPath{graphFixtureChild}, Bundle: child,
		PhysicalName: "test-coordinator-reviewer", Namespace: capacityFixtureNS,
	})

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "dependencies", graphFixtureChild), gotDir)
	assert.Same(t, driver, gotDriver)
	assert.Equal(t, "registry.test/translator:1@sha256:fixture", rewrites["registry.test/translator:1"])
}

func TestAgentInstallGraphFitResourcesDoesNotPromptForChildCapacityOnATerminal(t *testing.T) {
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
	root, err := oap.FromFolder(dir)
	require.NoError(t, err)
	kb := fakeBundle(t, vmNode(), vmPod("p1", "1Gi"), vmPod("p2", "1Gi"))
	var out bytes.Buffer
	theme := tui.NewTheme(tui.Caps{})
	driver := refusingDriver{t: t}
	w := newCLIWorkflow(cliWorkflowConfig{
		ctx: context.Background(), globals: graphGlobals(kb), kube: kb,
		root: root, sourcePath: dir, source: &installSource{kind: installSourceKindFile},
		options: cliInstallOptions{fitResources: true}, interactive: true,
		questionDriver: driver, questionTheme: theme,
		questionOpts: []install.ResolveOption{install.PresentOver(driver, theme)},
		in:           strings.NewReader(""), out: &out,
	})

	plan, err := w.Plan(context.Background(), root, install.GraphAnswers{
		Interactive: true,
		QuestionOptions: []install.ResolveOption{
			install.PresentOver(driver, theme),
		},
	})

	require.NoErrorf(t, err, "--fit-resources must accept capacity defaults without presenting them; out=%s", out.String())
	var child *install.Prepared
	for _, node := range plan.Nodes {
		if node.Input.Path.String() == graphFixtureChild {
			child = node.Prepared
		}
	}
	require.NotNil(t, child)
	var class *unstructured.Unstructured
	for _, cr := range child.CRs {
		if cr.GetKind() == "SpiceboxClass" {
			class = cr
			break
		}
	}
	require.NotNil(t, class)
	assert.Equal(t, "2Gi", memoryField(t, class))
}
