package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
)

// kindWizard is this kind's wizard, reached the way `oap channel create`
// reaches it — through the registered Kind rather than by constructing the
// concrete type — so a Kind.Wizard that stopped returning it is caught here.
func kindWizard(t *testing.T) channelkinds.Wizard {
	t.Helper()
	w := Kind{}.Wizard()
	require.NotNil(t, w, "the fake kind must return a wizard")
	return w
}

// TestWizardInputs_AsksNothing pins that the fake kind declares no questions
// at all — and that this is a legitimate answer to the contract rather than
// an omission.
//
// Every value in the fake kind's manifests is fixed; the one screen it
// described under the old contract presented no group (fixtureScreen.Prepare
// returns a nil *huh.Group). Declaring a question here to satisfy an
// "at least one input" rule would ADD a prompt an operator's answer cannot
// change, which is precisely the user-visible change this migration promises
// not to make.
//
// The empty want is passed through kindtest.AssertPromptShapes rather than
// asserted inline so this kind establishes the shape Tasks 5–7 fill in:
// every migrated kind pins its questions' PROMPT TEXT, which the CLI's
// pinned-prompts gate cannot see (it records hand-maintained AnswerKeys, not
// what the widgets say).
func TestWizardInputs_AsksNothing(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{Namespace: "default"})
	require.NoError(t, err, "the fake kind's inputs must resolve without a cluster")
	kindtest.AssertPromptShapes(t, qs, nil)
	assert.Empty(t, qs, "the fake kind asks nothing")
}

// TestWizardHandoff_IsNil: no browser round-trip, so nil — the common case
// three of the four kinds are in.
func TestWizardHandoff_IsNil(t *testing.T) {
	spec, err := kindWizard(t).Handoff(context.Background(), channelkinds.WizardInput{Namespace: "default"})
	require.NoError(t, err)
	assert.Nil(t, spec, "the fake kind sends the operator nowhere")
}

// TestWizardResolve_DerivesNothing: the fake kind holds no credential to
// verify and no external service to ask, so its Resolve is the nil/nil case
// channelkinds.Wizard.Resolve calls the common one.
func TestWizardResolve_DerivesNothing(t *testing.T) {
	derived, err := kindWizard(t).Resolve(context.Background(), channelkinds.WizardInput{Namespace: "default"}, nil)
	require.NoError(t, err)
	assert.Empty(t, derived, "the fake kind derives no further answers")
}

// TestWizardResult_IsPureOverItsArguments: two calls with the same arguments
// must produce the same output, and neither may depend on a prior Inputs call
// having run on the same value. The admin UI calls Result server-side, where
// the two calls can land on different instances.
//
// The second call is made on a FRESH wizard that has never had Inputs called
// on it, which is what makes this a test of purity rather than of
// determinism: a Result reading a namespace stashed by Inputs would return an
// error (or empty manifests) on that second value, not an equal one.
func TestWizardResult_IsPureOverItsArguments(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "demo-ns"}
	answers := map[string]string{"unread-key": "unread-value"}

	primed := kindWizard(t)
	_, err := primed.Inputs(context.Background(), in)
	require.NoError(t, err)
	first, err := primed.Result(in, answers)
	require.NoError(t, err)

	second, err := kindWizard(t).Result(in, answers)
	require.NoError(t, err)

	assert.Equal(t, first, second,
		"Result must be a pure function of (in, answers) — the admin UI calls it server-side, on a value Inputs never touched")
	require.NotNil(t, first.ChannelManifest)
	assert.Equal(t, "demo-ns", first.ChannelManifest.Namespace,
		"the namespace must come from in, not from a field Inputs set")
}

// TestWizardResult_RefusesWithoutANamespace covers the one error path the
// fake kind has: every manifest it builds is namespaced, so an empty
// WizardInput.Namespace must fail rather than produce cluster-scoped
// nonsense.
func TestWizardResult_RefusesWithoutANamespace(t *testing.T) {
	out, err := kindWizard(t).Result(channelkinds.WizardInput{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
	assert.Nil(t, out.ChannelManifest, "a refused Result produces no manifests")
	assert.Nil(t, out.SecretManifest, "a refused Result produces no manifests")
}

// TestWizardResult_IsTheWholeFixture pins what a `--kind fake` run actually
// produces, field by field.
//
// It is the assertion this kind needs most: with no questions to pin, the
// prompt-shape gate above is vacuous here, so the manifests are the only thing
// left that can regress. Every value below is FIXED — none comes from an
// answer — which is precisely why it can be written out in full.
func TestWizardResult_IsTheWholeFixture(t *testing.T) {
	const ns = "demo-ns"

	out, err := kindWizard(t).Result(channelkinds.WizardInput{Namespace: ns}, nil)
	require.NoError(t, err)

	require.NotNil(t, out.ChannelManifest)
	ch := out.ChannelManifest
	assert.Equal(t, "fake-channel", ch.Name)
	assert.Equal(t, ns, ch.Namespace)
	assert.Equal(t, "agentprimitives.authzed.com/v1alpha1", ch.APIVersion)
	assert.Equal(t, "Channel", ch.Kind)
	assert.Equal(t, "fake", ch.Spec.Kind)
	assert.Equal(t, "default-agent", ch.Spec.AgentClass)
	assert.Equal(t, "user", ch.Spec.SessionScope)
	assert.Equal(t, "fake-creds", ch.Spec.CredentialsRef.SecretName,
		"the Channel must point at the Secret this same Result emits")
	assert.NotNil(t, ch.Spec.Fake, "a fake Channel needs its own config block or the controller rejects it")

	require.NotNil(t, out.SecretManifest)
	assert.Equal(t, "fake-creds", out.SecretManifest.Name)
	assert.Equal(t, ns, out.SecretManifest.Namespace)
	assert.Equal(t, map[string][]byte{"placeholder": []byte("ok")}, out.SecretManifest.Data)

	assert.Equal(t, []string{"fake kind: this is a test fixture; do not use in production."}, out.Notes)
	assert.Equal(t, []channelkinds.SummaryNote{{Label: fixtureNoteLabel, Value: fixtureNoteValue}}, out.Summary,
		"this kind runs no code while the questions are answered, so its one summary line must arrive as data")
}
