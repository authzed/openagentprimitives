package wizardrun

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// --- fixtures ----------------------------------------------------------------

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// recordingWizard records the order its steps ran in, so a test can assert
// that a step which must not be reached was not reached — rather than
// asserting only on the error, which a wizard that ran everything and then
// failed would also produce.
type recordingWizard struct {
	steps      *[]string
	resolveOut map[string]string
	resolveErr error
	resultErr  error
	// sawAnswers is what Result was handed, captured so the merge rules are
	// checkable without a second wizard.
	sawAnswers map[string]string
}

func (w *recordingWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	*w.steps = append(*w.steps, "inputs")
	return nil, nil
}

func (w *recordingWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

func (w *recordingWizard) Resolve(_ context.Context, _ channelkinds.WizardInput, _ map[string]string) (map[string]string, error) {
	*w.steps = append(*w.steps, "resolve")
	return w.resolveOut, w.resolveErr
}

func (w *recordingWizard) Result(_ channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	*w.steps = append(*w.steps, "result")
	w.sawAnswers = map[string]string{}
	for k, v := range answers {
		w.sawAnswers[k] = v
	}
	return channelkinds.WizardOutput{}, w.resultErr
}

// handoffSpec is a minimal spec that satisfies ValidateHandoffSpec — a
// non-nil Begin and Complete — so a test of the ORDER is not passing because
// validation refused first.
func handoffSpec() *channelkinds.HandoffSpec {
	return &channelkinds.HandoffSpec{
		Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
			return channelkinds.HandoffStart{URL: "https://demo.example/start"}, nil
		},
		Complete: func(context.Context, url.Values) (map[string]string, error) { return nil, nil },
	}
}

// --- the order ---------------------------------------------------------------

