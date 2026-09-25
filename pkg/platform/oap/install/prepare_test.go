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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

type recordingClient struct {
	client.Client
	mutations []ObjectKey
}

func (c *recordingClient) record(obj client.Object) {
	c.mutations = append(c.mutations, ObjectKey{GVK: obj.GetObjectKind().GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()})
}
func (c *recordingClient) Mutations() []ObjectKey { return c.mutations }
func (c *recordingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.record(obj)
	return c.Client.Create(ctx, obj, opts...)
}
func (c *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.record(obj)
	return c.Client.Update(ctx, obj, opts...)
}
func (c *recordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.record(obj)
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *recordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.record(obj)
	return c.Client.Delete(ctx, obj, opts...)
}

func TestPrepareIsReadOnly(t *testing.T) {
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	p, err := Prepare(context.Background(), c, validBundle(), nil, nil, InstallOpts{Namespace: "agents"})
	require.NoError(t, err)
	assert.NotNil(t, p)
	assert.Empty(t, c.Mutations())
}

func TestPrepareTransformsWithoutWrites(t *testing.T) {
	b := validBundle()
	b.Manifests = []byte(strings.ReplaceAll(string(b.Manifests), "    inline: hi", "    inline: hi\n  authz:\n    slots:\n    - name: viewers\n  toolBundles:\n  - name: tools\n    class: sandbox") + "\n---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: SpiceboxClass\nmetadata:\n  name: sandbox\nspec:\n  image: local:dev\n")
	b.Manifest.Questions = append(b.Manifest.Questions, oap.Question{Name: "prompt", Type: oap.QString, Prompt: "Prompt", Binding: []oap.Binding{{Target: "AgentClass/fixture-agent#spec.systemPrompt.inline"}}})
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	p, err := Prepare(context.Background(), c, b, oap.Answers{"prompt": "answered"}, []SecretSpec{{Name: "widget-token", Key: "token", Value: "private-credential"}}, InstallOpts{
		Namespace: "agents", RootInstallName: "root", DependencyPath: oap.DependencyPath{"child"},
		ResourceNames: instance.NameMap{"AgentClass/fixture-agent": "root-child", "SpiceboxClass/sandbox": "root-sandbox", "Secret/widget-token": "root-token"},
		SourceKind:    "embedded", SourceRef: "agents/child", SourceDigest: "sha256:child",
		ImageRewrites: map[string]string{"local:dev": "remote@sha256:resolved"},
		Sets:          map[string]string{"capacity": "adjusted"},
		ExtraQuestions: func(_ context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			value, _, err := unstructured.NestedString(crs[0].Object, "spec", "systemPrompt", "inline")
			require.NoError(t, err)
			assert.Equal(t, "answered", value)
			return []oap.Question{{Name: "capacity", Type: oap.QString, Prompt: "Capacity", Binding: []oap.Binding{{Target: "AgentClass/root-child#spec.systemPrompt.inline"}}}}, []string{"capacity notice"}, nil
		},
	})
	require.NoError(t, err)
	assert.Empty(t, c.Mutations())
	assert.Equal(t, "root-child", p.InstallName)
	assert.Equal(t, "root-child", p.Owner.Name)
	assert.Equal(t, []string{"capacity notice"}, p.Warnings)
	ac := p.CRs[0]
	assert.Equal(t, "agents", ac.GetNamespace())
	assert.Equal(t, "root", ac.GetLabels()[instance.LabelInstall])
	assert.Equal(t, "child", ac.GetAnnotations()[AnnotationDependencyPath])
	prompt, _, err := unstructured.NestedString(ac.Object, "spec", "systemPrompt", "inline")
	require.NoError(t, err)
	assert.Equal(t, "adjusted", prompt)
	slots, _, err := unstructured.NestedSlice(ac.Object, "spec", "authz", "slots")
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.AuthzSlotMembershipDefault, slots[0].(map[string]any)["membership"])
	refs, _, err := unstructured.NestedSlice(ac.Object, "spec", "toolBundles")
	require.NoError(t, err)
	assert.Equal(t, "root-sandbox", refs[0].(map[string]any)["class"])
	image, _, err := unstructured.NestedString(p.CRs[1].Object, "spec", "image")
	require.NoError(t, err)
	assert.Equal(t, "remote@sha256:resolved", image)
	assert.Empty(t, p.CRs[1].GetNamespace())
	source, err := instance.ParseOapSource(ac.GetAnnotations()[instance.AnnotationOapSource])
	require.NoError(t, err)
	assert.Equal(t, instance.OapSource{SourceKind: "embedded", Ref: "agents/child", Digest: "sha256:child", Version: "1.0.0"}, source)
	assert.Equal(t, "root-token", p.secretObjects[0].GetName())
	assert.Equal(t, "root", p.secretObjects[0].GetLabels()[instance.LabelInstall])
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private-credential")
	assert.NotContains(t, fmt.Sprintf("%+v %#v", p, p), "private-credential")
}

