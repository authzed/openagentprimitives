package channelwizard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser" // register browser kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"    // register fake kind
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"   // register local kind
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

// stubKindWizard is a channelkinds.Wizard whose every answer a test controls:
// what it declares, what its handoff is, what it derives, and what it was
// handed.
//
// A local double rather than a real kind, because the shapes these tests need
// are ones no single real kind has all of — a question already seeded, a
// question dropped before it is asked, a derivation step that fails — and
// because recording WHAT REACHED Result is the only way to tell a run that
// carried the answers through from one that merely returned nil.
type stubKindWizard struct {
	inputs  []oap.Question
	handoff *channelkinds.HandoffSpec

	// resolved and resolveErr are what this kind's derivation step answers
	// with; both zero means it derives nothing, the common case.
	resolved   map[string]string
	resolveErr error

	// What Result was handed, recorded so a test can assert the answers
	// actually arrived rather than that the run merely returned nil.
	gotIn      channelkinds.WizardInput
	gotAnswers map[string]string
	resulted   bool

	// gotInputsIn is what the DECLARATION step was handed, recorded
	// separately from gotIn: a kind shapes its question set from this call,
	// and a value that only reached Result would be too late for the
	// questions to have been shaped around it.
	gotInputsIn channelkinds.WizardInput

	// gotResolveAnswers is what Resolve was handed, recorded so a test can
	// prove the step ran against the RUN's answers rather than an empty map.
	gotResolveAnswers map[string]string
}

func (w *stubKindWizard) Inputs(_ context.Context, in channelkinds.WizardInput) ([]oap.Question, error) {
	w.gotInputsIn = in
	return w.inputs, nil
}

func (w *stubKindWizard) Handoff(context.Context, channelkinds.WizardInput) (*channelkinds.HandoffSpec, error) {
	return w.handoff, nil
}

func (w *stubKindWizard) Resolve(_ context.Context, _ channelkinds.WizardInput, answers map[string]string) (map[string]string, error) {
	// Copied rather than aliased: the dispatcher merges the derived answers
	// into the very map it passed in, so holding the reference would record
	// the POST-merge state and a test asserting what Resolve saw would be
	// asserting what Result saw.
	w.gotResolveAnswers = map[string]string{}
	for k, v := range answers {
		w.gotResolveAnswers[k] = v
	}
	if w.resolveErr != nil {
		return nil, w.resolveErr
	}
	return w.resolved, nil
}

func (w *stubKindWizard) Result(in channelkinds.WizardInput, answers map[string]string) (channelkinds.WizardOutput, error) {
	w.gotIn, w.gotAnswers, w.resulted = in, answers, true
	return channelkinds.WizardOutput{
		ChannelManifest: &spiceboxv1alpha1.Channel{
			Spec: spiceboxv1alpha1.ChannelSpec{Kind: "demo-kind"},
		},
	}, nil
}

// scriptedOptions presents a run over the line-oriented driver, answering
// each question from script in order. Used where a test needs the run to
// COMPLETE, so it can assert on what reached Result rather than only on which
// refusal came back.
func scriptedOptions(script string) tui.Options {
	th := tui.NewTheme(tui.Caps{})
	return tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader(script), io.Discard, th)}
}

// screenIDs names the screens a run would present, in order — which is also
// the step rail, since tui.Steps derives the rail from exactly this slice.
func screenIDs(screens []tui.Screen) []string {
	ids := make([]string, 0, len(screens))
	for _, s := range screens {
		ids = append(ids, s.ID())
	}
	return ids
}

// TestRunChannelWizard_AnswerForAKindsOwnInputIsAccepted closes the
// join between renderQuestions (which DROPS a question the flags already
// answered) and checkAnswerKeys (which refuses an --answer key the flow does
// not declare).
//
// Both are individually correct and the composition is not: deriving the
// declared set from the screens that SURVIVED rendering makes the key the
// operator just supplied invisible, so the flag is refused on precisely the
// value it was given. The kind here declares two inputs and the run seeds
// one, which is the shape that discriminates — with only the seeded input
// declared, no screen survives, the declared set is empty and checkAnswerKeys
// skips itself entirely.
func TestRunChannelWizard_AnswerForAKindsOwnInputIsAccepted(t *testing.T) {
	w := &stubKindWizard{inputs: []oap.Question{
		{Name: "org", Type: oap.QString, Prompt: "Which organization?"},
		{Name: "team", Type: oap.QString, Prompt: "Which team?"},
	}}

	st, seeded, err := Seed("", []string{"org=demo-org"})
	require.NoError(t, err, "Seed")

	_, answered, err := Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions("demo-team\n"))
	require.NoError(t, err,
		"--answer naming one of this kind's own declared inputs must be accepted, not refused as a key it does not ask")

	require.True(t, w.resulted, "the run must have reached Result")
	assert.Equal(t, map[string]string{"org": "demo-org", "team": "demo-team"}, w.gotAnswers,
		"Result must receive the seeded answer and the asked one alike")
	assert.Equal(t, "default", w.gotIn.Namespace,
		"Result must receive the same WizardInput Inputs was given")
	assert.Empty(t, answered.Notes(),
		"a kind runs no code of its own while the questions are answered, so nothing but the questions can note anything")
}

