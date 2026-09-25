package materialize

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newReader(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func controllerOwnerRef(kind, name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v1.SchemeGroupVersion.String(),
		Kind:       kind,
		Name:       name,
		UID:        "uid-src",
		Controller: ptr.To(true),
	}
}

func TestSkill(t *testing.T) {
	src := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns"},
		Spec:       v1.SkillSourceSpec{RepoURL: "https://github.com/trusted-org/skills"},
	}
	otherOrgSrc := &v1.SkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "other-src", Namespace: "ns"},
		Spec:       v1.SkillSourceSpec{RepoURL: "https://github.com/other-org/repo"},
	}

	cases := []struct {
		name          string
		objs          []client.Object
		namespace     string
		ownerRefs     []metav1.OwnerReference
		canonicalName string
		want          bool
	}{
		{
			name:          "THE BYPASS: fabricated Source but no owner-ref: not materialized",
			objs:          []client.Object{src},
			namespace:     "ns",
			ownerRefs:     nil,
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "owner-ref to a non-existent SkillSource: not materialized",
			objs:          nil,
			namespace:     "ns",
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("SkillSource", "does-not-exist")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "owner-ref to a real SkillSource with a DIFFERENT authority: not materialized",
			objs:          []client.Object{otherOrgSrc},
			namespace:     "ns",
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("SkillSource", "other-src")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "LEGIT: owner-ref to a real SkillSource whose authority matches: materialized",
			objs:          []client.Object{src},
			namespace:     "ns",
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("SkillSource", "src")},
			canonicalName: "github.com/trusted-org/skills//skills/foo@v1",
			want:          true,
		},
		{
			name:          "local// name: not materialized (hand-authored path, no owner needed)",
			objs:          nil,
			namespace:     "ns",
			ownerRefs:     nil,
			canonicalName: "local//foo",
			want:          false,
		},
		{
			name:          "owner-ref present but wrong Kind (not SkillSource): not materialized",
			objs:          []client.Object{src},
			namespace:     "ns",
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("SomeOtherKind", "src")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:      "owner-ref present but Controller=false (non-controller ref): not materialized",
			objs:      []client.Object{src},
			namespace: "ns",
			ownerRefs: []metav1.OwnerReference{{
				APIVersion: v1.SchemeGroupVersion.String(), Kind: "SkillSource", Name: "src",
				UID: "uid-src", Controller: ptr.To(false),
			}},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "cross-namespace owner-ref cannot be satisfied: not materialized",
			objs:          []client.Object{src}, // src lives in "ns"
			namespace:     "other-ns",           // Skill claims to live elsewhere
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("SkillSource", "src")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader(t, tc.objs...)
			got, err := Skill(context.Background(), r, tc.namespace, tc.ownerRefs, tc.canonicalName)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSkill_GetErrorOtherThanNotFoundIsReturned(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	wantErr := errors.New("transient api failure")
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return wantErr
		},
	}).Build()

	ownerRefs := []metav1.OwnerReference{controllerOwnerRef("SkillSource", "src")}
	got, err := Skill(context.Background(), c, "ns", ownerRefs, "github.com/trusted-org/skills//evil")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.False(t, got, "an inconclusive lookup must not report materialized=true")
}

func TestClusterSkill(t *testing.T) {
	src := &v1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "src"},
		Spec:       v1.ClusterSkillSourceSpec{RepoURL: "https://github.com/trusted-org/skills"},
	}
	otherOrgSrc := &v1.ClusterSkillSource{
		ObjectMeta: metav1.ObjectMeta{Name: "other-src"},
		Spec:       v1.ClusterSkillSourceSpec{RepoURL: "https://github.com/other-org/repo"},
	}

	cases := []struct {
		name          string
		objs          []client.Object
		ownerRefs     []metav1.OwnerReference
		canonicalName string
		want          bool
	}{
		{
			name:          "THE BYPASS: fabricated Source but no owner-ref: not materialized",
			objs:          []client.Object{src},
			ownerRefs:     nil,
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "owner-ref to a non-existent ClusterSkillSource: not materialized",
			objs:          nil,
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("ClusterSkillSource", "does-not-exist")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "owner-ref to a real ClusterSkillSource with a DIFFERENT authority: not materialized",
			objs:          []client.Object{otherOrgSrc},
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("ClusterSkillSource", "other-src")},
			canonicalName: "github.com/trusted-org/skills//evil",
			want:          false,
		},
		{
			name:          "LEGIT: owner-ref to a real ClusterSkillSource whose authority matches: materialized",
			objs:          []client.Object{src},
			ownerRefs:     []metav1.OwnerReference{controllerOwnerRef("ClusterSkillSource", "src")},
			canonicalName: "github.com/trusted-org/skills//skills/foo@v1",
			want:          true,
		},
		{
			name:          "local// name: not materialized",
			objs:          nil,
			ownerRefs:     nil,
			canonicalName: "local//foo",
			want:          false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReader(t, tc.objs...)
			got, err := ClusterSkill(context.Background(), r, tc.ownerRefs, tc.canonicalName)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClusterSkill_GetErrorOtherThanNotFoundIsReturned(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(s))
	wantErr := errors.New("transient api failure")
	c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return wantErr
		},
	}).Build()

	ownerRefs := []metav1.OwnerReference{controllerOwnerRef("ClusterSkillSource", "src")}
	got, err := ClusterSkill(context.Background(), c, ownerRefs, "github.com/trusted-org/skills//evil")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.False(t, got, "an inconclusive lookup must not report materialized=true")
}

func TestUnparseableCanonicalName(t *testing.T) {
	// A parse error is surfaced elsewhere (validate.Skill / CheckProvenance's
	// own canonical.Parse call); this package just reports "not materialized"
	// without erroring so callers don't double-report the same problem.
	r := newReader(t)
	got, err := Skill(context.Background(), r, "ns", nil, "not-a-canonical-name")
	require.NoError(t, err)
	assert.False(t, got)
}