func TestPrepareMetadataNameBindingKeepsAuthoredIdentityForPhysicalMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts InstallOpts
		want string
	}{
		{name: "dependency-free direct compatibility", opts: InstallOpts{Namespace: "agents"}, want: "answer-name"},
		{name: "root graph without explicit name", opts: InstallOpts{Namespace: "agents", ResourceNames: instance.NameMap{"AgentClass/fixture-agent": "fixture-agent"}}, want: "fixture-agent"},
		{name: "root graph with explicit name", opts: InstallOpts{Namespace: "agents", RootInstallName: "private", ResourceNames: instance.NameMap{"AgentClass/fixture-agent": "private-fixture-agent"}}, want: "private-fixture-agent"},
		{name: "nested private node", opts: InstallOpts{Namespace: "agents", RootInstallName: "private", DependencyPath: oap.DependencyPath{"reviewer"}, ResourceNames: instance.NameMap{"AgentClass/fixture-agent": "private-reviewer"}}, want: "private-reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle()
			if tc.opts.ResourceNames != nil {
				b.Manifests = []byte(strings.Replace(string(b.Manifests), "  systemPrompt:", "  agentIdentity: fixture-identity\n  systemPrompt:", 1) + `---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: fixture-identity
spec:
  credentials: []
`)
				tc.opts.ResourceNames["AgentIdentity/fixture-identity"] = tc.want + "-identity"
			}
			b.Manifest.Questions = append(b.Manifest.Questions, oap.Question{
				Name: "resourceName", Type: oap.QString, Prompt: "Resource name",
				Binding: []oap.Binding{{Target: "AgentClass/fixture-agent#metadata.name"}},
			})
			c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}

			p, err := Prepare(context.Background(), c, b, oap.Answers{"resourceName": "answer-name"}, nil, tc.opts)

			require.NoError(t, err)
			wantCRs := 1
			if tc.opts.ResourceNames != nil {
				wantCRs = 2
			}
			require.Len(t, p.CRs, wantCRs)
			var preparedAgent *unstructured.Unstructured
			for _, cr := range p.CRs {
				if cr.GetKind() == "AgentClass" {
					preparedAgent = cr
				}
			}
			require.NotNil(t, preparedAgent)
			assert.Equal(t, tc.want, preparedAgent.GetName())
			assert.Equal(t, tc.want, p.InstallName)
			if tc.opts.ResourceNames != nil {
				ref, found, refsErr := unstructured.NestedString(preparedAgent.Object, "spec", "agentIdentity")
				require.NoError(t, refsErr)
				require.True(t, found)
				assert.Equal(t, tc.want+"-identity", ref)
			}
			assert.Empty(t, c.Mutations())
		})
	}
}

func TestPrepareGuardsAreReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		invalid, adopt, pinMismatch bool
		wantError                   string
	}{
		{name: "invalid bundle", invalid: true, wantError: "bundle invalid"},
		{name: "foreign class refused", wantError: "refusing to overwrite"},
		{name: "foreign class adopted", adopt: true},
		{name: "pin mismatch refused", adopt: true, pinMismatch: true, wantError: "pin mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle()
			if tc.invalid {
				b.Manifest.OapFormatVersion = "99"
			}
			dep := &unstructured.Unstructured{Object: map[string]any{"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": "SpiceboxToolkit", "metadata": map[string]any{"name": "shared"}, "status": map[string]any{"pin": map[string]any{"digest": "sha256:live"}}}}
			b.Manifest.Requires.ClusterDeps = []oap.RequiredClusterDep{{Kind: "SpiceboxToolkit", Name: "shared"}}
			if tc.pinMismatch {
				b.Manifest.Requires.ClusterDeps[0].Pin = &oap.Pin{Digest: "sha256:different"}
			}
			foreign := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent", Namespace: "agents"}}
			c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(foreign, dep).Build()}
			p, err := Prepare(context.Background(), c, b, nil, nil, InstallOpts{Namespace: "agents", AdoptAll: tc.adopt})
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, []string{"AgentClass/fixture-agent"}, p.Adopted)
				assert.True(t, p.Existing[ObjectKey{GVK: v1alpha1.SchemeGroupVersion.WithKind("AgentClass"), Namespace: "agents", Name: "fixture-agent"}])
			}
			assert.Empty(t, c.Mutations())
		})
	}
}