func TestResolvePreparedRunCollectsFallbackAnswersAndFinishNeverPrompts(t *testing.T) {
	w := &stubKindWizard{
		inputs: []oap.Question{{Name: "route", Type: oap.QString, Prompt: "Route"}},
		handoff: &channelkinds.HandoffSpec{
			Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
				return channelkinds.HandoffStart{}, nil
			},
			Complete: func(context.Context, url.Values) (map[string]string, error) {
				return nil, nil
			},
			SkipWhen:       func(map[string]string) string { return "the manual route was selected" },
			FallbackInputs: []oap.Question{{Name: "token", Type: oap.QSecret, Prompt: "Token"}},
		},
	}
	st, seeded, err := Seed("", nil)
	require.NoError(t, err)
	theme := tui.NewTheme(tui.Caps{})
	driver := &recordingDriver{inner: tui.Plain(strings.NewReader("manual\ns3cr3t\n"), io.Discard, theme)}

	prepared, answered, err := PrepareRun(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded, nil,
		tui.Options{Theme: theme, Driver: driver})
	require.NoError(t, err)
	assert.Equal(t, []string{"route"}, driver.ids,
		"planning collects only ordinary inputs; handoff fallbacks belong to resolution")

	answered, err = ResolvePreparedRun(context.Background(), prepared, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"route", "token"}, driver.ids)
	require.Contains(t, prepared.SensitiveValues(), "s3cr3t")

	beforeFinish := len(driver.ids)
	_, _, err = FinishPreparedRun(prepared)
	require.NoError(t, err)
	assert.Len(t, driver.ids, beforeFinish, "execution must use the fully answered payload without presenting another screen")
	assert.Equal(t, "s3cr3t", answered.Get("token"))
}

func TestResolvePreparedRunRetainsFallbackSecretsForRedactionOnFailure(t *testing.T) {
	secret := "fallback-secret-before-resolve-failure"
	w := &stubKindWizard{
		handoff: &channelkinds.HandoffSpec{
			Begin: func(map[string]string, string) (channelkinds.HandoffStart, error) {
				return channelkinds.HandoffStart{}, nil
			},
			Complete: func(context.Context, url.Values) (map[string]string, error) { return nil, nil },
			SkipWhen: func(map[string]string) string { return "manual route" },
			FallbackInputs: []oap.Question{{
				Name: "token", Type: oap.QSecret, Prompt: "Token",
			}},
		},
		resolveErr: errors.New("provider resolution failed"),
	}
	st, seeded, err := Seed("", nil)
	require.NoError(t, err)
	theme := tui.NewTheme(tui.Caps{})
	prepared, _, err := PrepareRun(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded, nil,
		tui.Options{Theme: theme, Driver: tui.Plain(strings.NewReader(secret+"\n"), io.Discard, theme)})
	require.NoError(t, err)

	_, err = ResolvePreparedRun(context.Background(), prepared, nil, nil)
	require.ErrorContains(t, err, "provider resolution failed")
	assert.Contains(t, prepared.SensitiveValues(), secret,
		"a fallback collected before a later failure must still reach the workflow redactor")
}

func TestPrepareRunDefersMachineAnswerWithoutPrompting(t *testing.T) {
	w := &stubKindWizard{inputs: []oap.Question{{
		Name: wizardkeys.KeyExternalBaseURL, Type: oap.QString, Prompt: "External URL",
	}}}
	st, seeded, err := Seed("", nil)
	require.NoError(t, err)
	driver := &recordingDriver{inner: scriptedOptions("").Driver}

	prepared, _, err := PrepareRun(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		map[string]bool{wizardkeys.KeyExternalBaseURL: true},
		tui.Options{Theme: tui.NewTheme(tui.Caps{}), Driver: driver})
	require.NoError(t, err)
	assert.Empty(t, driver.ids)

	_, err = ResolvePreparedRun(context.Background(), prepared,
		map[string]string{wizardkeys.KeyExternalBaseURL: "https://public.example.test"}, nil)
	require.NoError(t, err)
	_, _, err = FinishPreparedRun(prepared)
	require.NoError(t, err)
	assert.Equal(t, "https://public.example.test", w.gotAnswers[wizardkeys.KeyExternalBaseURL])
	assert.Empty(t, driver.ids, "injecting a machine-derived answer during execution must not present a screen")
}

