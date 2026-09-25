package wizardrun

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

type trackedApplyClient struct {
	client.Client
	t              *testing.T
	failPatchKind  string
	createRaceKind string
	afterFirstGet  string
	afterGet       func(context.Context, client.Object)
	getCounts      map[string]int
	patchFn        func(context.Context, client.Object, client.Patch, ...client.PatchOption) error
}

func (c *trackedApplyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		switch obj.(type) {
		case *corev1.Secret:
			kind = "Secret"
		case *spiceboxv1alpha1.Channel:
			kind = "Channel"
		}
	}
	if kind == c.afterFirstGet {
		if c.getCounts == nil {
			c.getCounts = map[string]int{}
		}
		c.getCounts[kind]++
		if c.getCounts[kind] == 1 && c.afterGet != nil {
			c.afterGet(ctx, obj)
		}
	}
	return err
}

func (c *trackedApplyClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("uid-" + obj.GetObjectKind().GroupVersionKind().Kind + "-" + obj.GetName()))
	}
	if obj.GetObjectKind().GroupVersionKind().Kind == c.createRaceKind {
		foreign := obj.DeepCopyObject().(client.Object)
		foreign.SetUID(types.UID("foreign-uid"))
		require.NoError(c.t, c.Client.Create(ctx, foreign))
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: obj.GetObjectKind().GroupVersionKind().Kind}, obj.GetName())
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *trackedApplyClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.patchFn != nil {
		return c.patchFn(ctx, obj, patch, opts...)
	}
	if obj.GetObjectKind().GroupVersionKind().Kind == c.failPatchKind {
		return errors.New("injected patch failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func externallyUpdateTrackedChannel(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	var current spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &current))
	current.Labels = map[string]string{"updated": "externally"}
	require.NoError(t, c.Update(ctx, &current))
}

func TestApplyTrackedReturnsReceiptThatRollsBackPartialCreatesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	c := &trackedApplyClient{Client: ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build(), t: t, failPatchKind: "Channel"}
	out := wizardOutput()
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.ErrorContains(t, err, "injected patch failure")
	require.NotNil(t, receipt)
	require.NoError(t, receipt.Rollback(ctx, c))
	require.NoError(t, receipt.Rollback(ctx, c), "rollback is one-shot and replay-safe")
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, out.SecretManifest.DeepCopy())))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, out.ChannelManifest.DeepCopy())))
}

func TestApplyTrackedConcurrentCreateFailsClosedAndPreservesCompetitor(t *testing.T) {
	ctx := context.Background()
	c := &trackedApplyClient{Client: ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build(), t: t, createRaceKind: "Secret"}
	out := wizardOutput()
	out.ChannelManifest = nil
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.Error(t, err)
	require.NotNil(t, receipt)
	require.NoError(t, receipt.Rollback(ctx, c))
	var got = out.SecretManifest.DeepCopy()
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, got))
	assert.Equal(t, types.UID("foreign-uid"), got.UID)
}

func TestApplyTrackedCarriesAbsentGuardSnapshotIntoCreate(t *testing.T) {
	ctx := context.Background()
	base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := &trackedApplyClient{Client: base, t: t, afterFirstGet: "Secret"}
	c.afterGet = func(ctx context.Context, _ client.Object) {
		competitor := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-channel-creds", Namespace: "demo-ns", UID: "competitor-uid"}}
		require.NoError(t, base.Create(ctx, competitor))
	}
	out := wizardOutput()
	out.ChannelManifest = nil
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.Error(t, err)
	require.NoError(t, receipt.Rollback(ctx, c))
	var got corev1.Secret
	require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, &got))
	assert.Equal(t, types.UID("competitor-uid"), got.UID)
	assert.Empty(t, got.Annotations[InstalledByAnnotation])
}

func TestApplyTrackedRefusesDeletedOrReplacedPreexistingSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{name: "deleted"},
		{name: "replaced", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "demo-channel-creds", Namespace: "demo-ns", UID: "original-uid", ResourceVersion: "1",
				Annotations: map[string]string{InstalledByAnnotation: "ap"},
			}}
			base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
			c := &trackedApplyClient{Client: base, t: t, afterFirstGet: "Secret"}
			c.afterGet = func(ctx context.Context, obj client.Object) {
				require.NoError(t, base.Delete(ctx, obj.DeepCopyObject().(client.Object)))
				if tc.replace {
					replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-channel-creds", Namespace: "demo-ns", UID: "replacement-uid"}}
					require.NoError(t, base.Create(ctx, replacement))
				}
			}
			out := wizardOutput()
			out.ChannelManifest = nil
			out.CapabilityPatch = nil

			receipt, err := ApplyTracked(ctx, c, "ap", out)
			require.Error(t, err)
			require.NoError(t, receipt.Rollback(ctx, c))
			if tc.replace {
				var got corev1.Secret
				require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, &got))
				assert.Equal(t, types.UID("replacement-uid"), got.UID)
				assert.Empty(t, got.Annotations[InstalledByAnnotation])
			}
		})
	}
}

