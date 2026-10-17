package browser

import "context"

// HumanStepRequester is request_human_step's seam to the runner's
// human-in-the-loop machinery, mirroring meta.PreferenceSaver exactly: the
// interface lives in this tool package (the consumer), the runner implements it
// over the approval host (pkg/agent/runner's humanStepRequester). A fake
// satisfies it in tests with no Loop/orchestrator.
//
// request_human_step never returns a secret TO THE MODEL. Confirm reports only
// WHETHER the human completed the step; Captcha reports only that a notice was
// sent. RequestCode returns the human's typed code so the TOOL (the runner) can
// type it into the page element the agent named — it is never handed to the model.
type HumanStepRequester interface {
	// Confirm posts a login_human_step card with a Done button and blocks until a
	// person presses it, declines, or the approval window lapses — a push approval
	// the person completes on their phone, say. The outcome tells the tool which
	// happened; it carries no secret.
	Confirm(ctx context.Context, prompt string) (HumanStepOutcome, error)

	// RequestCode posts a login_human_step card asking the person to type an
	// emailed/SMS code, and blocks until they answer, decline, or the window lapses.
	// On an approval the outcome carries the person's typed Code, which the CALLER
	// (request_human_step) types into the page via the CDP fill path — the same
	// InsertText the type tool uses, but sourced from the human's code, not a vault
	// credential. The code is NEVER returned as a tool result or shown to the model.
	// A timeout is fail-closed: TimedOut is true, Code empty, and the caller reports
	// a failure rather than silently succeeding.
	RequestCode(ctx context.Context, prompt string) (HumanStepCodeOutcome, error)

	// Captcha surfaces a one-way notice that a person must take over in the browser
	// (a CAPTCHA the agent cannot solve, or any step with no actionable kind). It
	// does NOT block — there is no decision leg; the agent has reported it cannot
	// proceed, and a human steps in out of band.
	Captcha(ctx context.Context, prompt string) error
}

// HumanStepCodeOutcome is the result of a RequestCode round-trip. Exactly one of
// Approved / Denied / TimedOut is true whenever RequestCode returns a nil error.
type HumanStepCodeOutcome struct {
	// Approved is true when the person supplied the code (and Code holds it).
	Approved bool
	// Denied is true when the person explicitly declined to provide a code.
	Denied bool
	// TimedOut is true when the prompt lapsed with no answer — fail-closed, never a
	// silent success; distinguished from Denied so the model learns which happened.
	TimedOut bool
	// DecidedBy is the canonical subject id of whoever answered (set for Approved
	// and Denied), empty on a timeout.
	DecidedBy string
	// Code is the person's typed login code, set ONLY on Approved. It is a SECRET:
	// the caller types it into the page and returns a status; it is never placed in
	// a tool result, a log, the transcript, or any model-visible message.
	Code string
}

// HumanStepOutcome is the result of a Confirm round-trip. Exactly one of
// Approved / Denied / TimedOut is true whenever Confirm returns a nil error, so
// the tool can tell the model which happened rather than reporting every
// non-approval as a human "no".
type HumanStepOutcome struct {
	// Approved is true when the person confirmed the step (pressed Done).
	Approved bool
	// Denied is true when the person explicitly declined.
	Denied bool
	// TimedOut is true when the confirm prompt lapsed with no decision — a
	// fail-closed result, never a silent success. Distinguished from Denied so the
	// model learns whether a person said no or nobody answered.
	TimedOut bool
	// DecidedBy is the canonical subject id of whoever decided (set for Approved
	// and Denied), or empty on a timeout — nobody decided.
	DecidedBy string
}