// TestPrepareRun_GithubCreateRouteDoesNotAskForInstallationBeforeHandoff
// catches the ordering break where graph planning eagerly presented every
// fallback input. GitHub cannot supply an installation ID until the App
// manifest handoff has created the App and opened its installation page.
func TestPrepareRun_GithubCreateRouteDoesNotAskForInstallationBeforeHandoff(t *testing.T) {
	kind, ok := registry.Get("github")
	require.True(t, ok)
	st, seeded, err := Seed("demo-reviewbot-gh", []string{
		"agentclass=demo-agent",
		"app-source=create",
		"owner-type=organization",
		"org=demo-org",
		"external-base-url=https://ap.demo.test",
		"authzsubject=service:demo-agent-github",
	})
	require.NoError(t, err)
	theme := tui.NewTheme(tui.Caps{})
	mutated := false
	driver := &recordingDriver{
		inner: tui.Plain(strings.NewReader("7654321\n"), io.Discard, theme), mutated: &mutated,
	}
	cluster := aptest.NewBundle(t)

	prepared, _, err := PrepareRun(context.Background(), kind.Wizard(), channelkinds.WizardInput{
		Namespace: "default", Seeded: seeded.Values, WorkingDir: t.TempDir(), OperatorShell: true,
	}, "github", st, spiceboxv1alpha1.ChannelRoleInput, seeded, nil,
		tui.Options{Theme: theme, Driver: driver})

	require.NoError(t, err)
	assert.Empty(t, driver.ids,
		"planning may collect ordinary inputs, but installation-id and manual credentials belong after the create-App handoff")

	svc := fakeServiceReading(t, "abc", "redirect_url")
	t.Cleanup(svc.Close)
	browsertest.Use(t, func(u string) error {
		if strings.HasPrefix(u, "http://127.0.0.1:") {
			return fakeBrowser(t, svc.URL, nil)(u)
		}
		// The second address is GitHub's fire-and-forget App installation
		// page. Recording it is enough; fetching it would make the test use
		// the network and it has no callback to drive.
		return nil
	})
	prepared.handoff.Complete = func(context.Context, url.Values) (map[string]string, error) {
		return map[string]string{
			"app-id":         "55555",
			"slug":           "demo-org-demo-reviewbot-gh",
			"private-key":    "-----BEGIN RSA PRIVATE KEY-----\nZmFrZQ==\n-----END RSA PRIVATE KEY-----\n",
			"webhook-secret": "whsec_faketestwebhooksecretvalue",
		}, nil
	}

	_, err = ResolvePreparedRun(context.Background(), prepared, nil, cluster.Controller)
	require.NoError(t, err)
	assert.Equal(t, []string{"installation-id"}, driver.ids,
		"the handoff creates the App credentials before its result-dependent installation ID is requested")

	questionsBeforeMutation := len(driver.ids)
	require.NoError(t, cluster.Controller.Create(context.Background(), &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "first-graph-mutation", Namespace: "default"},
	}))
	mutated = true
	_, _, err = FinishPreparedRun(prepared)
	require.NoError(t, err)
	assert.Len(t, driver.ids, questionsBeforeMutation,
		"the sealed apply payload cannot prompt after graph mutation begins")
	assert.False(t, driver.afterMutation,
		"the real GitHub create route may not present a screen after the first graph-owned Kubernetes mutation")
}

// TestRunChannelWizard_TheDeclaredRoleReachesTheKindBeforeItStatesItsQuestions
// is the join between the two things `--role` has to do.
//
// It is stamped onto the produced Channel (wizardrun.Finish), and that half
// always worked. The half that did not: a kind cannot ask for a field that
// only one role needs unless it learns the role at DECLARATION time, and the
// role used to travel beside the WizardInput rather than on it — so
// Inputs saw nothing. slack's role=output destination is the live case: the
// Channel was created with no spec.slack.outputDefaults and had to be patched
// by hand.
func TestRunChannelWizard_TheDeclaredRoleReachesTheKindBeforeItStatesItsQuestions(t *testing.T) {
	w := &stubKindWizard{}

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	out, _, err := Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st,
		spiceboxv1alpha1.ChannelRoleOutput, seeded, scriptedOptions(""))
	require.NoError(t, err)

	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, w.gotInputsIn.Role,
		"a kind states its questions from this call; a role it cannot see here cannot shape them")
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, w.gotIn.Role,
		"and the same role reaches Result, which builds the manifests")
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleOutput, out.ChannelManifest.Spec.Role,
		"the declared role is still stamped onto the Channel")
}

