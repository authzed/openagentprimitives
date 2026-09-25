package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// contractInput is the WizardInput every kind is asked with: a namespace, and
// a cluster holding one AgentClass.
//
// The AgentClass is not decoration. Three of the six registered kinds list the
// namespace's classes to build their binding enum and REFUSE a namespace with
// none — so a nil client makes every one of them error, and a sweep run that
// way would inspect nothing while looking green. The counters below exist to
// catch that; this fixture is what keeps them from having to.
func contractInput(t *testing.T) channelkinds.WizardInput {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return channelkinds.WizardInput{
		Namespace: "demo-ns",
		K8s: ctrlfake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(&spiceboxv1alpha1.AgentClass{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "demo-ns"},
			}).
			Build(),
	}
}

// TestEveryKindsWizardSatisfiesTheContract pins that each registered kind's
// Wizard can be asked what it needs without panicking, and that what it
// answers is RENDERABLE — every question passing channelkinds.ValidateInputs,
// which is the check standing between a kind and a client that would otherwise
// panic on it or silently drop it.
//
// Both halves of the wizard's question set are checked: Inputs, and the
// FallbackInputs of a non-nil Handoff, which a client renders through exactly
// the same path (see channelkinds.HandoffSpec.FallbackInputs).
//
// Returning NO questions is a legitimate answer, not a failure: a kind whose
// manifests are fully determined by its WizardInput has nothing to ask (see
// channelkinds.Wizard.Inputs), and the fake test kind is exactly that. So
// is refusing outright, for a kind whose Channels are built elsewhere —
// channelkinds.UnavailableWizard. That is why the counter below exists: a tree
// in which EVERY kind refused would otherwise leave this test green with
// nothing checked.
func TestEveryKindsWizardSatisfiesTheContract(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by init")

	answered := 0

	for _, k := range kinds {
		t.Run(k.Name()+": Inputs and any handoff fallback are renderable, or the kind refuses", func(t *testing.T) {
			w := k.Wizard()
			require.NotNil(t, w, "a kind must return a non-nil wizard; see channelkinds.UnavailableWizard")

			in := contractInput(t)
			qs, err := w.Inputs(context.Background(), in)
			if err != nil {
				assert.Nil(t, qs, "a refusing Inputs must declare no questions")
				return
			}
			answered++
			assert.NoError(t, channelkinds.ValidateInputs(qs),
				"a kind must not declare an input no client could render")

			spec, err := w.Handoff(context.Background(), in)
			if err != nil {
				return
			}
			if spec == nil {
				return
			}
			assert.NoError(t, channelkinds.ValidateInputs(spec.FallbackInputs),
				"a handoff's fallback questions are rendered exactly like Inputs, so they answer to the same validator")
			assert.NotNil(t, spec.Begin, "a non-nil HandoffSpec must say where to send the operator")
			assert.NotNil(t, spec.Complete, "a non-nil HandoffSpec must be able to exchange its callback")
		})
	}

	assert.Positive(t, answered,
		"no registered kind answered Inputs; every wizard refusing is a regression, "+
			"not a tree in which there is nothing to check")
}

// TestEveryKindsWizardNamesItsQuestionsDistinctly pins the one fact a client
// building an --answer / form-field set depends on: a question's Name is how
// an answer is addressed, so two questions sharing one would make an answer
// ambiguous.
//
// channelkinds.ValidateInputs already refuses a duplicate WITHIN Inputs; what
// this adds is the join across Inputs and a handoff's FallbackInputs, which
// no single ValidateInputs call sees — the client concatenates them
// (wizardrun.AllInputs) and a collision there is a question that
// silently loses to its twin.
func TestEveryKindsWizardNamesItsQuestionsDistinctly(t *testing.T) {
	inspected := 0

	for _, k := range registry.All() {
		w := k.Wizard()
		if w == nil {
			continue
		}
		in := contractInput(t)
		qs, err := w.Inputs(context.Background(), in)
		if err != nil {
			continue
		}
		spec, err := w.Handoff(context.Background(), in)
		if err != nil {
			continue
		}
		if spec != nil {
			qs = append(append([]oap.Question(nil), qs...), spec.FallbackInputs...)
		}
		if len(qs) == 0 {
			continue
		}
		inspected++
		t.Run(k.Name()+": every question name is unique across Inputs and the handoff fallback", func(t *testing.T) {
			seen := map[string]bool{}
			for i, q := range qs {
				assert.NotEmpty(t, q.Name, "question %d has no name", i)
				assert.False(t, seen[q.Name],
					"question name %q is used twice; an answer is addressed by name", q.Name)
				seen[q.Name] = true
			}
		})
	}

	assert.Positive(t, inspected, "no registered kind declared a question to inspect")
}
