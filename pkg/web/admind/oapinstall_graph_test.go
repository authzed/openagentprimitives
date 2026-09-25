package admind

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

type failNamedChannelPatchClient struct {
	client.Client
	name string
}

type failNamedObjectPatchClient struct {
	client.Client
	kind string
	name string
}

type apiIdentityClient struct{ client.Client }

func createWithFixtureIdentity(ctx context.Context, c client.Client, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("fixture-" + obj.GetObjectKind().GroupVersionKind().Kind + "-" + obj.GetName()))
	}
	return c.Create(ctx, obj, opts...)
}

func (c *apiIdentityClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return createWithFixtureIdentity(ctx, c.Client, obj, opts...)
}

func (c *failNamedObjectPatchClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return createWithFixtureIdentity(ctx, c.Client, obj, opts...)
}

func (c *failNamedChannelPatchClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return createWithFixtureIdentity(ctx, c.Client, obj, opts...)
}

func (c *failNamedObjectPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == c.kind && obj.GetName() == c.name {
		return fmt.Errorf("injected later node failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *failNamedChannelPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Channel" && obj.GetName() == c.name {
		return fmt.Errorf("injected channel apply failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type admindGraphFixtureOptions struct {
	rootManifest  string
	rootObjects   string
	childManifest string
	childObjects  string
}

func packedAdmindGraph(t *testing.T, opts admindGraphFixtureOptions) []byte {
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
`+opts.rootObjects), 0o644))
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

func admindGraphQuestionManifest() string {
	return `questions:
  - name: voice
    type: string
    prompt: Voice
    required: true
    binding:
      - target: AgentClass/reviewer#spec.systemPrompt.inline
`
}

func TestOapInstallMetadataNameAnswerUsesLegacyRootNaming(t *testing.T) {
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{rootManifest: `questions:
  - name: root-name
    type: string
    prompt: Root name
    binding:
      - target: AgentClass/test-coordinator#metadata.name
`})
	for _, tc := range []struct {
		name      string
		install   string
		wantRoot  string
		wantChild string
	}{
		{name: "answered name", wantRoot: "custom", wantChild: "custom-reviewer"},
		{name: "explicit prefix", install: "prefix", wantRoot: "prefix-custom", wantChild: "prefix-reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
			a := newChannelFormAdmind(t, c)
			fields := map[string]string{"namespace": "demo-ns", "values": `{"root-name":"custom"}`}
			if tc.install != "" {
				fields["name"] = tc.install
			}
			resp := postInstall(t, a, packed, fields)
			require.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())
			for _, name := range []string{tc.wantRoot, tc.wantChild} {
				var got spiceboxv1alpha1.AgentClass
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "demo-ns", Name: name}, &got))
			}
		})
	}
}

func TestOapInstallChildQuestionIsAddressedByPath(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	rec := postInstall(t, a, packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: admindGraphQuestionManifest()}), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Questions []struct {
			AgentPath string `json:"agentPath"`
			Name      string `json:"name"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Questions, 1)
	assert.Equal(t, "reviewer", body.Questions[0].AgentPath)
	assert.Equal(t, "voice", body.Questions[0].Name)
}

func TestOapInstallGraphReturnsEveryMissingQuestionInOneResponse(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	rootQuestion := `questions:
  - name: ship
    type: string
    prompt: Ship
    required: true
    binding:
      - target: AgentClass/test-coordinator#spec.systemPrompt.inline
`
	rec := postInstall(t, a, packedAdmindGraph(t, admindGraphFixtureOptions{
		rootManifest: rootQuestion, childManifest: admindGraphQuestionManifest(),
	}), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Questions []struct {
			AgentPath string `json:"agentPath"`
			Name      string `json:"name"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Questions, 2)
	assert.Equal(t, "ship", body.Questions[0].Name)
	assert.Empty(t, body.Questions[0].AgentPath)
	assert.Equal(t, "voice", body.Questions[1].Name)
	assert.Equal(t, "reviewer", body.Questions[1].AgentPath)
}

func TestOapInstallChildCapacityQuestionIsAddressedByPath(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a, err := New(Config{
		Mem: memory.NewLocal(inmem.NewBackend()), K8s: c,
		Checker: allowChecker{canonical: "YWRtaW4"}, Token: "test-token",
		Clientset: k8sfake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "desktop-node"},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("2"),
				corev1.ResourceMemory:           resource.MustParse("1Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			}},
		}),
	})
	require.NoError(t, err)
	rec := postInstall(t, a, packedAdmindGraph(t, admindGraphFixtureOptions{childObjects: `
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: child-sandbox
spec:
  resources:
    memory: "4Gi"
`}), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Questions []struct {
			AgentPath string `json:"agentPath"`
			Name      string `json:"name"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Questions)
	assert.Equal(t, "reviewer", body.Questions[0].AgentPath)
	assert.Contains(t, body.Questions[0].Name, "child-sandbox")

	values, err := json.Marshal(map[string]string{
		"agents.reviewer." + body.Questions[0].Name: "768Mi",
	})
	require.NoError(t, err)
	done := postInstall(t, a, packedAdmindGraph(t, admindGraphFixtureOptions{childObjects: `
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxClass
metadata:
  name: child-sandbox
spec:
  resources:
    memory: "4Gi"
`}), map[string]string{"namespace": "demo-ns", "values": string(values)})
	require.Equal(t, http.StatusOK, done.Code, "body: %s", done.Body.String())
	var success oapInstallResponse
	require.NoError(t, json.Unmarshal(done.Body.Bytes(), &success))
	require.Len(t, success.Agents, 1)
	seen := map[string]bool{}
	for _, warning := range success.Agents[0].Warnings {
		assert.False(t, seen[warning], "success response repeated warning %q", warning)
		seen[warning] = true
	}
}

func TestOapInstallChildChannelInputsAreAddressedByPath(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: slack
      role: input
      name: reviewer-slack
    - kind: fake
      role: output
      name: delivery
`})
	rec := postInstall(t, a, packed, map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Channels []struct {
			AgentPath string         `json:"agentPath"`
			Kind      string         `json:"kind"`
			Questions []oap.Question `json:"questions"`
		} `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Channels, 2)
	assert.Equal(t, "reviewer", body.Channels[0].AgentPath)
	assert.Equal(t, "slack", body.Channels[0].Kind)
	assert.NotEmpty(t, body.Channels[0].Questions)
}

func TestOapInstallGraphStagesChildChannelBeforeExecutingBundle(t *testing.T) {
	const namespace = "demo-ns"
	base := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
		},
	}).Build()
	c := &apiIdentityClient{Client: base}
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})

	first := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, first.Code, "body: %s", first.Body.String())
	var decision oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &decision))
	row := channelRowNamed(t, decision, "test-coordinator-reviewer-review-bento")
	assert.Equal(t, "reviewer", row.AgentPath)
	require.NotEmpty(t, row.SetupToken)
	pending, err := a.channelSetups.get(row.SetupToken, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"))
	require.NoError(t, err)
	require.NotNil(t, pending.graphBinding)
	assert.Equal(t, namespace, pending.graphBinding.RootNamespace)
	assert.Equal(t, "test-coordinator", pending.graphBinding.RootInstall)
	assert.NotEmpty(t, pending.graphBinding.RootDigest)

	var root spiceboxv1alpha1.AgentClass
	assert.Error(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root),
		"the complete graph must remain untouched until every staged channel is ready")

	setup := postChannelSetup(t, a, row.SetupToken, "user:YWRtaW4", map[string]string{
		"authzsubject": "service:child-bot",
		"interval":     "@every 24h",
	})
	require.Equal(t, http.StatusOK, setup.Code, "body: %s", setup.Body.String())
	assert.NotContains(t, setup.Body.String(), "service:child-bot")
	var channel spiceboxv1alpha1.Channel
	assert.Error(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: row.Name}, &channel),
		"setup resolves and seals output but must not apply it")

	final := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusOK, final.Code, "body: %s", final.Body.String())
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: row.Name}, &channel))
	assert.Equal(t, "test-coordinator", channel.Labels[instance.LabelInstall])
	assert.Equal(t, namespace, channel.Labels[instance.LabelInstallNamespace])
	assert.Equal(t, "reviewer", channel.Annotations[install.AnnotationDependencyPath])
	var credential corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: row.Name + "-creds"}, &credential))
	assert.Equal(t, "test-coordinator", credential.Labels[instance.LabelInstall])
	assert.Equal(t, namespace, credential.Labels[instance.LabelInstallNamespace])
	assert.Equal(t, "reviewer", credential.Annotations[install.AnnotationDependencyPath])

	again := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusOK, again.Code, "an already wired reinstall must not reuse the consumed token: %s", again.Body.String())
}