// TestRunChannelWizard_AnswerNamingNothingThisKindAsksIsStillRefused is the
// other half of the test above: widening the declared set to the questions
// Inputs returned must not turn checkAnswerKeys into a no-op. A typo must
// still fail at the flag, naming what the kind does ask.
func TestRunChannelWizard_AnswerNamingNothingThisKindAsksIsStillRefused(t *testing.T) {
	w := &stubKindWizard{inputs: []oap.Question{
		{Name: "org", Type: oap.QString, Prompt: "Which organization?"},
	}}

	st, seeded, err := Seed("", []string{"orgg=demo-org"})
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions("demo-org\n"))
	require.Error(t, err, "a key no question of this kind declares must be refused")
	assert.Contains(t, err.Error(), "orgg", "the refusal names the key the user typed")
	assert.Contains(t, err.Error(), "org", "and lists what the kind does ask")
	assert.False(t, w.resulted, "a refused flag must stop the run before it builds manifests")
}

// TestSeededNameDropsAKindsChannelNameQuestion closes the second
// join: --name is written into the run's State OUTSIDE Seed's --answer
// loop, so a seeded-answer map built from the --answer values alone would not
// carry it. A kind's channel-name question would then be rendered — taking
// a step on the rail (tui.Steps derives the rail from exactly these screens)
// that the run never asks, because tui.Question.Prepare returns no group for
// a key State already has.
//
// It is asserted over Seed's own output rather than a hand-built map,
// because the composition of those two functions IS the defect: a test that
// passed its own map would pass whether or not Seed carried --name.
//
// The fake kind cannot show this — it declares no questions at all — so the
// double is what stands in until a kind with a channel-name question
// migrates.
func TestSeededNameDropsAKindsChannelNameQuestion(t *testing.T) {
	qs := []oap.Question{
		{Name: "org", Type: oap.QString, Prompt: "Which organization?"},
		{Name: wizardkeys.KeyChannelName, Type: oap.QString, Prompt: "Name for the Channel"},
	}

	st, seeded, err := Seed("demo-channel", nil)
	require.NoError(t, err, "Seed")

	screens, err := renderQuestions(qs, seeded.Values, st, tui.Caps{})
	require.NoError(t, err)
	assert.Equal(t, []string{"org"}, screenIDs(screens),
		"--name answers a channel-name question, so that question must not take a step on the rail")

	// Dropped must not mean lost: the answer still has to reach Result.
	assert.Equal(t, "demo-channel", answersFrom(qs, st)[wizardkeys.KeyChannelName],
		"the name supplied with --name must still be one of the answers Result reads")
}

// TestRunChannelWizard_DrivesAHandoffAndMergesWhatItProduced is the wiring
// gate on the browser step: the dispatcher must run it BETWEEN the answers and
// Resolve, hand it what the questions collected, and merge what came back into
// the map Result reads.
//
// The exchange's key is deliberately one no question declares, so a dispatcher
// that ran the handoff and dropped its return would leave Result with an
// answer map missing it — which asserting on Result's map, rather than on the
// handoff having run, is what catches.
//
// It installs a process-wide browser driver, so it must not run in parallel
// with anything else that does.
func TestRunChannelWizard_DrivesAHandoffAndMergesWhatItProduced(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	browsertest.Use(t, fakeBrowser(t, svc.URL, nil))

	w := &stubKindWizard{
		inputs:  []oap.Question{{Name: "org", Type: oap.QString, Prompt: "Which organization?"}},
		handoff: demoHandoff(t, svc.URL),
	}

	st, seeded, err := Seed("", []string{"org=demo-org"})
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions("67890\n"))
	require.NoError(t, err, "the run must complete")

	require.True(t, w.resulted, "Result must be reached")
	assert.Equal(t, "from-abc", w.gotAnswers["app-id"],
		"what the exchange produced has to reach Result; nothing else can supply it")
	assert.Equal(t, "67890", w.gotAnswers["installation-id"],
		"and so does what the fallback asked for afterwards")
	assert.Equal(t, "from-abc", w.gotResolveAnswers["app-id"],
		"Resolve runs AFTER the handoff, so it sees what the handoff produced")
}

