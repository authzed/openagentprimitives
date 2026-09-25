//go:build darwin && arm64

package desktopcmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

type desktopGraphFixtureOptions struct {
	rootManifest  string
	childManifest string
	childObjects  string
}

func packedDesktopGraph(t *testing.T, opts desktopGraphFixtureOptions) []byte {
	t.Helper()
	root := t.TempDir()
	child := filepath.Join(root, "dependencies", "reviewer")
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
  systemPrompt:
    inline: root
  subagents:
    - reviewer
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(child, "oap.yaml"), []byte(`
oapFormatVersion: "1"
agent:
  version: "1.0.0"
`+opts.childManifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(child, "manifests", "agent.yaml"), []byte(`
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: reviewer
spec:
  systemPrompt:
    inline: child
`+opts.childObjects), 0o644))
	bundle, err := oap.FromFolder(root)
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)
	return packed
}

func desktopGraphScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return scheme
}

func desktopGraphQuestion(defaultYAML string) string {
	return `questions:
  - name: voice
    type: string
    prompt: Voice
    required: true
` + defaultYAML + `
    binding:
      - target: AgentClass/reviewer#spec.systemPrompt.inline
`
}

func assertNoGraphAgentClasses(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, name := range []string{"test-coordinator", "test-coordinator-reviewer"} {
		var got spiceboxv1alpha1.AgentClass
		err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &got)
		assert.True(t, apierrors.IsNotFound(err), "%s must not have been installed", name)
	}
}

func TestInstallOapFromPathDefersWhenChildNeedsConfiguration(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).Build()
	result, err := installOapFromPath(context.Background(), noNodesBundle(c), io.Discard,
		packedDesktopGraph(t, desktopGraphFixtureOptions{childManifest: desktopGraphQuestion("")}), "default")
	require.ErrorIs(t, err, errNeedsConfig)
	assert.Nil(t, result)
	assertNoGraphAgentClasses(t, c)
}

func TestInstallOapFromPathInstallsChildDefaultsAndLogsGraphResult(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).Build()
	var out bytes.Buffer
	result, err := installOapFromPath(context.Background(), noNodesBundle(c), &out,
		packedDesktopGraph(t, desktopGraphFixtureOptions{childManifest: desktopGraphQuestion("    default: theatrical")}), "default")
	require.NoError(t, err)
	require.NotNil(t, result)
	var child spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-coordinator-reviewer"}, &child))
	assert.Equal(t, "theatrical", child.Spec.SystemPrompt.Inline)
	assert.Contains(t, out.String(), "reviewer")
	assert.Contains(t, out.String(), "test-coordinator-reviewer")
}

func TestInstallOapFromPathUsesAnsweredRootMetadataName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).Build()
	result, err := installOapFromPath(context.Background(), noNodesBundle(c), io.Discard,
		packedDesktopGraph(t, desktopGraphFixtureOptions{rootManifest: `questions:
  - name: root-name
    type: string
    prompt: Root name
    default: custom
    binding:
      - target: AgentClass/test-coordinator#metadata.name
`}), "default")
	require.NoError(t, err)
	require.NotNil(t, result)
	var root spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "custom"}, &root))
	var child spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "custom-reviewer"}, &child))
}

func TestInstallOapFromPathAutoFitsChildCapacity(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).Build()
	kb := &kube.Bundle{
		Typed:      k8sfake.NewSimpleClientset(aptest.NodeWithCapacity("desktop-node", "1Gi")),
		Controller: c,
	}
	var out bytes.Buffer
	_, err := installOapFromPath(context.Background(), kb, &out, packedDesktopGraph(t, desktopGraphFixtureOptions{childObjects: `
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: child-sandbox
spec:
  resources:
    memory: "4Gi"
`}), "default")
	require.NoError(t, err)
	var got spiceboxv1alpha1.SpiceboxClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "test-coordinator-reviewer-child-sandbox"}, &got))
	assert.True(t, got.Spec.Resources.Memory.Value() <= 1*1024*1024*1024)
	assert.Contains(t, out.String(), "reviewer")
}

func TestInstallOapFromPathChildAdoptionDialogNamesLogicalPath(t *testing.T) {
	preExisting := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "test-coordinator-reviewer",
	}}
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).WithObjects(preExisting).Build()
	restore := desktopAdoptDialog
	t.Cleanup(func() { desktopAdoptDialog = restore })
	var title, message string
	desktopAdoptDialog = func(gotTitle, gotMessage string) bool {
		title, message = gotTitle, gotMessage
		return true
	}
	_, err := installOapFromPath(context.Background(), noNodesBundle(c), io.Discard,
		packedDesktopGraph(t, desktopGraphFixtureOptions{}), "default")
	require.NoError(t, err)
	assert.Contains(t, title, "Adopt")
	assert.Contains(t, message, "test-coordinator > reviewer")
	assert.Contains(t, message, "test-coordinator-reviewer")
}

func TestInstallOapFromPathNeverLogsChildSecretAnswer(t *testing.T) {
	const secretValue = "task7-desktop-secret"
	c := fake.NewClientBuilder().WithScheme(desktopGraphScheme(t)).Build()
	var out bytes.Buffer
	_, err := installOapFromPath(context.Background(), noNodesBundle(c), &out,
		packedDesktopGraph(t, desktopGraphFixtureOptions{childManifest: `questions:
  - name: token
    type: secret
    prompt: Token
    required: true
    default: ` + secretValue + `
    secret:
      createSecret:
        name: child-token
        key: token
`}), "default")
	require.NoError(t, err)
	assert.NotContains(t, out.String(), secretValue)
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-coordinator-reviewer-child-token"}, &secret))
	stored := string(secret.Data["token"])
	if stored == "" {
		stored = secret.StringData["token"] // controller-runtime fake does not run the apiserver's stringData conversion
	}
	assert.Equal(t, secretValue, stored)
}