// TestFinish_ACollidingNameIsRefusedBeforeAnythingIrreversibleRuns is P5-R21 at
// the shared level: the collision check is in front of BOTH steps that do work
// no client can take back — the handoff (github creates a real App) and
// Resolve (slack creates and installs a real app, and no Slack API lists a
// user's apps afterwards).
//
// The assertion that carries the property is that neither step RAN, not that
// Finish errored: a Finish that drove the handoff, resolved, and then noticed
// the collision would also error, having already orphaned an app.
func TestFinish_ACollidingNameIsRefusedBeforeAnythingIrreversibleRuns(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns"},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()

	var steps []string
	drove := false
	w := &recordingWizard{steps: &steps}

	_, err := Finish(context.Background(), Params{
		Wizard:  w,
		In:      channelkinds.WizardInput{K8s: c, Namespace: "demo-ns"},
		Answers: map[string]string{"name": "demo-channel"},
		Handoff: handoffSpec(),
		DriveHandoff: func(context.Context, *channelkinds.HandoffSpec, map[string]string) (map[string]string, error) {
			drove = true
			return nil, nil
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `channel "demo-channel" already exists`)
	assert.False(t, drove, "the handoff creates a real App; it must not run for a name that cannot be created")
	assert.Empty(t, steps, "neither Resolve nor Result may run once the name is known to be taken")
}

// TestFinish_MonitoringReconfiguresTheChannelItMatched: a monitoring run's
// name being taken is the point rather than the problem, so the collision
// check is skipped — the same guard the CLI's own pre-wizard check makes.
func TestFinish_MonitoringReconfiguresTheChannelItMatched(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns"},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()

	var steps []string
	w := &recordingWizard{steps: &steps}

	_, err := Finish(context.Background(), Params{
		Wizard:  w,
		In:      channelkinds.WizardInput{K8s: c, Namespace: "demo-ns", Monitoring: true},
		Answers: map[string]string{"name": "demo-channel"},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"resolve", "result"}, steps)
}

// TestFinish_TheHandoffRunsBeforeResolveAndBothOverwrite pins the two rules
// that decide what Result finally reads: the handoff runs first because
// Resolve reads what it produced (github's manual route answers with a PATH to
// the key file and Resolve turns it into the key), and each later stage
// overwrites — a value an external service just minted beats one a form
// guessed at.
func TestFinish_TheHandoffRunsBeforeResolveAndBothOverwrite(t *testing.T) {
	var steps []string
	w := &recordingWizard{steps: &steps, resolveOut: map[string]string{
		"from-handoff": "resolve-won",
		"resolved":     "yes",
	}}

	_, err := Finish(context.Background(), Params{
		Wizard: w,
		In:     channelkinds.WizardInput{Namespace: "demo-ns"},
		Answers: map[string]string{
			"typed":        "by-the-operator",
			"from-handoff": "form-guessed",
		},
		Handoff: handoffSpec(),
		DriveHandoff: func(_ context.Context, _ *channelkinds.HandoffSpec, answers map[string]string) (map[string]string, error) {
			steps = append(steps, "handoff")
			// It reads the collected answers, which is the whole reason it is
			// handed them.
			assert.Equal(t, "by-the-operator", answers["typed"])
			return map[string]string{"from-handoff": "handoff-won"}, nil
		},
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"handoff", "resolve", "result"}, steps)

	require.NotNil(t, w.sawAnswers)
	assert.Equal(t, "by-the-operator", w.sawAnswers["typed"])
	assert.Equal(t, "yes", w.sawAnswers["resolved"])
	assert.Equal(t, "resolve-won", w.sawAnswers["from-handoff"],
		"Resolve overwrites the handoff, which overwrote the form")
}

// TestFinish_AKindNeedingAHandoffIsRefusedWhenTheClientCannotDriveOne: a
// client that wired no driver must not silently skip the step and build
// manifests from answers the round trip was going to supply.
func TestFinish_AKindNeedingAHandoffIsRefusedWhenTheClientCannotDriveOne(t *testing.T) {
	var steps []string
	w := &recordingWizard{steps: &steps}

	_, err := Finish(context.Background(), Params{
		Wizard:  w,
		In:      channelkinds.WizardInput{Namespace: "demo-ns"},
		Answers: map[string]string{},
		Handoff: handoffSpec(),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser round trip")
	assert.Empty(t, steps, "nothing may be built from a run whose handoff never happened")
}

// TestFinish_AMalformedSpecIsRefusedByNameBeforeTheDriverRuns: Begin and
// Complete are dereferenced by whichever client drives the spec, so the
// refusal has to be in front of the driver, not inside it.
func TestFinish_AMalformedSpecIsRefusedByNameBeforeTheDriverRuns(t *testing.T) {
	cases := []struct {
		name string
		spec *channelkinds.HandoffSpec
		want string
	}{
		{
			name: "no Begin: nowhere to send the operator",
			spec: &channelkinds.HandoffSpec{Complete: handoffSpec().Complete},
			want: "no Begin",
		},
		{
			name: "no Complete: nothing to do with the callback",
			spec: &channelkinds.HandoffSpec{Begin: handoffSpec().Begin},
			want: "no Complete",
		},
		{
			name: "satisfiedBy naming no fallback question: a typo that silently does nothing",
			spec: func() *channelkinds.HandoffSpec {
				s := handoffSpec()
				s.SatisfiedBy = map[string]string{"nosuch": "private-key"}
				return s
			}(),
			want: `satisfiedBy names "nosuch"`,
		},
		{
			name: "a fallback question no renderer can honor: refused up front, not mid-detour",
			spec: func() *channelkinds.HandoffSpec {
				s := handoffSpec()
				s.FallbackInputs = []oap.Question{{
					Name: "q", Type: oap.QString, Prompt: "Q",
					Binding: []oap.Binding{{Target: "AgentClass/x#spec.y"}},
				}}
				return s
			}(),
			want: "binding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var steps []string
			drove := false
			_, err := Finish(context.Background(), Params{
				Wizard:  &recordingWizard{steps: &steps},
				In:      channelkinds.WizardInput{Namespace: "demo-ns"},
				Answers: map[string]string{},
				Handoff: tc.spec,
				DriveHandoff: func(context.Context, *channelkinds.HandoffSpec, map[string]string) (map[string]string, error) {
					drove = true
					return nil, nil
				},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, drove)
			assert.Empty(t, steps)
		})
	}
}

// TestFinish_AResolveFailureIsReturnedVerbatim: the kind's sentence is the one
// line that says what the operator has to fix; a dispatcher-level wrapper
// would bury it.
func TestFinish_AResolveFailureIsReturnedVerbatim(t *testing.T) {
	var steps []string
	cause := errors.New("Slack rejected this bot token (auth.test): invalid_auth")
	w := &recordingWizard{steps: &steps, resolveErr: cause}

	_, err := Finish(context.Background(), Params{
		Wizard:  w,
		In:      channelkinds.WizardInput{Namespace: "demo-ns"},
		Answers: map[string]string{},
	})

	require.Error(t, err)
	assert.Equal(t, cause.Error(), err.Error())
	assert.NotContains(t, steps, "result", "no manifests may be built from an unverified credential")
}

// --- the declared role -------------------------------------------------------

// rolelessWizard produces a Channel and sets NO role on it, which is the only
// case where the declared role is load-bearing.
//
// It is the fixture shape this property has to be tested with, and the reason
// is a defect that survived a full review: admind's end-to-end submit test used
// the bento kind, whose Result hardcodes role=input, so a client that stamped
// nothing produced exactly what a client that stamped "input" produced, and the
// test could not tell them apart. slack's non-monitoring Result is the
// production instance of this shape — it sets no role, so a Channel it produces
// takes ChannelSpec.Role's `both` default, and role=both is deliberately not an
// output-binding candidate (channelkinds/outputbind).
// role, when set, is what this wizard's own Result puts on the Channel —
// standing in for bento and github, which hardcode one. Empty is the roleless
// case above.
type rolelessWizard struct {
	role      string
	resultErr error
}

func (w *rolelessWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return nil, nil
}

func (w *rolelessWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

func (w *rolelessWizard) Resolve(context.Context, channelkinds.WizardInput, map[string]string) (map[string]string, error) {
	return nil, nil
}

func (w *rolelessWizard) Result(in channelkinds.WizardInput, _ map[string]string) (channelkinds.WizardOutput, error) {
	if w.resultErr != nil {
		return channelkinds.WizardOutput{}, w.resultErr
	}
	return channelkinds.WizardOutput{
		ChannelManifest: &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: in.Namespace},
			Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "demo-kind", Role: w.role},
		},
	}, nil
}

// TestFinish_TheDeclaredRoleIsStampedOntoTheChannel is the property both
// clients now get from one place. Before it lived here, `oap agent install`
// stamped the role and admind did not, so the same declaration produced a
// role=output Channel from the terminal and a role=both one from the console —
// and `both` is not an output-binding candidate, so the agent's input Channel
// went Valid=False with nowhere to deliver.
func TestFinish_TheDeclaredRoleIsStampedOntoTheChannel(t *testing.T) {
	out, err := Finish(context.Background(), Params{
		Wizard:  &rolelessWizard{},
		In:      channelkinds.WizardInput{Namespace: "demo-ns", Role: spiceboxv1alpha1.ChannelRoleOutput},
		Answers: map[string]string{},
	})

	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, out.ChannelManifest.Spec.Role,
		"a caller that declared role: output must not get a Channel left on the CRD's `both` default")
}

// TestFinish_TheRoleStampedIsTheRoleTheKindWasAskedWith closes the join the
// declared role used to have a hole in.
//
// The role reached the Channel from Params and nothing else, so a kind's own
// Inputs never saw it — and a kind whose Channel needs a field that only one
// role needs (slack's role=output destination) could not ask for it. Reading
// the stamp off the same WizardInput every step of the run is handed makes the
// two unable to disagree: there is one role, and asking a kind with one role
// while stamping another is not expressible.
func TestFinish_TheRoleStampedIsTheRoleTheKindWasAskedWith(t *testing.T) {
	w := &roleRecordingWizard{}
	out, err := Finish(context.Background(), Params{
		Wizard:  w,
		In:      channelkinds.WizardInput{Namespace: "demo-ns", Role: spiceboxv1alpha1.ChannelRoleOutput},
		Answers: map[string]string{},
	})

	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, w.resolveRole,
		"Resolve is asked with the role its Channel will be stamped with")
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, w.resultRole,
		"Result is asked with the role its Channel will be stamped with")
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, out.ChannelManifest.Spec.Role)
}