// TestRunChannelWizard_GithubRunsEndToEnd is the join no per-file test spans:
// the github kind's own tests exercise Inputs, Handoff, Resolve and Result one
// at a time, and this drives the REAL registered kind through the REAL
// dispatcher in the order a run takes them.
//
// Fully seeded and unattended, so nothing prompts and nothing reaches the
// network: the handoff is skipped because every credential is already in hand
// (github's Handoff returns nil for exactly that case), which is what makes
// this hermetic without a stub in sight.
func TestRunChannelWizard_GithubRunsEndToEnd(t *testing.T) {
	k, ok := registry.Get("github")
	require.True(t, ok, "the github kind must be registered in the oap binary")

	b := aptest.NewBundle(t, &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	})

	st, seeded, err := Seed("demo-reviewbot-gh", []string{
		"agentclass=demo-agent",
		// Seeded because the kind now asks for it and offers no default: a
		// login alone cannot say whether it names an organization or a
		// personal account, and every GitHub address the run produces branches
		// on the answer. An unattended run that omits it fails closed naming
		// the flag, which is the intended contract, not an obstacle.
		"owner-type=organization",
		"org=demo-org",
		"external-base-url=https://ap.demo.test",
		"authzsubject=service:demo-agent-github",
		"app-id=123456",
		"slug=demo-app",
		"private-key=-----BEGIN RSA PRIVATE KEY-----\ndemo\n-----END RSA PRIVATE KEY-----",
		"webhook-secret=demo-secret",
		"installation-id=7654321",
	})
	require.NoError(t, err, "Seed")

	wizOut, _, err := Run(context.Background(), k.Wizard(),
		channelkinds.WizardInput{
			K8s:            b.Controller,
			Namespace:      "default",
			Seeded:         seeded.Values,
			NonInteractive: true,
		},
		"github", st, "", seeded,
		tui.Options{Theme: tui.NewTheme(tui.Caps{}), NonInteractive: true, Out: &bytes.Buffer{}})
	require.NoError(t, err, "a fully-seeded unattended run must complete")

	require.NotNil(t, wizOut.ChannelManifest)
	assert.Equal(t, "demo-reviewbot-gh", wizOut.ChannelManifest.Name)
	assert.Equal(t, "demo-agent", wizOut.ChannelManifest.Spec.AgentClass)
	require.NotNil(t, wizOut.ChannelManifest.Spec.GitHub)
	assert.Equal(t, "demo-app", wizOut.ChannelManifest.Spec.GitHub.AppSlug)
	require.NotNil(t, wizOut.SecretManifest)
	assert.Equal(t, []byte("123456"), wizOut.SecretManifest.Data["app-id"])
	assert.Equal(t, []byte("7654321"), wizOut.SecretManifest.Data["installation-id"])
	require.NotEmpty(t, wizOut.Summary,
		"the data path records the run's decisions as data; only Result can produce them")
}

// TestRunChannelWizard_AnswerForAHandoffFallbackQuestionIsAccepted: the
// fallback's questions are questions this kind asks — on the route a headless
// host takes — and `--answer app-id=…` is how an unattended run supplies what
// the browser step would have produced. A declared set built from Inputs alone
// would refuse the flag that makes those runs possible.
func TestRunChannelWizard_AnswerForAHandoffFallbackQuestionIsAccepted(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	browsertest.Use(t, fakeBrowser(t, svc.URL, nil))

	w := &stubKindWizard{
		inputs:  []oap.Question{{Name: "org", Type: oap.QString, Prompt: "Which organization?"}},
		handoff: demoHandoff(t, svc.URL),
	}

	st, seeded, err := Seed("", []string{"org=demo-org", "installation-id=67890"})
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions(""))
	require.NoError(t, err, "a flag naming a fallback question must not be refused")
	assert.Equal(t, "67890", w.gotAnswers["installation-id"])
}

// TestRunChannelWizard_AHandoffKeyNothingAsksIsStillRefused is the other half:
// widening the declared set to the fallback's questions must not turn
// checkAnswerKeys into a no-op.
func TestRunChannelWizard_AHandoffKeyNothingAsksIsStillRefused(t *testing.T) {
	svc := fakeService(t, "abc")
	t.Cleanup(svc.Close)

	w := &stubKindWizard{
		inputs:  []oap.Question{{Name: "org", Type: oap.QString, Prompt: "Which organization?"}},
		handoff: demoHandoff(t, svc.URL),
	}

	st, seeded, err := Seed("", []string{"instalation-id=67890"})
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions(""))
	require.Error(t, err, "a typo'd key must fail at the flag")
	assert.Contains(t, err.Error(), "instalation-id")
	assert.Contains(t, err.Error(), "installation-id", "and the refusal lists what the kind does ask")
	assert.False(t, w.resulted, "nothing is built from a run refused at its flags")
}