func TestOapInstallGraphCredentialSecretOwnershipPreflight(t *testing.T) {
	const (
		namespace   = "demo-ns"
		channelName = "test-coordinator-reviewer-review-bento"
		secretName  = channelName + "-creds"
	)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})
	for _, tc := range []struct {
		name      string
		labels    map[string]string
		wantError bool
	}{
		{name: "another graph", labels: map[string]string{instance.LabelInstall: "other-root", instance.LabelInstallNamespace: namespace}, wantError: true},
		{name: "project managed without graph owner", labels: nil, wantError: true},
		{name: "same graph reinstall", labels: map[string]string{instance.LabelInstall: "test-coordinator", instance.LabelInstallNamespace: namespace}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
				Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer"},
			}).Build()
			c := &apiIdentityClient{Client: base}
			a := newChannelFormAdmind(t, c)
			first := postInstall(t, a, packed, map[string]string{"namespace": namespace})
			require.Equal(t, http.StatusBadRequest, first.Code, "body: %s", first.Body.String())
			var decision oapInstallMissingQuestionsResponse
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &decision))
			row := channelRowNamed(t, decision, channelName)
			setup := postChannelSetup(t, a, row.SetupToken, "user:YWRtaW4", map[string]string{
				"authzsubject": "service:child-bot", "interval": "@every 24h",
			})
			require.Equal(t, http.StatusOK, setup.Code, "body: %s", setup.Body.String())
			require.NoError(t, c.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace, Name: secretName, Labels: tc.labels,
				Annotations: map[string]string{wizardrun.InstalledByAnnotation: installedByAdmind},
			}, Data: map[string][]byte{"token": []byte("preserve-me")}}))

			final := postInstall(t, a, packed, map[string]string{"namespace": namespace})
			if tc.wantError {
				require.Equal(t, http.StatusConflict, final.Code, "body: %s", final.Body.String())
				assert.Contains(t, final.Body.String(), secretName)
				var got corev1.Secret
				require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: secretName}, &got))
				assert.Equal(t, []byte("preserve-me"), got.Data["token"])
				assert.Equal(t, tc.labels, got.Labels)
				assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &spiceboxv1alpha1.AgentClass{})))

				adopt, marshalErr := json.Marshal([]string{"Secret/" + secretName})
				require.NoError(t, marshalErr)
				refused := postInstall(t, a, packed, map[string]string{"namespace": namespace, "adopt": string(adopt)})
				require.Equal(t, http.StatusBadRequest, refused.Code, "body: %s", refused.Body.String())
				assert.Contains(t, refused.Body.String(), "refusing to adopt")
				require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: secretName}, &got))
				assert.Equal(t, []byte("preserve-me"), got.Data["token"])
				deleted, uninstallErr := install.UninstallGraph(context.Background(), base, "test-coordinator", namespace)
				require.NoError(t, uninstallErr)
				assert.Zero(t, deleted)
				require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: secretName}, &corev1.Secret{}))
				return
			}

			require.Equal(t, http.StatusOK, final.Code, "body: %s", final.Body.String())
			var got corev1.Secret
			require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: secretName}, &got))
			assert.Equal(t, "test-coordinator", got.Labels[instance.LabelInstall])
			assert.Equal(t, "reviewer", got.Annotations[install.AnnotationDependencyPath])
		})
	}
}

