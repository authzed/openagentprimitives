package channelwizard

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/browser"
)

// PreparedRun is a channel wizard staged for graph execution. PrepareRun
// collects ordinary answers; ResolvePreparedRun adds handoff-derived
// fallbacks and seals the output before graph-owned mutation. Its fields stay
// private because answers and output manifests may contain credentials;
// diagnostic formatting and JSON expose counts only.
type PreparedRun struct {
	wizard   channelkinds.Wizard
	input    channelkinds.WizardInput
	handoff  *channelkinds.HandoffSpec
	answers  map[string]string
	state    *tui.State
	opts     tui.Options
	output   channelkinds.WizardOutput
	resolved bool
}

func (p PreparedRun) String() string {
	return fmt.Sprintf("PreparedRun{answers:%d, handoff:%t}", len(p.answers), p.handoff != nil)
}

func (p PreparedRun) GoString() string { return p.String() }

func (p PreparedRun) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Answered int  `json:"answered"`
		Handoff  bool `json:"handoff"`
	}{Answered: len(p.answers), Handoff: p.handoff != nil})
}

// SensitiveValues returns answer values for registration with the install
// redactor. Callers must not render or serialize them.
func (p *PreparedRun) SensitiveValues() []string {
	if p == nil {
		return nil
	}
	values := make([]string, 0, len(p.answers))
	for _, value := range p.answers {
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

// PrepareRun collects the wizard's ordinary inputs, but performs no provider
// handoff and no cluster mutation. Handoff fallbacks are intentionally
// deferred until ResolvePreparedRun: some of them depend on results that do
// not exist until after the handoff has run.
func PrepareRun(
	ctx context.Context,
	w channelkinds.Wizard,
	wizIn channelkinds.WizardInput,
	kindName string,
	st *tui.State,
	role string,
	seeded Seeded,
	deferred map[string]bool,
	opts tui.Options,
) (*PreparedRun, *tui.State, error) {
	if w == nil {
		return nil, st, fmt.Errorf("channel wizard: no wizard to prepare")
	}
	if st == nil {
		return nil, st, fmt.Errorf("channel wizard: state must not be nil")
	}
	wizIn.Role = role
	inputs, err := w.Inputs(ctx, wizIn)
	if err != nil {
		return nil, st, err
	}
	handoff, err := w.Handoff(ctx, wizIn)
	if err != nil {
		return nil, st, err
	}
	if err := channelkinds.ValidateInputs(inputs); err != nil {
		return nil, st, err
	}
	if handoff != nil {
		if err := wizardrun.ValidateHandoffSpec(handoff); err != nil {
			return nil, st, err
		}
	}
	allInputs := wizardrun.AllInputs(inputs, handoff)
	declared := inputAnswerKeys(allInputs)
	if err := checkAnswerKeys(kindName, seeded.Keys, declared); err != nil {
		return nil, st, err
	}
	answered, err := collectPreparedQuestions(ctx, inputs, seeded.Values, deferred, st, opts)
	if err != nil {
		return nil, answered, err
	}
	known := copyAnswers(seeded.Values)
	for key, value := range answersFrom(inputs, answered) {
		known[key] = value
	}
	return &PreparedRun{
		wizard: w, input: wizIn, handoff: handoff,
		answers: known, state: answered, opts: opts,
	}, answered, nil
}

func collectPreparedQuestions(
	ctx context.Context,
	questions []oap.Question,
	seeded map[string]string,
	deferred map[string]bool,
	st *tui.State,
	opts tui.Options,
) (*tui.State, error) {
	filtered := make([]oap.Question, 0, len(questions))
	for _, question := range questions {
		if !deferred[question.Name] {
			filtered = append(filtered, question)
		}
	}
	screens, err := renderQuestions(filtered, seeded, st, screenCaps(opts))
	if err != nil {
		return st, err
	}
	return driveChannelScreens(ctx, screens, st, declaredAnswerKeys(screens), opts)
}

// ResolvePreparedRun runs the provider handoff, collects only fallbacks still
// missing after that handoff, and seals the resulting manifests. It must run
// before graph-owned Kubernetes resources are mutated.
func ResolvePreparedRun(
	ctx context.Context,
	prepared *PreparedRun,
	runtimeAnswers map[string]string,
	executionK8s client.Client,
) (*tui.State, error) {
	if prepared == nil {
		return nil, fmt.Errorf("channel wizard: no prepared run to resolve")
	}
	if prepared.resolved {
		return prepared.state, nil
	}
	answers := copyAnswers(prepared.answers)
	for key, value := range runtimeAnswers {
		answers[key] = value
		seedAnswer(prepared.state, key, value)
	}
	wizIn := prepared.input
	if executionK8s != nil {
		wizIn.K8s = executionK8s
	}
	out, err := wizardrun.Finish(ctx, wizardrun.Params{
		Wizard: prepared.wizard, In: wizIn, Answers: answers, Handoff: prepared.handoff,
		DriveHandoff: func(ctx context.Context, spec *channelkinds.HandoffSpec, current map[string]string) (map[string]string, error) {
			return runHandoff(ctx, handoffRun{
				spec: spec, answers: current, seeded: answers, state: prepared.state,
				opts: prepared.opts, openBrowser: browser.Open,
			})
		},
	})
	if prepared.handoff != nil {
		for key, value := range answersFrom(prepared.handoff.FallbackInputs, prepared.state) {
			answers[key] = value
		}
	}
	// Keep every answer, including values returned by the handoff or collected
	// as a fallback, available to the install redactor even when resolution
	// fails after one of those values was obtained.
	prepared.answers = answers
	if err != nil {
		return prepared.state, err
	}
	prepared.output = out
	prepared.resolved = true
	return prepared.state, nil
}

// FinishPreparedRun returns a sealed wizard result. It performs no handoff,
// provider work, prompting, or Kubernetes reads/writes.
func FinishPreparedRun(prepared *PreparedRun) (channelkinds.WizardOutput, *tui.State, error) {
	if prepared == nil {
		return channelkinds.WizardOutput{}, nil, fmt.Errorf("channel wizard: no prepared run to finish")
	}
	if !prepared.resolved {
		return channelkinds.WizardOutput{}, prepared.state, fmt.Errorf("channel wizard: prepared run is not resolved")
	}
	return prepared.output, prepared.state, nil
}