// TestAnswersFrom_ReadsEachAnswerBackInTheShapeItsWidgetRecordedIt: the
// answers Result reads are recovered from the run's State, and the State
// holds a multi-select's answer as a LIST while every other widget holds a
// string. Reading them all with State.Get would silently hand Result an empty
// string for the one question type that does not store one.
//
// The answers are produced by driving the real widgets renderQuestions built,
// not by writing State directly, so the mapping is checked against what the
// widgets actually do rather than against a second description of it.
func TestAnswersFrom_ReadsEachAnswerBackInTheShapeItsWidgetRecordedIt(t *testing.T) {
	qs := []oap.Question{
		{Name: "org", Type: oap.QString, Prompt: "Which organization?"},
		{Name: "addons", Type: oap.QResourceList, Prompt: "Addons", Enum: []string{"metrics", "tracing", "audit"}},
	}

	st := tui.NewState()
	screens, err := renderQuestions(qs, nil, st, tui.Caps{})
	require.NoError(t, err)
	require.Len(t, screens, 2)

	th := tui.NewTheme(tui.Caps{})
	// "demo-org" answers the text field; "2\n3\n0" selects the second and
	// third options of the multi-select and confirms.
	answered, err := tui.RunWith(context.Background(), screens,
		tui.Options{Theme: th, Driver: tui.Plain(strings.NewReader("demo-org\n2\n3\n0\n"), io.Discard, th)}, st)
	require.NoError(t, err, "RunWith")

	got := answersFrom(qs, answered)
	assert.Equal(t, "demo-org", got["org"], "a text answer is read back as itself")
	assert.Equal(t, "tracing,audit", got["addons"],
		"a multi-select records a list; it must be rejoined the way --answer supplies one, not read as an empty string")

	// A question nothing answered is ABSENT rather than present-and-empty, so
	// a kind can still tell "answered with nothing" from "never asked".
	withUnasked := append(qs, oap.Question{Name: "unasked", Type: oap.QString, Prompt: "Never rendered"})
	assert.NotContains(t, answersFrom(withUnasked, answered), "unasked")
}

// TestRunChannelWizard_ResolveDerivesAnswersResultThenReads is the wiring
// gate on channelkinds.Wizard.Resolve: the step must run BETWEEN the
// answers and Result, be handed what the run collected, and have what it
// derives merged into the map Result reads.
//
// The derived key is deliberately one no question declares ("botuserid" is
// slack's shape: a value only the external service can supply), so a
// dispatcher that called Resolve and dropped its return would leave Result
// with an answer map missing it — which asserting on Result's map, rather
// than on Resolve having been called, is what catches.
func TestRunChannelWizard_ResolveDerivesAnswersResultThenReads(t *testing.T) {
	w := &stubKindWizard{
		inputs:   []oap.Question{{Name: "org", Type: oap.QString, Prompt: "Which organization?"}},
		resolved: map[string]string{"botuserid": "U0DEMO123"},
	}

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions("demo-org\n"))
	require.NoError(t, err, "the run must complete")

	assert.Equal(t, map[string]string{"org": "demo-org"}, w.gotResolveAnswers,
		"Resolve must see what the run collected, before its own derivation is merged in")
	require.True(t, w.resulted, "the run must have reached Result")
	assert.Equal(t, map[string]string{"org": "demo-org", "botuserid": "U0DEMO123"}, w.gotAnswers,
		"Result must read the collected answers WITH the derived ones merged over them")
}

// TestRunChannelWizard_TypedNameCollisionIsRefusedBeforeResolve is P5-R21.
//
// Resolve is where a kind does the irreversible half of its work — slack's
// provisioning route creates and installs a real Slack app there, and Slack has
// no API that lists a user's apps afterwards. With the collision checked only
// in front of the apply, an operator who TYPED a name that is already taken
// lost the run AND orphaned an app nothing could find again.
//
// A TYPED name is what discriminates. The pre-wizard check no-ops on an empty
// name, so it only ever sees a `--name` flag; this answer does not exist until
// the screens have run, which is why the check has to sit between them and
// Resolve.
//
// The double asserted here is deliberately generic rather than slack: the fix
// is one line in the driver and applies to every kind, and
// gotResolveAnswers staying nil is the direct proof that the irreversible step
// never began.
func TestRunChannelWizard_TypedNameCollisionIsRefusedBeforeResolve(t *testing.T) {
	const taken = "demo-channel"
	b := aptest.NewBundle(t,
		&spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: taken, Namespace: "default"}},
	)

	w := &stubKindWizard{inputs: []oap.Question{
		{Name: wizardkeys.KeyChannelName, Type: oap.QString, Prompt: "Channel resource name"},
	}}

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{K8s: b.Controller, Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions(taken+"\n"))
	require.Error(t, err, "a typed name that is already taken must be refused")
	assert.Contains(t, err.Error(), "already exists")
	assert.Contains(t, err.Error(), taken, "the refusal must name the Channel")

	assert.Nil(t, w.gotResolveAnswers,
		"Resolve must not have run: it is where a kind creates things it cannot take back")
	assert.False(t, w.resulted, "and no manifests may be built for a refused run")
}