func TestOapInstallGraphChannelApplyFailureInvalidatesTokenAndCleansOnce(t *testing.T) {
	const (
		namespace   = "demo-ns"
		channelName = "test-coordinator-reviewer-review-bento"
	)
	base := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
		},
	}).Build()
	c := &failNamedChannelPatchClient{Client: base, name: channelName}
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})

	first := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, first.Code, "body: %s", first.Body.String())
	var decision oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &decision))
	row := channelRowNamed(t, decision, channelName)
	setup := postChannelSetup(t, a, row.SetupToken, "user:YWRtaW4", map[string]string{
		"authzsubject": "service:child-bot", "interval": "@every 24h",
	})
	require.Equal(t, http.StatusOK, setup.Code, "body: %s", setup.Body.String())
	var cleanups atomic.Int32
	_, err := a.channelSetups.update(row.SetupToken, identity.CanonicalFromTrusted("YWRtaW4", "test fixture"), func(p *pendingChannelSetup) {
		p.cleanup = &channelSetupCleanup{fn: func(context.Context) error { cleanups.Add(1); return nil }}
	})
	require.NoError(t, err)

	failed := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusInternalServerError, failed.Code, "body: %s", failed.Body.String())
	assert.Contains(t, failed.Body.String(), "injected channel apply failure")
	assert.Equal(t, int32(1), cleanups.Load(), "apply invalidation and workflow rollback must share one cleanup receipt")
	var root spiceboxv1alpha1.AgentClass
	assert.Error(t, base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root))
	var partialSecret corev1.Secret
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: channelName + "-creds"}, &partialSecret)),
		"a Secret created before the Channel patch failed must be removed by the tracked apply receipt")

	retry := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, retry.Code, "body: %s", retry.Body.String())
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &decision))
	retryRow := channelRowNamed(t, decision, channelName)
	assert.NotEqual(t, row.SetupToken, retryRow.SetupToken, "a failed apply must invalidate the claimed token")
}

