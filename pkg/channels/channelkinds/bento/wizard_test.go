package bento

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

func newFakeK8s(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func newAgentClass(name, namespace string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// kindWizard is this kind's wizard, reached the way `oap channel create`
// reaches it — through the registered Kind rather than by constructing the
// concrete type — so a Kind.Wizard that stopped returning it is caught here.
func kindWizard(t *testing.T) channelkinds.Wizard {
	t.Helper()
	w := Kind{}.Wizard()
	require.NotNil(t, w, "the bento kind must return a wizard")
	return w
}

// acceptedDefaults is the answer map a run in which the operator accepted
// every offered default produces: each question's own Default, read back as
// the string a client records.
//
// It exists so a test of the MANIFESTS is driven by the real defaults rather
// than by a transcription of them — a default that silently changed would
// otherwise leave every manifest assertion below green.
func acceptedDefaults(t *testing.T, qs []oap.Question) map[string]string {
	t.Helper()
	answers := make(map[string]string, len(qs))
	for _, q := range qs {
		s, ok := q.Default.(string)
		require.Truef(t, ok, "question %q offers no string default for a bare accept to take", q.Name)
		answers[q.Name] = s
	}
	return answers
}

// TestWizard_BuildsBentoChannelCR is the happy path with every default
// accepted: ask what is needed, take each question's own Default as the
// answer, and check the manifests that come back.
func TestWizard_BuildsBentoChannelCR(t *testing.T) {
	cli := newFakeK8s(newAgentClass("crm-companies", "default")).Build()
	in := channelkinds.WizardInput{K8s: cli, Namespace: "default"}

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err, "Inputs")

	out, err := kindWizard(t).Result(in, acceptedDefaults(t, qs))
	require.NoError(t, err, "Result")

	require.NotNil(t, out.ChannelManifest, "ChannelManifest is nil")
	ch := out.ChannelManifest

	assert.Equal(t, "bento", ch.Spec.Kind, "Kind")
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleInput, ch.Spec.Role, "Role")
	assert.Equal(t, "crm-companies", ch.Spec.AgentClass, "AgentClass")
	assert.Equal(t, "service:crm-companies-bot", ch.Spec.AuthzSubject, "AuthzSubject")

	require.NotNil(t, ch.Spec.Bento, "Bento not populated")
	require.NotNil(t, ch.Spec.Bento.Generate, "Bento.Generate not populated")
	assert.Contains(t, ch.Spec.Bento.Generate.Mapping, "root = ",
		"Mapping doesn't use canonical root = pattern")
	assert.Equal(t, "@every 168h", ch.Spec.Bento.Generate.Interval, "Interval")

	// Channel name should default to <agentclass>-trigger.
	assert.Equal(t, "crm-companies-trigger", ch.Name, "Channel name")
	// CredentialsRef must name <channel>-creds.
	assert.Equal(t, ch.Name+"-creds", ch.Spec.CredentialsRef.SecretName,
		"CredentialsRef.SecretName")
	assert.Equal(t, "default", ch.Namespace, "Channel namespace")

	// Secret manifest must be present and empty (bento needs no keys).
	require.NotNil(t, out.SecretManifest, "SecretManifest is nil")
	assert.Equal(t, ch.Spec.CredentialsRef.SecretName, out.SecretManifest.Name,
		"SecretManifest.Name")
	assert.Empty(t, out.SecretManifest.Data, "bento needs no credential keys")

	// Notes must contain the three guidance points.
	assert.GreaterOrEqual(t, len(out.Notes), 3, "expected at least 3 notes")
	joined := strings.Join(out.Notes, "\n")
	assert.Contains(t, joined, "sessionInteractPermission", "notes missing sessionInteractPermission guidance")
	assert.Contains(t, joined, "credentials Secret", "notes missing credentials Secret guidance")
	assert.Contains(t, joined, "SlackOutputDefaults", "notes missing SlackOutputDefaults pairing guidance")

	// The five lines the run summary shows. Stated by Result because this kind
	// runs no code of its own while the questions are being answered — see
	// channelkinds.WizardOutput.Summary.
	assert.Equal(t, []channelkinds.SummaryNote{
		{Label: "AgentClass", Value: "crm-companies"},
		{Label: "Name", Value: "crm-companies-trigger"},
		{Label: "Subject", Value: "service:crm-companies-bot"},
		{Label: "Schedule", Value: defaultInterval},
		{Label: "Mapping", Value: defaultMapping},
	}, out.Summary)
}