func TestApplyTrackedGuardedCarriesPlanIdentityToWriteBoundary(t *testing.T) {
	credentialObject := func() *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("v1")
		obj.SetKind("Secret")
		obj.SetNamespace("demo-ns")
		obj.SetName("demo-channel-creds")
		return obj
	}

	t.Run("plan-time absence cannot become an adopted create race", func(t *testing.T) {
		ctx := context.Background()
		base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		observed, err := ObserveObject(ctx, base, credentialObject())
		require.NoError(t, err)
		require.False(t, observed.Exists)
		competitor := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "demo-channel-creds", Namespace: "demo-ns", UID: "competitor-uid",
			Annotations: map[string]string{InstalledByAnnotation: "ap"},
		}, Data: map[string][]byte{"token": []byte("competitor")}}
		require.NoError(t, base.Create(ctx, competitor))
		out := wizardOutput()
		out.ChannelManifest = nil
		out.CapabilityPatch = nil

		receipt, err := ApplyTrackedGuarded(ctx, base, "ap", out, []ObjectGuard{observed.Guard})
		require.ErrorContains(t, err, "changed existence after graph preflight")
		require.NoError(t, receipt.Rollback(ctx, base))
		var got corev1.Secret
		require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, &got))
		assert.Equal(t, []byte("competitor"), got.Data["token"])
	})

	t.Run("same UID changed resourceVersion is refused", func(t *testing.T) {
		ctx := context.Background()
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "demo-channel-creds", Namespace: "demo-ns", UID: "existing-uid", ResourceVersion: "1",
			Annotations: map[string]string{InstalledByAnnotation: "ap"},
		}, Data: map[string][]byte{"token": []byte("original")}}
		base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
		observed, err := ObserveObject(ctx, base, credentialObject())
		require.NoError(t, err)
		require.True(t, observed.Exists)
		var changed corev1.Secret
		require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, &changed))
		changed.Data["token"] = []byte("changed")
		require.NoError(t, base.Update(ctx, &changed))
		out := wizardOutput()
		out.ChannelManifest = nil
		out.CapabilityPatch = nil

		receipt, err := ApplyTrackedGuarded(ctx, base, "ap", out, []ObjectGuard{observed.Guard})
		require.ErrorContains(t, err, "changed after graph preflight")
		require.NoError(t, receipt.Rollback(ctx, base))
		require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, &changed))
		assert.Equal(t, []byte("changed"), changed.Data["token"])
	})
}

func TestApplyTrackedRollbackPreservesPreexistingAndChangedObjects(t *testing.T) {
	ctx := context.Background()
	existing := &spiceboxv1alpha1.Channel{
		TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns", UID: "existing-uid", ResourceVersion: "1"},
	}
	c := &trackedApplyClient{Client: ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build(), t: t}
	out := wizardOutput()
	out.SecretManifest = nil
	out.CapabilityPatch = nil
	out.ReplaceExisting = true

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.NoError(t, err)
	var changed spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &changed))
	changed.Labels = map[string]string{"changed": "elsewhere"}
	require.NoError(t, c.Update(ctx, &changed))
	require.NoError(t, receipt.Rollback(ctx, c))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &changed))
	assert.Equal(t, "elsewhere", changed.Labels["changed"])
}

func TestApplyTrackedSuccessfulCreatesRemainRollbackableUntilGraphFinalizes(t *testing.T) {
	ctx := context.Background()
	c := &trackedApplyClient{Client: ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build(), t: t}
	out := wizardOutput()
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.NoError(t, err)
	require.NoError(t, receipt.Rollback(ctx, c))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel-creds"}, out.SecretManifest.DeepCopy())))
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, out.ChannelManifest.DeepCopy())))
}

func TestApplyTrackedSuccessfulPatchDoesNotAdoptLaterSameUIDResourceVersion(t *testing.T) {
	ctx := context.Background()
	base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := &trackedApplyClient{Client: base, t: t}
	c.patchFn = func(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if err := base.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		externallyUpdateTrackedChannel(t, ctx, base)
		return nil
	}
	out := wizardOutput()
	out.SecretManifest = nil
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.NoError(t, err)
	require.Error(t, receipt.Rollback(ctx, c), "rollback must retain the Patch response identity, not adopt a later same-UID update")
	var got spiceboxv1alpha1.Channel
	require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &got))
	assert.Equal(t, "externally", got.Labels["updated"])
}

func TestApplyTrackedPatchErrorDoesNotAdoptLaterSameUIDResourceVersion(t *testing.T) {
	ctx := context.Background()
	base := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	c := &trackedApplyClient{Client: base, t: t}
	c.patchFn = func(ctx context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
		externallyUpdateTrackedChannel(t, ctx, base)
		return errors.New("injected ambiguous patch failure")
	}
	out := wizardOutput()
	out.SecretManifest = nil
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.ErrorContains(t, err, "injected ambiguous patch failure")
	require.Error(t, receipt.Rollback(ctx, c), "a failed Patch keeps the Create receipt identity and must not adopt later state")
	var got spiceboxv1alpha1.Channel
	require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &got))
	assert.Equal(t, "externally", got.Labels["updated"])
}

func TestApplyTrackedRollbackPreservesInvocationObjectChangedAfterApply(t *testing.T) {
	ctx := context.Background()
	c := &trackedApplyClient{Client: ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build(), t: t}
	out := wizardOutput()
	out.SecretManifest = nil
	out.CapabilityPatch = nil

	receipt, err := ApplyTracked(ctx, c, "ap", out)
	require.NoError(t, err)
	var changed spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &changed))
	changed.Labels = map[string]string{"changed": "elsewhere"}
	require.NoError(t, c.Update(ctx, &changed))
	require.Error(t, receipt.Rollback(ctx, c), "resourceVersion precondition must refuse deletion after an external change")
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &changed))
	assert.Equal(t, "elsewhere", changed.Labels["changed"])
}