func TestOapInstallGraphLaterNodeFailureRollsBackAppliedChannelReceipt(t *testing.T) {
	const (
		namespace   = "demo-ns"
		channelName = "test-coordinator-reviewer-review-bento"
	)
	base := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
		},
	}).Build()
	c := &failNamedObjectPatchClient{Client: base, kind: "AgentClass", name: "test-coordinator"}
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})

	first := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, first.Code, "body: %s", first.Body.String())
	var decision oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &decision))
	row := channelRowNamed(t, decision, channelName)
	setup := postChannelSetup(t, a, row.SetupToken, "user:YWRtaW4", map[string]string{
		"authzsubject": "service:child-bot", "interval": "@every 24h",
	})
	require.Equal(t, http.StatusOK, setup.Code, "body: %s", setup.Body.String())

	failed := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusInternalServerError, failed.Code, "body: %s", failed.Body.String())
	assert.Contains(t, failed.Body.String(), "injected later node failure")
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: channelName}, &spiceboxv1alpha1.Channel{})))
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: channelName + "-creds"}, &corev1.Secret{})))
}

func TestOapInstallGraphTokenMintFailurePreventsExecution(t *testing.T) {
	const namespace = "demo-ns"
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
		},
	}).Build()
	a := newChannelFormAdmind(t, c)
	a.channelSetups.max = 0
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})

	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "too many channel setups")
	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	row := channelRowNamed(t, body, "test-coordinator-reviewer-review-bento")
	assert.Empty(t, row.SetupToken)
	var root spiceboxv1alpha1.AgentClass
	assert.Error(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root))
}

func TestOapInstallGraphChannelConflictIsARowAndPreventsExecution(t *testing.T) {
	const namespace = "demo-ns"
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-review-bento"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "fake", Role: spiceboxv1alpha1.ChannelRoleInput, AgentClass: "someone-else",
			},
		},
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
			},
		},
	).Build()
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: review-bento
    - kind: slack
      role: output
      name: delivery