// TestWizard_CustomValues: every answer typed rather than accepted, so a
// Result that read a default instead of the answer would be caught.
func TestWizard_CustomValues(t *testing.T) {
	cli := newFakeK8s(newAgentClass("crm-companies", "default")).Build()
	in := channelkinds.WizardInput{K8s: cli, Namespace: "default"}

	out, err := kindWizard(t).Result(in, map[string]string{
		keyAgentClass:   "crm-companies",
		keyChannelName:  "my-bento-trigger",
		keyAuthzSubject: "service:my-custom-bot",
		keyInterval:     "0 9 * * MON",
		keyMapping:      `root = "Monday morning run"`,
	})
	require.NoError(t, err, "Result")

	ch := out.ChannelManifest
	require.NotNil(t, ch, "ChannelManifest is nil")
	assert.Equal(t, "my-bento-trigger", ch.Name, "Name")
	assert.Equal(t, "service:my-custom-bot", ch.Spec.AuthzSubject, "AuthzSubject")
	assert.Equal(t, "0 9 * * MON", ch.Spec.Bento.Generate.Interval, "Interval")
	assert.Equal(t, `root = "Monday morning run"`, ch.Spec.Bento.Generate.Mapping, "Mapping")
	assert.Equal(t, "my-bento-trigger-creds", ch.Spec.CredentialsRef.SecretName,
		"the Secret follows the custom Channel name, not the default")
}

// TestWizard_BindsTheAgentClassActuallyChosen pins that the AgentClass answer
// is READ rather than always resolving to the first option — the failure a
// wizard that ignored the answer would still pass every other test with.
func TestWizard_BindsTheAgentClassActuallyChosen(t *testing.T) {
	cli := newFakeK8s(
		newAgentClass("alpha-agent", "default"),
		newAgentClass("zeta-agent", "default"),
	).Build()
	in := channelkinds.WizardInput{K8s: cli, Namespace: "default"}

	out, err := kindWizard(t).Result(in, map[string]string{
		keyAgentClass:   "zeta-agent",
		keyChannelName:  "zeta-agent-trigger",
		keyAuthzSubject: "service:zeta-agent-bot",
		keyInterval:     defaultInterval,
		keyMapping:      defaultMapping,
	})
	require.NoError(t, err)

	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "zeta-agent", out.ChannelManifest.Spec.AgentClass,
		"the second option must be bindable, not just the first")
}

// TestWizard_PreflightErrors covers the refusals Inputs makes before anything
// is asked. Both cases share the same shape: build an input, ask for the
// questions, expect an error matching a substring.
func TestWizard_PreflightErrors(t *testing.T) {
	cases := []struct {
		name      string
		input     channelkinds.WizardInput
		errSubstr string
	}{
		{
			name:      "no AgentClasses in namespace: error mentions no AgentClasses",
			input:     channelkinds.WizardInput{K8s: newFakeK8s().Build(), Namespace: "default"},
			errSubstr: "no AgentClasses",
		},
		{
			name:      "missing namespace: error mentions namespace",
			input:     channelkinds.WizardInput{},
			errSubstr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qs, err := kindWizard(t).Inputs(context.Background(), tc.input)
			require.Error(t, err, "expected error")
			assert.Nil(t, qs, "a refusing Inputs declares no questions")
			assert.Contains(t, err.Error(), tc.errSubstr, "error message")
		})
	}
}

