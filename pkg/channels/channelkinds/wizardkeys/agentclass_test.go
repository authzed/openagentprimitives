package wizardkeys

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// fakeCluster is a client holding exactly the named AgentClasses in "default".
func fakeCluster(t *testing.T, names ...string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	objs := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "default"},
		})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
}

// TestListAgentClasses_ListsMetadataOnly pins that name listing requests
// METADATA ONLY, never the typed spec. The reason is a real outage: AgentClass's
// spec.skills changed shape from []string to []AgentSkill (commit f04d2431e), and
// a class written under the old schema and never re-applied still has string
// items in etcd — the apiserver validates on write, not read, so it serves them
// back verbatim. A typed List decodes every class's whole spec and fails outright
// on that one object, which took channel wiring for an UNRELATED agent down with
// a "cannot unmarshal string into ... AgentSkill" error. A PartialObjectMetadata
// request never transmits or decodes a spec, so name listing cannot be broken by
// spec drift elsewhere. This asserts on the request type rather than planting the
// stale object because the fake client eagerly decodes stored objects into the
// typed struct at build time and cannot hold spec bytes the struct rejects — the
// same decode that fails in production. A refactor back to a typed List reopens
// the outage and fails here.
func TestListAgentClasses_ListsMetadataOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	var sawMetadataList bool
	c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			meta, ok := list.(*metav1.PartialObjectMetadataList)
			require.Truef(t, ok, "listAgentClasses must request metadata only, got %T", list)
			sawMetadataList = true
			meta.Items = []metav1.PartialObjectMetadata{
				{ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "legacy-agent", Namespace: "default"}},
			}
			return nil
		},
	})

	got, err := listAgentClasses(context.Background(), c, "default")
	require.NoError(t, err)
	assert.True(t, sawMetadataList, "the List must have been served through the metadata path")
	assert.ElementsMatch(t, []string{"demo-agent", "legacy-agent"}, got)
}

// TestAgentClassQuestion is the one implementation three kinds now share, so
// every rule that used to be restated per kind is asserted once here.
//
// The rows are the situations a real run lands in, and each pins BOTH halves
// of the answer: the question a client would render, and the (class,
// unambiguous) pair the kind derives its other defaults from. Getting the
// second wrong is the silent failure — a wrong default is accepted by a blank
// answer, so a Channel ends up named after an AgentClass nobody picked.
func TestAgentClassQuestion(t *testing.T) {
	const prompt = "Bind this demo Channel to AgentClass"

	cases := []struct {
		name            string
		in              channelkinds.WizardInput
		errSubstr       []string
		wantEnum        []string
		wantChosen      string
		wantUnambiguous bool
	}{
		{
			name:            "one AgentClass, nothing seeded: it is the answer, so dependent defaults may derive from it",
			in:              channelkinds.WizardInput{Namespace: "default", K8s: fakeCluster(t, "demo-agent")},
			wantEnum:        []string{"demo-agent"},
			wantChosen:      "demo-agent",
			wantUnambiguous: true,
		},
		{
			name:            "two AgentClasses, nothing seeded: the first is pre-selected but is a GUESS, so nothing derives from it",
			in:              channelkinds.WizardInput{Namespace: "default", K8s: fakeCluster(t, "demo-agent", "other-agent")},
			wantEnum:        []string{"demo-agent", "other-agent"},
			wantChosen:      "demo-agent",
			wantUnambiguous: false,
		},
		{
			name: "a seeded AgentClass that exists: verified against the listing and treated as the operator's own choice",
			in: channelkinds.WizardInput{
				Namespace: "default",
				K8s:       fakeCluster(t, "demo-agent", "other-agent"),
				Seeded:    map[string]string{KeyAgentClass: "other-agent"},
			},
			wantEnum:        []string{"demo-agent", "other-agent"},
			wantChosen:      "other-agent",
			wantUnambiguous: true,
		},
		{
			name: "a seeded AgentClass that does not exist: REFUSED rather than bound to nothing",
			in: channelkinds.WizardInput{
				Namespace: "default",
				K8s:       fakeCluster(t, "demo-agent"),
				Seeded:    map[string]string{KeyAgentClass: "does-not-exist"},
			},
			errSubstr: []string{"does-not-exist", "cannot bind to an agent that does not exist"},
		},
		{
			name:      "an empty namespace with nothing seeded: refused, naming what to create first",
			in:        channelkinds.WizardInput{Namespace: "default", K8s: fakeCluster(t)},
			errSubstr: []string{"no AgentClasses found", "create one before creating a Channel"},
		},
		{
			name: "an offline dry-run (nil client) with a seeded value: taken on trust and stood in as the sole Enum option",
			in: channelkinds.WizardInput{
				Namespace: "default",
				Seeded:    map[string]string{KeyAgentClass: "seeded-agent"},
			},
			wantEnum:        []string{"seeded-agent"},
			wantChosen:      "seeded-agent",
			wantUnambiguous: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, chosen, unambiguous, err := AgentClassQuestion(context.Background(), tc.in, prompt)
			if len(tc.errSubstr) > 0 {
				require.Error(t, err)
				for _, want := range tc.errSubstr {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)

			assert.Equal(t, KeyAgentClass, q.Name, "the key is the shared one, which is what --answer addresses")
			assert.Equal(t, oap.QEnum, q.Type)
			assert.Equal(t, prompt, q.Prompt, "the caller's prompt is the one thing a kind supplies")
			assert.Equal(t, tc.wantEnum, q.Enum)
			assert.Equal(t, tc.wantChosen, q.Default,
				"an enum's widget pre-selects its first option regardless of Default, so Default is always set")
			assert.Equal(t, tc.wantChosen, chosen)
			assert.Equal(t, tc.wantUnambiguous, unambiguous,
				"a class the operator named, or the only one that exists, is a FACT; the first of several is a guess")
			require.NoError(t, channelkinds.ValidateInputs([]oap.Question{q}),
				"the question must be renderable by any client, including an offline one")
		})
	}
}