`})

	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	var body oapInstallMissingQuestionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	row := channelRowNamed(t, body, "test-coordinator-reviewer-review-bento")
	assert.Equal(t, channelFormConflict, row.Status)
	assert.Empty(t, row.SetupToken)
	var root spiceboxv1alpha1.AgentClass
	assert.Error(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root))
}

func TestOapInstallChildConflictIsAddressedByPath(t *testing.T) {
	const namespace = "demo-ns"
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: "test-coordinator-reviewer-child-config",
	}}
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(foreign).Build()
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childObjects: `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: child-config
data:
  value: bundled
`})
	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Conflicts []struct {
			AgentPath string `json:"agentPath"`
			Kind      string `json:"kind"`
			Name      string `json:"name"`
		} `json:"conflicts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Conflicts, 1)
	assert.Equal(t, "reviewer", body.Conflicts[0].AgentPath)
	assert.Equal(t, "ConfigMap", body.Conflicts[0].Kind)

	adopt, err := json.Marshal([]string{"ConfigMap/test-coordinator-reviewer-child-config"})
	require.NoError(t, err)
	rec = postInstall(t, a, packed, map[string]string{"namespace": namespace, "adopt": string(adopt)})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var adopted corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: foreign.Name}, &adopted))
	assert.Equal(t, "bundled", adopted.Data["value"])
}

func TestOapInstallGraphReturnsEveryConflictInOneResponse(t *testing.T) {
	const namespace = "demo-ns"
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "root-config"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-child-config"}},
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-delivery"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "fake", Role: spiceboxv1alpha1.ChannelRoleOutput, AgentClass: "test-coordinator-reviewer",
			},
		},
	).Build()
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{
		rootObjects: `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: root-config
`,
		childObjects: `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: child-config
`,
		childManifest: `requires:
  channels:
    - kind: fake
      role: output
      name: delivery
`,
	})
	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace})
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Conflicts []struct {
			AgentPath string `json:"agentPath"`
			Name      string `json:"name"`
		} `json:"conflicts"`
		Channels []oapInstallChannel `json:"channels"`
		Warnings []string            `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Conflicts, 2)
	assert.Equal(t, "root-config", body.Conflicts[0].Name)
	assert.Empty(t, body.Conflicts[0].AgentPath)
	assert.Equal(t, "test-coordinator-reviewer-child-config", body.Conflicts[1].Name)
	assert.Equal(t, "reviewer", body.Conflicts[1].AgentPath)
	require.Len(t, body.Channels, 1)
	assert.Equal(t, channelFormAlreadyWired, body.Channels[0].Status)
	assert.Equal(t, "reviewer", body.Channels[0].AgentPath)
	assert.NotEmpty(t, body.Warnings)
}

func TestOapInstallGraphRejectsStaleAdoptionKeyBeforeMutation(t *testing.T) {
	const namespace = "demo-ns"
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-coordinator-reviewer-child-config"}}
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(foreign).Build()
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childObjects: `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: child-config
data:
  value: bundled
`})
	adopt, err := json.Marshal([]string{
		"ConfigMap/test-coordinator-reviewer-child-config",
		"ConfigMap/stale-object",
	})
	require.NoError(t, err)
	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace, "adopt": string(adopt)})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "stale-object")
	var root spiceboxv1alpha1.AgentClass
	assert.Error(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "test-coordinator"}, &root))
}

func TestOapInstallGraphFatalInspectionErrorIsNotHiddenByStaleAdoption(t *testing.T) {
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `requires:
  channels:
    - kind: bento
      role: input
      name: trigger
`})
	rec := postInstall(t, a, packed, map[string]string{
		"namespace": "demo-ns", "adopt": `["ConfigMap/stale"]`,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "declares no role")
	assert.NotContains(t, rec.Body.String(), "not a conflicting object")
}

func TestOapInstallGraphExplicitlyRefusesSecretAdoption(t *testing.T) {
	const namespace = "demo-ns"
	physicalSecret := "test-coordinator-reviewer-child-token"
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: physicalSecret},
	}).Build()
	a := newChannelFormAdmind(t, c)
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `questions:
  - name: token
    type: secret
    prompt: Token
    required: true
    secret:
      createSecret:
        name: child-token
        key: token
