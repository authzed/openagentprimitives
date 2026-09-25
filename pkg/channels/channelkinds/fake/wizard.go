// Setup flow for `oap channel create --kind fake`. The fake kind exists to give
// tests a Channel to point at, so every value in its manifests is fixed and
// the flow asks nothing.
package fake

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// The one line this kind puts on the run summary. Named constants rather than
// literals so the test pinning the summary asserts against the same strings
// Result returns instead of a transcription of them.
const (
	fixtureNoteLabel = "Channel"
	fixtureNoteValue = "fake-channel (test fixture)"
)

// fakeWizard holds nothing: every method takes the WizardInput it needs as an
// argument, which is what lets Result be called on a value no Inputs call ever
// touched — see channelkinds.Wizard.Result.
type fakeWizard struct{}

// Inputs asks nothing: every value in the fixture is fixed, so there is no
// answer an operator could give that would change the manifests. See
// channelkinds.Wizard.Inputs for why that is a legitimate answer rather
// than an omission.
func (w *fakeWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return nil, nil
}

// Handoff sends the operator nowhere.
func (w *fakeWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

// Resolve derives nothing: every value in the fixture is fixed, so there is no
// external service to ask about an answer.
func (w *fakeWizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}

// Result builds the fixture from in alone; this kind has no answers to read.
func (w *fakeWizard) Result(in channelkinds.WizardInput, _ map[string]string) (channelkinds.WizardOutput, error) {
	out, err := fixtureOutput(in.Namespace)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	out.Summary = []channelkinds.SummaryNote{{Label: fixtureNoteLabel, Value: fixtureNoteValue}}
	return out, nil
}

// fixtureOutput is the Secret + Channel this kind always produces, refusing a
// run with no namespace to put them in: every manifest here is namespaced, so
// an empty one would be cluster-scoped nonsense rather than a usable fixture.
func fixtureOutput(ns string) (channelkinds.WizardOutput, error) {
	if ns == "" {
		return channelkinds.WizardOutput{}, errors.New("namespace required")
	}
	return channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "fake-creds", Namespace: ns},
			Data:       map[string][]byte{"placeholder": []byte("ok")},
		},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "agentprimitives.authzed.com/v1alpha1",
				Kind:       "Channel",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "fake-channel", Namespace: ns},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "fake", AgentClass: "default-agent", SessionScope: "user",
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "fake-creds"},
				Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
			},
		},
		Notes: []string{"fake kind: this is a test fixture; do not use in production."},
	}, nil
}