// TestRunChannelWizard_MonitoringReSetupIsNotRefusedForItsOwnName is the
// control on that check: the monitoring flow deliberately reconfigures the
// Channel it matched, so its name being taken is the point rather than the
// problem. The pre-wizard check skips monitoring for the same reason, and this
// one must skip it identically or `oap init`'s second run cannot work at all.
func TestRunChannelWizard_MonitoringReSetupIsNotRefusedForItsOwnName(t *testing.T) {
	const existing = "slack-monitoring-demo"
	b := aptest.NewBundle(t,
		&spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: existing, Namespace: "default"}},
	)

	w := &stubKindWizard{inputs: []oap.Question{
		{Name: wizardkeys.KeyChannelName, Type: oap.QString, Prompt: "Channel resource name"},
	}}

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	_, _, err = Run(context.Background(), w,
		channelkinds.WizardInput{K8s: b.Controller, Namespace: "default", Monitoring: true},
		"demo-kind", st, "", seeded, scriptedOptions(existing+"\n"))
	require.NoError(t, err, "re-setup names the Channel it is reconfiguring; that is not a collision")
	assert.True(t, w.resulted, "and the run must reach Result")
}

// TestRunChannelWizard_ResolveFailureStopsTheRunVerbatim: a rejected
// credential is a user-facing failure, and the kind's own sentence is the one
// thing that says what to fix — so the dispatcher must surface it unchanged
// and must NOT go on to build manifests from answers the derivation could not
// complete.
func TestRunChannelWizard_ResolveFailureStopsTheRunVerbatim(t *testing.T) {
	const reason = "Slack rejected this bot token (auth.test): invalid_auth"
	w := &stubKindWizard{
		inputs:     []oap.Question{{Name: "org", Type: oap.QString, Prompt: "Which organization?"}},
		resolveErr: errors.New(reason),
	}

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	out, _, err := Run(context.Background(), w,
		channelkinds.WizardInput{Namespace: "default"}, "demo-kind", st, "", seeded,
		scriptedOptions("demo-org\n"))
	require.Error(t, err)
	assert.Equal(t, reason, err.Error(),
		"the kind's own message must reach the operator verbatim, not wrapped in a dispatcher phrase")
	assert.False(t, w.resulted, "Result must not run on answers the derivation could not complete")
	assert.Nil(t, out.ChannelManifest, "a stopped run produces no manifests")
}

// TestRunChannelWizard_AKindThatAsksNothingStillRuns: the fake kind declares
// no questions at all, which channelkinds.Wizard.Inputs allows, so a
// dispatcher with a "returned nothing to ask" guard would refuse a run that is
// in fact complete.
//
// Both halves of the summary are asserted. A kind runs no code of its own
// while the questions are being answered, so State must hold nothing and the
// line must arrive on WizardOutput.Summary instead — checking only the second
// would pass for a run that also recorded it twice.
func TestRunChannelWizard_AKindThatAsksNothingStillRuns(t *testing.T) {
	k, ok := registry.Get("fake")
	require.True(t, ok, "the fake kind must be registered in the oap binary")

	st, seeded, err := Seed("", nil)
	require.NoError(t, err, "Seed")

	wizOut, answered, err := Run(context.Background(), k.Wizard(),
		channelkinds.WizardInput{Namespace: "default"}, "fake", st, "", seeded,
		tui.Options{Theme: tui.NewTheme(tui.Caps{}), Out: &bytes.Buffer{}})
	require.NoError(t, err, "the fake kind's run asks nothing and must complete")

	assert.Empty(t, answered.Notes(),
		"a kind runs no code of its own while the questions are answered, so nothing may reach State")
	assert.Equal(t, []channelkinds.SummaryNote{{Label: "Channel", Value: "fake-channel (test fixture)"}}, wizOut.Summary,
		"the same summary line must arrive as data instead")
	require.NotNil(t, wizOut.ChannelManifest)
	assert.Equal(t, "default", wizOut.ChannelManifest.Namespace,
		"Result builds from the WizardInput it was handed, not from state Inputs left behind")
}

