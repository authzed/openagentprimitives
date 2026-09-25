// Package plansteps registers the `plan-steps-complete` completion
// requirement: a session may not declare its work complete while steps of its
// own declared plan are still pending or in progress.
//
// The agent writes the plan, so this is not the runtime second-guessing what
// the work was — it is holding the agent to its own account of it. The failure
// it answers is an agent calling agent_work_complete with a summary that says
// it is about to do more: the plan widget kept spinning on steps that genuinely
// never ran, and nothing compared the two.
package plansteps

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
)

// Key is the value an AgentClass declares in spec.completionRequirements.
const Key = "plan-steps-complete"

func init() { completion.Register(Requirement{}) }

// Requirement is the registered plan-steps-complete kind.
type Requirement struct{}

func (Requirement) Key() string { return Key }

// Title is user copy — it appears on the bypass notice a person reads.
func (Requirement) Title() string { return "every step the agent planned is finished" }

// Check reports any plan item still in a non-terminal status.
//
// Terminal covers done, error and stopped: an item the agent recorded as failed
// IS accounted for, and refusing completion on it would trap a session whose
// only honest report is that a step did not work. What must not pass is a step
// nobody ever resolved either way.
//
// A session with no plans at all is satisfied — declaring no plan is the
// ordinary single-step shape, not an empty promise.
func (Requirement) Check(_ context.Context, in completion.Input) (completion.Finding, error) {
	// A delegated child carries no plan of its OWN: planning tools are withheld
	// from it (a child requests authority, it does not author a plan), so it can
	// never have a step to leave open. plan-steps-complete is therefore
	// satisfied BY DESIGN for a child, stated here explicitly rather than
	// reached by the accident of an empty plan store below — which also makes it
	// robust to a child that has no plans store at all, a case the fail-closed
	// branch below would otherwise turn into a wedge on a requirement the child
	// structurally cannot satisfy (the completion gate runs before return_result
	// submits, and a child cannot update_plan its way to met).
	if in.Session != nil && in.Session.IsDelegatedChild {
		return completion.Finding{Met: true}, nil
	}

	store, ok := plans.TryFrom(in.Session)
	if !ok {
		// Fail-closed: a class that declared this requirement is asking about
		// plan state, and a session that carries none cannot answer. Saying
		// "met" would make the requirement quietly inert.
		return completion.Finding{}, errors.New("this session carries no plan state")
	}

	var open []string
	for _, p := range store.All() {
		for _, it := range p.Items {
			if it.Status.IsTerminal() {
				continue
			}
			open = append(open, fmt.Sprintf("%s/%s %q is %s", p.Name, it.ID, it.Label, it.Status))
		}
	}
	if len(open) == 0 {
		return completion.Finding{Met: true}, nil
	}
	sort.Strings(open) // stable message across map iteration order

	return completion.Finding{Missing: fmt.Sprintf(
		"%d step(s) of your own plan are still open: %s. Either finish them, or call update_plan "+
			"to set each one's status to \"done\" or \"error\" so the record matches what actually "+
			"happened — a step left pending is shown to the user as still running.",
		len(open), strings.Join(open, "; ")),
	}, nil
}
