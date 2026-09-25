package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// CompletionConfig carries the per-session completion gate.
//
// The zero value is the ungated tool: no requirements declared, no
// `bypass_reason` in the schema, and Execute behaves exactly as it did before
// the gate existed.
type CompletionConfig struct {
	// Requirements are the completion-requirement keys this session's
	// AgentClass declared (spec.completionRequirements). Each is resolved
	// through the completion registry at call time; an empty list leaves the
	// gate inert, which is what keeps a registered requirement from tightening
	// a class that never opted into it.
	Requirements []string

	// RecordBypass records an agent's decision to finish with a requirement
	// unmet, so a human learns of it. REQUIRED whenever Requirements is
	// non-empty: the gate refuses a bypass it cannot record, because a bypass
	// nobody can see is just an off switch.
	RecordBypass func(ctx context.Context, b completion.Bypass) error
}

// completionGate is the completion-requirement check itself, embedded by BOTH
// terminal tools — agent_work_complete and return_result.
//
// Shared rather than copied. A completion requirement is the operator's
// guarantee about what a session of that class produces, and reporting to an
// agent rather than to a human does not void it: a delegated child finishing
// through return_result is still a session of a class that made a promise.
// Two copies of a gate drift, and the copy that drifts is the one on the
// unattended path nobody watches.
type completionGate struct{ cfg CompletionConfig }

// gated reports whether this session declared anything to check.
func (g completionGate) gated() bool { return len(g.cfg.Requirements) > 0 }

// descriptionAddendum is what a gated tool appends to its own description, so
// the model learns about the refusal and its escape before it hits them.
func (g completionGate) descriptionAddendum() string {
	if !g.gated() {
		return ""
	}
	return " This agent has completion requirements: the call is REFUSED while any of them is unmet, and the refusal names what is missing. If you genuinely mean to finish anyway, repeat the call with `bypass_reason` — the user is told you did and why."
}

// addSchemaProperties adds `bypass_reason` to a gated tool's input schema.
//
// It appears ONLY for a session that has something to bypass. A field the
// model can see is a field it will eventually try, and offering an escape from
// a gate this class never declared would be an invitation to nothing — the
// honest-schema rule respond_to_user's `attached` follows.
func (g completionGate) addSchemaProperties(props map[string]any) {
	if !g.gated() {
		return
	}
	props["bypass_reason"] = map[string]any{
		"type":      "string",
		"minLength": 1,
		"description": "Only for finishing while a completion requirement is unmet. State WHY in the user's terms — " +
			"this is shown to them, alongside what was left undone. Omit it unless you are deliberately overriding a refusal.",
	}
}

// check runs the declared gate for the tool named toolName. ok=false means the
// caller must return the accompanying Result instead of completing.
//
// bypassReason is the `bypass_reason` argument AS SENT: nil for absent,
// non-nil (possibly empty) for present. The two get different refusals — the
// first is an ordinary completion that has not been told about the gate yet,
// the second an attempted bypass with nothing said.
func (g completionGate) check(ctx context.Context, toolName string, bypassReason *string, sess *tool.SessionContext) (tool.Result, bool) {
	if !g.gated() {
		return tool.Result{}, true
	}

	unmet, err := completion.Evaluate(ctx, g.cfg.Requirements, completion.Input{Session: sess})
	if err != nil {
		// A requirement that cannot be evaluated is not a requirement that is
		// satisfied. Fold the fault in as one more unmet entry rather than
		// returning early: it then refuses, records and reports through exactly
		// the same path — bypass included — so a misconfigured class degrades
		// this round instead of wedging the session on something the agent has
		// no way to fix.
		unmet = append(unmet, completion.CheckFailedUnmet(err))
	}
	if len(unmet) == 0 {
		// Nothing to bypass. A reason sent anyway is not an error — the model
		// may simply be repeating its previous call after fixing the gap — but
		// it must not be recorded, or the user gets a notice about an override
		// that never happened.
		return tool.Result{}, true
	}

	if bypassReason == nil {
		return tool.Result{Content: refusalMessage(toolName, unmet), IsError: true, Trusted: true}, false
	}
	reason := strings.TrimSpace(*bypassReason)
	if reason == "" {
		return tool.Result{
			Content: toolName + ": `bypass_reason` was empty, so nothing was overridden. The reason is the " +
				"whole point of the bypass — it is shown to the user in place of the work that was skipped. " +
				"Either finish what is outstanding, or repeat the call with a real explanation.\n\n" +
				refusalMessage(toolName, unmet),
			IsError: true, Trusted: true,
		}, false
	}
	if g.cfg.RecordBypass == nil {
		return tool.Result{
			Content: toolName + ": this session cannot record a completion bypass, so one cannot be granted — " +
				"an override the user never learns about is not an override. Finish what is outstanding instead.\n\n" +
				refusalMessage(toolName, unmet),
			IsError: true, Trusted: true,
		}, false
	}
	if err := g.cfg.RecordBypass(ctx, completion.Bypass{Reason: reason, Unmet: unmet}); err != nil {
		// A bypass that was not recorded did not happen. Completing anyway
		// would leave the user with neither the work nor the notice.
		return tool.Result{
			Content: toolName + ": the bypass could not be recorded, so it did not take effect: " + err.Error() +
				". Retry, or finish what is outstanding.\n\n" + refusalMessage(toolName, unmet),
			IsError: true, Trusted: true,
		}, false
	}
	return tool.Result{}, true
}

// refusalMessage renders the unmet set for the model: what is missing, per
// requirement, and the standing offer of a bypass. toolName is the caller's own
// name so the retry instruction names the tool this session actually has — a
// delegated child is offered return_result IN PLACE OF agent_work_complete, and
// naming the absent one would hand it an escape it cannot reach.
func refusalMessage(toolName string, unmet []completion.Unmet) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		"%s: this round is not complete — %d requirement(s) this agent must satisfy are unmet, "+
			"so the session has NOT ended:\n", toolName, len(unmet)))
	b.WriteString(completion.UnmetList(unmet))
	b.WriteString("\nDo those, then call " + toolName + " again. If you are deliberately finishing without them " +
		"— a degraded result is still a result — repeat the call with a non-empty `bypass_reason` explaining why. " +
		"The reason and what was left undone are both shown to the user, so write it for them.")
	return b.String()
}

// marshalSchema renders a tool's input schema, falling back to a minimal valid
// schema over the required field rather than panicking: the schemas here are
// built from literals so marshalling cannot fail, and a marshalling bug must
// not take down every session's terminal tool.
func marshalSchema(schema map[string]any, requiredField string) json.RawMessage {
	out, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(fmt.Sprintf(
			`{"type":"object","properties":{%q:{"type":"string"}},"required":[%q]}`,
			requiredField, requiredField))
	}
	return out
}