// TestWizard_EveryAnswerResultNeedsIsDeclared is the join between the two
// halves of this kind's contract: Inputs is the ONLY place a client learns
// which --answer keys it may accept, and Result is what refuses a run that is
// missing one. A key Result requires but Inputs never declares is refused at
// the flag and then demanded by the manifests — a run nobody can complete.
//
// The required set is read out of Result's own refusal rather than
// transcribed: each key is dropped from an otherwise complete answer map, and
// the error naming it is what proves Result requires it.
func TestWizard_EveryAnswerResultNeedsIsDeclared(t *testing.T) {
	cli := newFakeK8s(newAgentClass("crm-companies", "default")).Build()
	in := channelkinds.WizardInput{K8s: cli, Namespace: "default"}

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)
	declared := map[string]bool{}
	for _, q := range qs {
		declared[q.Name] = true
	}

	full := map[string]string{
		keyAgentClass:   "crm-companies",
		keyChannelName:  "crm-companies-trigger",
		keyAuthzSubject: "service:crm-companies-bot",
		keyInterval:     defaultInterval,
		keyMapping:      defaultMapping,
	}
	for key := range full {
		t.Run("Result requires "+key+", so Inputs must declare it", func(t *testing.T) {
			answers := map[string]string{}
			for k, v := range full {
				if k != key {
					answers[k] = v
				}
			}
			_, err := kindWizard(t).Result(in, answers)
			require.Errorf(t, err, "Result must refuse a run missing %q", key)
			assert.Contains(t, err.Error(), key)
			assert.Truef(t, declared[key],
				"Result requires %q but Inputs never declares it, so no client would accept the flag", key)
		})
	}
}

// TestWizardResult_RefusesAMalformedAuthzSubject is the one answer whose SHAPE
// this kind must check itself.
//
// A channel wizard's questions carry no validation a client evaluates
// (channelkinds.ValidateInputs refuses a Question.Validation outright), so
// Result is the only gate: a subject that is not "service:<name>" takes the
// bound AgentClass to Valid=False, which also takes down its paired output
// Channel.
func TestWizardResult_RefusesAMalformedAuthzSubject(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default"}
	for _, subject := range []string{"user:someone", "crm-companies-bot", "service:"} {
		t.Run(subject, func(t *testing.T) {
			_, err := kindWizard(t).Result(in, map[string]string{
				keyAgentClass:   "crm-companies",
				keyChannelName:  "crm-companies-trigger",
				keyAuthzSubject: subject,
				keyInterval:     defaultInterval,
				keyMapping:      defaultMapping,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), keyAuthzSubject)
		})
	}
}

// pinnedBentoQuestions is what Inputs must return for a namespace holding
// exactly one AgentClass, "crm-companies" — the same fixture
// TestWizard_BuildsBentoChannelCR drives. It is its own function, not inlined,
// so a negative control can restate exactly what it broke.
func pinnedBentoQuestions() []kindtest.PromptShape {
	return []kindtest.PromptShape{
		{Name: keyAgentClass, Type: oap.QEnum, Prompt: "Bind to AgentClass", Required: true},
		{Name: keyChannelName, Type: oap.QString, Prompt: "Channel name", Required: true},
		{Name: keyAuthzSubject, Type: oap.QString, Prompt: "SpiceDB authzSubject", Required: true},
		{Name: keyInterval, Type: oap.QString, Prompt: "Bento interval", Required: true},
		{Name: keyMapping, Type: oap.QString, Prompt: "Bloblang mapping", Required: true},
	}
}

// TestWizardInputs_MatchesPinnedPromptShapes is bento's per-kind gate on
// P5-R3: the PROMPT TEXT an operator reads is asserted here, and nowhere else
// in the tree — a question that lost its wording while keeping its key would
// otherwise be invisible.
//
// kindtest.AssertPromptShapes both validates (channelkinds.ValidateInputs) and
// compares the whole slice in one assert.Equal, so an ADDED, DROPPED or
// REORDERED question fails exactly as loudly as a reworded prompt.
//
// bento is non-attributable — a scheduled firing has no human to attribute
// to — so keyAuthzSubject is asked unconditionally; see
// wizardkeys.ValidateAuthzSubject for what happens to a malformed one.
func TestWizardInputs_MatchesPinnedPromptShapes(t *testing.T) {
	cli := newFakeK8s(newAgentClass("crm-companies", "default")).Build()
	qs, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{K8s: cli, Namespace: "default"})
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, pinnedBentoQuestions())
}

// TestWizardInputs_KeepsDependentDefaultsWithASingleAgentClass pins one half
// of ruling P5-R16: with exactly one AgentClass in the namespace, classes[0]
// IS the unambiguous answer (there is no other choice), so "name" and
// "authzsubject" keep deriving their defaults from it — matching what a
// bare-Enter Screens run also produces (see TestWizardResult_MatchesTheScreenPath).
func TestWizardInputs_KeepsDependentDefaultsWithASingleAgentClass(t *testing.T) {
	cli := newFakeK8s(newAgentClass("crm-companies", "default")).Build()
	qs, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{K8s: cli, Namespace: "default"})
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	assert.Equal(t, "crm-companies", byName[keyAgentClass].Default, "AgentClass default")
	assert.Equal(t, "crm-companies-trigger", byName[keyChannelName].Default, "Name default")
	assert.Equal(t, "service:crm-companies-bot", byName[keyAuthzSubject].Default, "Subject default")
}