func TestApplyPreparedRefusesConcurrentCreation(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	p, err := Prepare(ctx, c, validBundle(), nil, []SecretSpec{{Name: "new-secret", Key: "token", Value: "private"}}, InstallOpts{Namespace: "agents"})
	require.NoError(t, err)
	foreign := &v1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent", Namespace: "agents"}, Spec: v1alpha1.AgentClassSpec{SystemPrompt: v1alpha1.PromptSource{Inline: "foreign"}}}
	require.NoError(t, c.Create(ctx, foreign))
	_, err = ApplyPrepared(ctx, c, p)
	require.Error(t, err)
	assert.True(t, apierrors.IsAlreadyExists(err))
	require.NoError(t, RollbackPrepared(ctx, c, p))
	var got v1alpha1.AgentClass
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(foreign), &got))
	assert.Equal(t, "foreign", got.Spec.SystemPrompt.Inline)
	var secret corev1.Secret
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "new-secret"}, &secret)))
}

func TestApplyPreparedCreatesThenAppliesAndReinstallConverges(t *testing.T) {
	ctx := context.Background()
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	for i := 0; i < 2; i++ {
		p, err := Prepare(ctx, c, validBundle(), nil, nil, InstallOpts{Namespace: "agents"})
		require.NoError(t, err)
		res, err := ApplyPrepared(ctx, c, p)
		require.NoError(t, err)
		assert.Equal(t, "fixture-agent", res.Name)
	}
	assert.Len(t, c.Mutations(), 3, "one Create, first SSA, and reinstall SSA")
}

func TestApplyPreparedCombinesKeysInOneSecret(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	p, err := Prepare(ctx, c, validBundle(), nil, []SecretSpec{
		{Name: "widget-token", Key: "user", Value: "test-user"},
		{Name: "widget-token", Key: "token", Value: "private-token"},
	}, InstallOpts{Namespace: "agents", ResourceNames: instance.NameMap{"AgentClass/fixture-agent": "root-child", "Secret/widget-token": "root-child-token"}})
	require.NoError(t, err)
	_, err = ApplyPrepared(ctx, c, p)
	require.NoError(t, err)
	var got corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "root-child-token"}, &got))
	assert.Equal(t, map[string]string{"user": "test-user", "token": "private-token"}, got.StringData)
}

func TestPreparedSecretValuesStayOutOfErrorsAndRenderedValues(t *testing.T) {
	const value = "private-credential-never-render"
	primary := errors.New("upstream echoed " + value)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return primary },
	}).Build()
	p, err := Prepare(context.Background(), c, validBundle(), nil, []SecretSpec{{Name: "widget-token", Key: "token", Value: value}}, InstallOpts{Namespace: "agents"})
	require.NoError(t, err)
	assert.NotContains(t, fmt.Sprintf("%+v %#v", *p, *p), value)
	_, err = ApplyPrepared(context.Background(), c, p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), value)
	assert.NotErrorIs(t, err, primary, "a redacted error must not unwrap to its credential-bearing cause")
}

func TestPrepareReinstallMapsDeclaredSecretWithoutWritingIt(t *testing.T) {
	b := validBundle()
	b.Manifests = append(b.Manifests, []byte("  agentIdentity: identity\n---\napiVersion: agentprimitives.authzed.com/v1alpha1\nkind: AgentIdentity\nmetadata:\n  name: identity\nspec:\n  credentials:\n  - name: widget\n    type: static\n    static:\n      secretRef:\n        name: widget-token\n        key: token\n")...)
	c := &recordingClient{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	p, err := Prepare(context.Background(), c, b, nil, nil, InstallOpts{Namespace: "agents", ResourceNames: instance.NameMap{
		"AgentClass/fixture-agent": "root-child", "AgentIdentity/identity": "root-child-identity", "Secret/widget-token": "root-child-token",
	}})
	require.NoError(t, err)
	credentials, _, err := unstructured.NestedSlice(p.CRs[1].Object, "spec", "credentials")
	require.NoError(t, err)
	credential := credentials[0].(map[string]any)
	name, _, err := unstructured.NestedString(credential, "static", "secretRef", "name")
	require.NoError(t, err)
	assert.Equal(t, "root-child-token", name)
	assert.Empty(t, p.secretObjects)
	assert.Empty(t, c.Mutations())
}