// TestRunChannelWizard_UnavailableKindStillRefusesWithItsReason: the kinds
// whose Channels are built by another process (local, browser) construct
// channelkinds.UnavailableWizard, whose every method refuses. The refusal an
// operator sees must be the kind's own reason — not a nil dereference, and
// not a run that asks nothing and applies empty manifests.
func TestRunChannelWizard_UnavailableKindStillRefusesWithItsReason(t *testing.T) {
	for _, kindName := range []string{"local", "browser"} {
		t.Run(kindName, func(t *testing.T) {
			k, ok := registry.Get(kindName)
			require.Truef(t, ok, "the %s kind must be registered in the oap binary", kindName)

			st, seeded, err := Seed("", nil)
			require.NoError(t, err, "Seed")

			wizOut, _, err := Run(context.Background(), k.Wizard(),
				channelkinds.WizardInput{Namespace: "default"}, kindName, st, "", seeded,
				tui.Options{Theme: tui.NewTheme(tui.Caps{}), Out: &bytes.Buffer{}})
			require.Error(t, err, "a kind with no wizard must refuse the run")
			assert.Contains(t, err.Error(), kindName, "the refusal names the kind it is about")
			assert.Nil(t, wizOut.ChannelManifest, "a refused run must produce no manifests")
		})
	}
}

// TestChannelCreateRefusesAStepNoFlagCanDo: a run that chose to create the
// Slack app by hand is refused with the manifest and the steps around it,
// rather than with a missing-answer error naming a flag that cannot help.
//
// The run is seeded with EVERY answer the flow declares, so nothing here is
// missing — the only thing wrong with it is that creating a Slack app happens
// in a browser.
//
// WHICH HALF REFUSES CHANGED WITH THE MIGRATION, and the claim being pinned is
// deliberately the one that did not. It used to be the interactionRequirer
// contract: manifestScreen declared the reason and this dispatcher asked for
// it before any screen ran, but only under --non-interactive. slack now
// answers channelkinds.Wizard, which has no equivalent of
// tui.Screen.RequiresInteraction, so the refusal comes from the kind's own
// Resolve — see slack.manualRouteRefusal, which explains why that route cannot
// be held mid-run at all — and it applies to every run, not only an unattended
// one. What must stay true either way is that the refusal says where the work
// happens and is not dressed up as a missing answer.
//
// K8s is nil deliberately: the refusal must land without a cluster, so a run
// that reached one would itself be the failure. Seeded carries the same State
// the run is driven against, which is the only configuration cmd/oap produces
// (runChannelCreate passes seeded.Values as both) and the only one in which
// Inputs can see what the flags answered.
//
// It drives the REGISTERED kind, whose wizard holds a live slack-go client, so
// this test doubles as the guarantee that nothing on this route reaches the
// network: the run must stop with the manifest before any auth.test. The two
// tokens are deliberately NOT seeded — on this route they are not questions
// the wizard asks, and supplying them anyway is the contradiction
// slack.agentInputs refuses rather than resolves.
func TestChannelCreateRefusesAStepNoFlagCanDo(t *testing.T) {
	// The refusal WRITES the app manifest beside the run — that copy is the
	// whole product of this route, since the message itself is scrollback the
	// next command scrolls away. Run from a temp directory so the file lands
	// there rather than in the package directory.
	t.Chdir(t.TempDir())

	kind, ok := registry.Get("slack")
	require.True(t, ok, "the slack kind must be registered in the oap binary")

	st := tui.NewState()
	seeded := Seeded{Values: map[string]string{}}
	for _, kv := range [][2]string{
		{"agentclass", "demo-agent"},
		{"slackapp", "false"},
		{"capabilities", "attachments"},
	} {
		seedAnswer(st, kv[0], kv[1])
		seeded.Values[kv[0]] = kv[1]
		seeded.Keys = append(seeded.Keys, kv[0])
	}

	_, _, err := Run(
		context.Background(),
		kind.Wizard(),
		channelkinds.WizardInput{Namespace: "default", Seeded: seeded.Values},
		"slack",
		st,
		"",
		seeded,
		tui.Options{Theme: tui.NewTheme(tui.Caps{}), Out: &bytes.Buffer{}, NonInteractive: true},
	)
	require.Error(t, err, "a run that cannot possibly complete must be refused")

	assert.Contains(t, err.Error(), "api.slack.com", "the refusal must say where the work happens")
	assert.Contains(t, err.Error(), "display_information",
		"and must carry the app manifest itself, which is the whole reason this route exists")
	assert.NotContains(t, err.Error(), "has no answer",
		"nothing was unanswered; calling it a missing answer sends the user hunting for a flag")
	assert.NotContains(t, err.Error(), "supply it via flags", "there is no such flag")
	assert.NotErrorIs(t, err, tui.ErrUnanswered,
		"this is an impossible run, not an under-specified one")
}