// TestWizardInputs_OmitsDependentDefaultsWithMultipleAgentClasses pins ruling
// P5-R16 (a wrong default is worse than no default): this test used to
// assert the OPPOSITE — that "name" and "authzsubject" derive from classes[0]
// even with more than one AgentClass present. That was the wrong behavior the
// ruling corrected (a wrong guess can be silently accepted on a blank answer;
// an omitted default cannot), so it is inverted here rather than deleted, to
// keep a gate on the multi-class case.
//
// The AgentClass question's OWN Default is unaffected — see Inputs' doc for
// why there is no equivalent "omit" available for an enum.
func TestWizardInputs_OmitsDependentDefaultsWithMultipleAgentClasses(t *testing.T) {
	cli := newFakeK8s(
		newAgentClass("alpha-agent", "default"),
		newAgentClass("zeta-agent", "default"),
	).Build()
	qs, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{K8s: cli, Namespace: "default"})
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	assert.Equal(t, "alpha-agent", byName[keyAgentClass].Default,
		"the AgentClass picker's own pre-selection is unaffected by this ruling")
	assert.Nil(t, byName[keyChannelName].Default,
		"Name must have NO derived default with more than one AgentClass in the namespace")
	assert.Nil(t, byName[keyAuthzSubject].Default,
		"Subject must have NO derived default with more than one AgentClass in the namespace")
}

// TestWizardInputs_RequiresAgentClasses: a namespace with no AgentClass has
// nothing to bind a Channel to, so the run is refused before anything is
// asked. Duplicated by TestWizard_PreflightErrors, which drives the same
// refusal as a table row; this one names it directly.
func TestWizardInputs_RequiresAgentClasses(t *testing.T) {
	_, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{K8s: newFakeK8s().Build(), Namespace: "default"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no AgentClasses")
}

// TestWizardInputs_RequiresNamespace: every manifest is namespaced, so a run
// with no namespace is refused before anything is asked.
func TestWizardInputs_RequiresNamespace(t *testing.T) {
	_, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

// TestWizardInputs_OfflineRunAcceptsASeededAgentClass covers the one route on
// which a seeded AgentClass is taken on trust: a nil K8s client, which is an
// offline dry-run with no cluster to check the name against.
//
// It is deliberately NOT the "a flag means we can skip the check" case that
// this kind used to have. wizardkeys.AgentClassQuestion verifies a seeded
// binding against the namespace's listing whenever there IS a client — see
// TestWizardInputs_SeededAgentClassMustExist — because a Channel bound to an
// AgentClass that does not exist lints clean and never works. What survives
// here is only that an offline run can still state a renderable question.
func TestWizardInputs_OfflineRunAcceptsASeededAgentClass(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{
		Namespace: "default",
		Seeded:    map[string]string{keyAgentClass: "seeded-agent"},
	})
	require.NoError(t, err, "a seeded AgentClass must not require a live cluster listing")
	require.NoError(t, channelkinds.ValidateInputs(qs), "must still be renderable")

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	require.Contains(t, byName, keyAgentClass)
	assert.Equal(t, []string{"seeded-agent"}, byName[keyAgentClass].Enum,
		"the seeded value stands in as the sole Enum option, so ValidateInputs' "+
			"QEnum-needs-values rule stays satisfied even though renderQuestions "+
			"will drop this question before it ever renders")
	assert.Equal(t, "seeded-agent", byName[keyAgentClass].Default)
	assert.Equal(t, "seeded-agent-trigger", byName[keyChannelName].Default,
		"name's default must derive from the AgentClass actually named, not from an unrelated listing")
	assert.Equal(t, "service:seeded-agent-bot", byName[keyAuthzSubject].Default)
}

// TestWizardInputs_SeededAgentClassMustExist pins the behaviour change this
// kind took on when the three copies of the AgentClass question converged on
// wizardkeys.AgentClassQuestion: a --answer agentclass=<name> naming a class
// the namespace does not hold is REFUSED, where bento used to accept it and
// emit a Channel bound to nothing.
//
// The cluster deliberately holds a DIFFERENT AgentClass rather than none, so a
// pass cannot come from the empty-namespace refusal ("no AgentClasses found")
// that a seeded run never used to reach either.
func TestWizardInputs_SeededAgentClassMustExist(t *testing.T) {
	k8s := newFakeK8s(newAgentClass("demo-agent", "default")).Build()

	_, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{
		K8s:       k8s,
		Namespace: "default",
		Seeded:    map[string]string{keyAgentClass: "does-not-exist"},
	})
	require.Error(t, err, "a seeded AgentClass that is not on the cluster must be refused")
	assert.Contains(t, err.Error(), "does-not-exist", "the refusal must name the AgentClass that is missing")
	assert.Contains(t, err.Error(), "cannot bind to an agent that does not exist",
		"and must say why, rather than reading as a listing failure")
}