`})
	values, err := json.Marshal(map[string]string{"agents.reviewer.token": "new-secret"})
	require.NoError(t, err)
	adopt, err := json.Marshal([]string{"Secret/" + physicalSecret})
	require.NoError(t, err)
	rec := postInstall(t, a, packed, map[string]string{"namespace": namespace, "values": string(values), "adopt": string(adopt)})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "refusing to adopt")
	assert.NotContains(t, rec.Body.String(), "new-secret")
}

func TestOapInstallGraphSuccessReturnsNestedResults(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, c)
	rec := postInstall(t, a, packedAdmindGraph(t, admindGraphFixtureOptions{}), map[string]string{"namespace": "demo-ns"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Name   string `json:"name"`
		Agents []struct {
			AgentPath string `json:"agentPath"`
			Name      string `json:"name"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "test-coordinator", body.Name)
	require.Len(t, body.Agents, 1)
	assert.Equal(t, "reviewer", body.Agents[0].AgentPath)
	assert.Equal(t, "test-coordinator-reviewer", body.Agents[0].Name)

	var child spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "demo-ns", Name: "test-coordinator-reviewer"}, &child))
}

func TestOapInstallGraphRejectsSecretValueWithoutEchoingIt(t *testing.T) {
	const secretValue = "task7-super-secret-value"
	a := newChannelFormAdmind(t, ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build())
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `questions:
  - name: token
    type: secret
    prompt: Token
    required: true
    secret:
      createSecret:
        name: child-token
        key: token
`})
	values, err := json.Marshal(map[string]string{"agents.reviewer.token": secretValue, "agents.reviewer.unknown": "bad"})
	require.NoError(t, err)
	rec := postInstall(t, a, packed, map[string]string{"namespace": "demo-ns", "values": string(values)})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), secretValue)
}

func TestRedactPostedValuesRedactsOverlappingSecretsLongestFirst(t *testing.T) {
	const (
		short = "task7-token"
		long  = "task7-token-LONG-SECRET-TAIL"
	)
	for i := 0; i < 200; i++ {
		got := redactPostedValues("provider rejected "+long, map[string]string{
			"short": short,
			"long":  long,
		})
		assert.NotContains(t, got, "LONG-SECRET-TAIL", "iteration %d leaked the longer secret tail: %q", i, got)
		assert.NotContains(t, got, long)
	}
}

type admindApplyRollbackFailureClient struct {
	client.Client
	secret string
}

func (c *admindApplyRollbackFailureClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetName() == "test-coordinator" {
		return fmt.Errorf("root apply failed around %s", c.secret)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *admindApplyRollbackFailureClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetName() == "test-coordinator-reviewer" {
		return fmt.Errorf("child rollback failed around %s", c.secret)
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestOapInstallGraphRollbackFailureIsPathQualifiedAndRedacted(t *testing.T) {
	const secretValue = "task7-rollback-secret"
	base := ctrlfake.NewClientBuilder().WithScheme(channelTestScheme(t)).Build()
	a := newChannelFormAdmind(t, &admindApplyRollbackFailureClient{Client: base, secret: secretValue})
	packed := packedAdmindGraph(t, admindGraphFixtureOptions{childManifest: `questions:
  - name: token
    type: secret
    prompt: Token
    required: true
    secret:
      createSecret:
        name: child-token
        key: token
`})
	values, err := json.Marshal(map[string]string{"agents.reviewer.token": secretValue})
	require.NoError(t, err)
	rec := postInstall(t, a, packed, map[string]string{"namespace": "demo-ns", "values": string(values)})
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "install test-coordinator: apply")
	assert.Contains(t, body.Error, "install test-coordinator > reviewer: rollback")
	assert.Contains(t, body.Error, "<redacted id=")
	assert.NotContains(t, body.Error, secretValue)
}