// roleRecordingWizard records the WizardInput.Role each step was handed, so a
// run can be checked against what the kind actually saw rather than only
// against what came back.
type roleRecordingWizard struct {
	resolveRole string
	resultRole  string
}

func (w *roleRecordingWizard) Inputs(context.Context, channelkinds.WizardInput) ([]oap.Question, error) {
	return nil, nil
}

func (w *roleRecordingWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return nil, nil
}

func (w *roleRecordingWizard) Resolve(_ context.Context, in channelkinds.WizardInput, _ map[string]string) (map[string]string, error) {
	w.resolveRole = in.Role
	return nil, nil
}

func (w *roleRecordingWizard) Result(in channelkinds.WizardInput, _ map[string]string) (channelkinds.WizardOutput, error) {
	w.resultRole = in.Role
	return channelkinds.WizardOutput{
		ChannelManifest: &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: in.Namespace},
			Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "demo-kind"},
		},
	}, nil
}

// TestFinish_NoDeclaredRoleLeavesTheKindsOwnAnswerAlone: the stamp is not a
// blanket rewrite. `oap channel create` with no --role, and every bundle
// written before requires.channels existed, must reach the same manifests they
// always did — including for a kind that sets a role ITSELF (bento and github
// both hardcode one, and overwriting theirs with "" would produce a Channel
// their own ValidateSpec rejects).
//
// It is pinned here and not through either client because neither can express
// it: LintRequiredChannels defaults an omitted declared role to `both` and
// refuses it for any kind that does not serve one, and bento — the kind whose
// Result sets a role — serves input alone.
func TestFinish_NoDeclaredRoleLeavesTheKindsOwnAnswerAlone(t *testing.T) {
	cases := []struct {
		name     string
		kindRole string
	}{
		{name: "a kind that sets no role: the field stays unset, so the apiserver applies its own default", kindRole: ""},
		{name: "a kind that sets its own role: that role survives untouched", kindRole: spiceboxv1alpha1.ChannelRoleInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Finish(context.Background(), Params{
				Wizard:  &rolelessWizard{role: tc.kindRole},
				In:      channelkinds.WizardInput{Namespace: "demo-ns"},
				Answers: map[string]string{},
			})

			require.NoError(t, err)
			require.NotNil(t, out.ChannelManifest)
			assert.Equal(t, tc.kindRole, out.ChannelManifest.Spec.Role,
				"an undeclared role must leave the field exactly as the kind left it")
		})
	}
}