// TestWizardHandoff_IsNil: bento asks only plain values, so there is no
// external service to round-trip through.
func TestWizardHandoff_IsNil(t *testing.T) {
	spec, err := kindWizard(t).Handoff(context.Background(), channelkinds.WizardInput{Namespace: "default"})
	require.NoError(t, err)
	assert.Nil(t, spec, "bento sends the operator nowhere")
}

// TestWizardResolve_DerivesNothing: bento holds no credential to verify, and
// every value its manifests need is already an answer, so its Resolve is the
// nil/nil case channelkinds.Wizard.Resolve calls the common one.
func TestWizardResolve_DerivesNothing(t *testing.T) {
	derived, err := kindWizard(t).Resolve(context.Background(), channelkinds.WizardInput{Namespace: "default"}, nil)
	require.NoError(t, err)
	assert.Empty(t, derived, "bento derives no further answers")
}

// TestWizardResult_IsPureOverItsArguments is the strong form of P5-R12's
// purity requirement: primed has already had Inputs run against a DIFFERENT
// namespace ("other-ns") before Result is called with in.Namespace="default".
// A Result that read anything the earlier call had left on the receiver would
// come back scoped to "other-ns" — or would differ from a fresh wizard's — so
// the two are compared rather than each checked alone.
func TestWizardResult_IsPureOverItsArguments(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default"}
	answers := map[string]string{
		keyAgentClass:   "crm-companies",
		keyChannelName:  "custom-name",
		keyAuthzSubject: "service:custom-bot",
		keyInterval:     "0 9 * * MON",
		keyMapping:      `root = "custom"`,
	}

	primed := kindWizard(t)
	_, err := primed.Inputs(context.Background(), channelkinds.WizardInput{
		K8s:       newFakeK8s(newAgentClass("crm-companies", "other-ns")).Build(),
		Namespace: "other-ns",
	})
	require.NoError(t, err, "Inputs")

	first, err := primed.Result(in, answers)
	require.NoError(t, err)

	second, err := kindWizard(t).Result(in, answers)
	require.NoError(t, err)

	assert.Equal(t, first, second,
		"Result must be a pure function of (in, answers) — not of receiver state an earlier Inputs call left behind")
	require.NotNil(t, first.ChannelManifest)
	assert.Equal(t, "default", first.ChannelManifest.Namespace,
		"the namespace must come from in, not from a field an earlier call set on the receiver")
}

// TestWizardResult_RefusesWithoutANamespace covers Result's one refusal that
// is not about a missing answer: every manifest it builds is namespaced.
func TestWizardResult_RefusesWithoutANamespace(t *testing.T) {
	out, err := kindWizard(t).Result(channelkinds.WizardInput{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
	assert.Nil(t, out.ChannelManifest, "a refused Result produces no manifests")
	assert.Nil(t, out.SecretManifest, "a refused Result produces no manifests")
}

// TestWizardResult_RefusesAnUnansweredQuestion: a missing answer must not
// produce a Channel with a blank name or no AgentClass binding.
func TestWizardResult_RefusesAnUnansweredQuestion(t *testing.T) {
	answers := map[string]string{
		keyAgentClass: "crm-companies",
		// name, authzsubject, interval, mapping deliberately unanswered.
	}
	_, err := kindWizard(t).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), keyChannelName)
}
