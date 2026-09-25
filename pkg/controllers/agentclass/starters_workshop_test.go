package agentclass

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

type recordingStarterLinker struct{ subjects []string }

func (f *recordingStarterLinker) EnsureAgentClassStarters(_ context.Context, _, _ string, subjects []string) error {
	f.subjects = append([]string(nil), subjects...)
	return nil
}

func workshopNamespace(name, sessNS, sessName string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		spiceboxv1alpha1.LabelWorkshopSessionNamespace: sessNS,
		spiceboxv1alpha1.LabelWorkshopSessionName:      sessName,
	}}}
}

// A class authored inside a workshop namespace is startable by the workshop's
// owner — the person who is building it — without any spec field naming
// them, so an installed copy never inherits it. A class anywhere else is
// exactly as before.
func TestEnsureStarterLinks_UnionsTheWorkshopOwner(t *testing.T) {
	const wsNS, builderNS, builderSess = "ws-abc123def456", "builder-ns", "agent-builder-1"
	cases := []struct {
		name    string
		objs    []client.Object
		classNS string
		allowed []string
		want    []string
	}{
		{
			name: "workshop class, no allowedStarters: the owner alone",
			objs: []client.Object{
				workshopNamespace(wsNS, builderNS, builderSess),
				&spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{Namespace: builderNS, Name: spiceboxv1alpha1.WorkshopName(builderSess)},
					Spec: spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "c4nonical1", SidecarToolbox: "workshop"}},
			},
			classNS: wsNS,
			want:    []string{"user:c4nonical1"},
		},
		{
			name: "workshop class with allowedStarters: the union, owner last",
			objs: []client.Object{
				workshopNamespace(wsNS, builderNS, builderSess),
				&spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{Namespace: builderNS, Name: spiceboxv1alpha1.WorkshopName(builderSess)},
					Spec: spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "c4nonical1", SidecarToolbox: "workshop"}},
			},
			classNS: wsNS,
			allowed: []string{"user:0ther1"},
			want:    []string{"user:0ther1", "user:c4nonical1"},
		},
		{
			name: "workshop class, owner already declared in allowedStarters: linked exactly once",
			objs: []client.Object{
				workshopNamespace(wsNS, builderNS, builderSess),
				&spiceboxv1alpha1.Workshop{ObjectMeta: metav1.ObjectMeta{Namespace: builderNS, Name: spiceboxv1alpha1.WorkshopName(builderSess)},
					Spec: spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "c4nonical1", SidecarToolbox: "workshop"}},
			},
			classNS: wsNS,
			allowed: []string{"user:c4nonical1"},
			want:    []string{"user:c4nonical1"},
		},
		{
			name:    "ordinary namespace: allowedStarters alone",
			objs:    []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}},
			classNS: "plain",
			allowed: []string{"user:0ther1"},
			want:    []string{"user:0ther1"},
		},
		{
			name:    "workshop labels but the Workshop is gone (teardown race): allowedStarters alone, no error",
			objs:    []client.Object{workshopNamespace(wsNS, builderNS, builderSess)},
			classNS: wsNS,
			allowed: []string{"user:0ther1"},
			want:    []string{"user:0ther1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: tc.classNS, Name: "demo-agent"}}
			if len(tc.allowed) > 0 {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{AllowedStarters: tc.allowed}}
			}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(append(tc.objs, ac)...).Build()
			linker := &recordingStarterLinker{}
			r := &Reconciler{Client: c, StarterLinker: linker}
			require.NoError(t, r.ensureStarterLinks(context.Background(), ac))
			assert.Equal(t, tc.want, linker.subjects)
		})
	}
}