// TestFinish_AnUnstampableRoleIsRefusedBeforeAnythingIrreversibleRuns: the role
// is validated in FRONT of the run for the collision check's reason. A role
// that cannot be stamped is knowable before a single question is asked, and
// discovering it after Resolve would cost the operator a real Slack app for a
// value that was never going to be accepted.
func TestFinish_AnUnstampableRoleIsRefusedBeforeAnythingIrreversibleRuns(t *testing.T) {
	cases := []struct {
		name       string
		role       string
		monitoring bool
		wantIn     string
	}{{
		name:   "a role no ChannelSpec.Role enum value matches: refused naming the legal set",
		role:   "outputs",
		wantIn: "input|output|both",
	}, {
		name:   "role=monitoring: refused, pointing at the flag that does ask for one",
		role:   spiceboxv1alpha1.ChannelRoleMonitoring,
		wantIn: "--monitoring",
	}, {
		name:       "a monitoring run given an agent role: refused as the contradiction it is",
		role:       spiceboxv1alpha1.ChannelRoleOutput,
		monitoring: true,
		wantIn:     "binds to no agent",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var steps []string
			w := &recordingWizard{steps: &steps}

			_, err := Finish(context.Background(), Params{
				Wizard:  w,
				In:      channelkinds.WizardInput{Namespace: "demo-ns", Monitoring: tc.monitoring, Role: tc.role},
				Answers: map[string]string{},
			})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.Empty(t, steps,
				"the refusal must land before Resolve and Result, not after the kind has already done work outside the cluster")
		})
	}
}

// --- the apply ---------------------------------------------------------------

// recordingApplier records every object it was handed, in order, with the
// manager it was applied under.
type recordingApplier struct {
	got []appliedObject
	err error
}

type appliedObject struct {
	kind, name, manager string
}

func (a *recordingApplier) ApplyObject(_ context.Context, obj *unstructured.Unstructured, fieldManager string) error {
	a.got = append(a.got, appliedObject{kind: obj.GetKind(), name: obj.GetName(), manager: fieldManager})
	return a.err
}

func capabilityPatch() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	u.SetKind("AgentClass")
	u.SetNamespace("demo-ns")
	u.SetName("demo-class")
	return u
}

func wizardOutput() channelkinds.WizardOutput {
	return channelkinds.WizardOutput{
		SecretManifest: &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: "demo-channel-creds", Namespace: "demo-ns"},
		},
		ChannelManifest: &spiceboxv1alpha1.Channel{
			TypeMeta:   metav1.TypeMeta{APIVersion: "agentprimitives.authzed.com/v1alpha1", Kind: "Channel"},
			ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns"},
		},
		CapabilityPatch: capabilityPatch(),
	}
}

// TestApply_TheCapabilityPatchGoesLastAndUnderItsOwnManager: the failure this
// order leaves behind is the safe one — a Channel whose newly requested
// features are off, rather than capabilities enabled for a Channel that failed
// to be created. And the AgentClass is not the wizard's object, so the wizard's
// claim on it must be a manager of its own.
func TestApply_TheCapabilityPatchGoesLastAndUnderItsOwnManager(t *testing.T) {
	ap := &recordingApplier{}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	require.NoError(t, Apply(context.Background(), c, ap, wizardOutput()))

	require.Len(t, ap.got, 3)
	assert.Equal(t, appliedObject{kind: "Secret", name: "demo-channel-creds", manager: FieldManager}, ap.got[0],
		"the credentials must exist before the Channel that reads them")
	assert.Equal(t, appliedObject{kind: "Channel", name: "demo-channel", manager: FieldManager}, ap.got[1])
	assert.Equal(t, appliedObject{kind: "AgentClass", name: "demo-class", manager: CapabilityFieldManager}, ap.got[2])
	assert.NotEqual(t, FieldManager, CapabilityFieldManager)
}

// TestApply_ANameThatIsAlreadyTakenIsRefusedBeforeAnythingIsWritten: the apply
// is a server-side apply, so a duplicate name would silently merge over a
// Channel the operator did not mean to touch. This is the last net.
func TestApply_ANameThatIsAlreadyTakenIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns"},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	ap := &recordingApplier{}

	err := Apply(context.Background(), c, ap, wizardOutput())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Empty(t, ap.got, "not even the Secret may be written when the Channel cannot be")
}

// TestApply_ReplaceExistingIsTheOneWayPastTheNet: only the wizard itself sets
// it, when it matched and is intentionally reconfiguring a monitoring Channel
// in place.
func TestApply_ReplaceExistingIsTheOneWayPastTheNet(t *testing.T) {
	existing := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "demo-ns"},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	ap := &recordingApplier{}

	out := wizardOutput()
	out.ReplaceExisting = true
	require.NoError(t, Apply(context.Background(), c, ap, out))
	assert.Len(t, ap.got, 3)
}

// TestApply_ANilSecretLeavesTheExistingCredentialsAlone: a re-setup where the
// operator left the token blank must not replace the Secret with an empty one.
func TestApply_ANilSecretLeavesTheExistingCredentialsAlone(t *testing.T) {
	ap := &recordingApplier{}
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	out := wizardOutput()
	out.SecretManifest = nil
	require.NoError(t, Apply(context.Background(), c, ap, out))

	require.Len(t, ap.got, 2)
	assert.Equal(t, "Channel", ap.got[0].kind)
	assert.Equal(t, "AgentClass", ap.got[1].kind)
}

// TestApply_AClientWithNoApplierIsRefusedRatherThanReportingSuccess: a nil
// Applier is a caller that forgot to wire the mechanism, and returning nil
// would report an install that wrote nothing as done.
func TestApply_AClientWithNoApplierIsRefusedRatherThanReportingSuccess(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	err := Apply(context.Background(), c, nil, wizardOutput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no applier")
}

// TestClientApplier_StampsTheProvenanceAnnotationAndIsIdempotent: the marker
// `oap clean` reads must be on what a server-side client writes too, or an
// admin-UI-created Channel is indistinguishable from pre-existing cluster
// infrastructure. It is a constant, so a byte-identical re-apply stays a no-op.
func TestClientApplier_StampsTheProvenanceAnnotationAndIsIdempotent(t *testing.T) {
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	ap := ClientApplier(c, "ap")

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("agentprimitives.authzed.com/v1alpha1")
	obj.SetKind("Channel")
	obj.SetNamespace("demo-ns")
	obj.SetName("demo-channel")

	require.NoError(t, ap.ApplyObject(context.Background(), obj, FieldManager))
	assert.Equal(t, "ap", obj.GetAnnotations()[InstalledByAnnotation])

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "demo-ns", Name: "demo-channel"}, &got))
	assert.Equal(t, "ap", got.Annotations[InstalledByAnnotation])
}

// TestInstalledByAnnotationMatchesTheClusterInstaller pins this package's
// spelling against pkg/platform/cloud's, which is the other pkg/-side
// definition of the same API-server string. `oap clean` reads ONE key; three
// spellings of it that drift means a resource this package writes stops being
// recognised as ap-managed, silently.
func TestInstalledByAnnotationMatchesTheClusterInstaller(t *testing.T) {
	assert.Equal(t, cloud.InstalledByAnnotation, InstalledByAnnotation)
}
