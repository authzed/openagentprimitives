package meta

import (
	"context"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
)

// PreferencesReader is a meta tool's read access to a session's resolved
// user-preferences snapshot — the class's declared schema, resolved through
// any admin globals and the current user's own saved values (see
// preferences.Resolve's precedence). Implemented by the runner
// (pkg/agent/runner's preferencesReader) over the operator's memory HTTP
// client; Task 13's preference tools depend on this interface, not the
// concrete client, so they stay unit-testable with a fake.
type PreferencesReader interface {
	// Current returns the resolved snapshot for this session's class and the
	// CURRENT turn's author — the runner scopes the read to the turn a
	// preference tool is called from, not to whoever started the session, so
	// a shared thread's other participants read their own preferences.
	Current(ctx context.Context) (preferences.SnapshotResponse, error)

	// ForRef returns the class-VISIBLE snapshot for whichever user ref
	// resolves to (see pkg/platform/identity/subjectresolve for the
	// supported forms) — never the CURRENT turn's author. Only preferences
	// the class marks visibility: class are ever returned this way; the
	// server-side resolution and disclosure filtering both live in
	// httpclient.Client.GetPreferencesForUserRef's handler
	// (pkg/memory/httpsrv/preferences.go), not here — this method is a thin
	// forward, same as Current.
	ForRef(ctx context.Context, ref string) (preferences.SnapshotResponse, error)
}

// SaveOutcome is the result of a PreferenceSaver.Save confirm round-trip.
// Exactly one of Approved / Denied / TimedOut is true whenever Save returns
// a nil error — Save never reports a decision without saying which one.
type SaveOutcome struct {
	// Approved is true when the addressee confirmed the save. The commit
	// itself already happened (channelsd's preference_save decision handler,
	// keyed on the verified decider) by the time Save returns — this field is
	// a report of what happened, not something the caller still has to do.
	Approved bool
	// Denied is true when the addressee explicitly declined. Nothing was
	// committed.
	Denied bool
	// TimedOut is true when the confirm prompt lapsed with no decision.
	// Nothing was committed — a lapsed deadline never approves — but it is
	// distinguished from Denied so the tool can tell the model which
	// happened, rather than reporting every non-approval as a human "no".
	TimedOut bool
	// DecidedBy is the canonical subject id of whoever decided (set for both
	// Approved and Denied), or empty on a timeout — nobody decided.
	DecidedBy string
}

// PreferenceSaver publishes a preference_save confirm addressed to the
// CURRENT TURN's own author and blocks for their decision. It is personal by
// design: in a shared thread, the person whose preference is being changed
// decides, not whoever owns or started the session (see
// categories.UserPreferenceConfirm's DecideRequester policy).
type PreferenceSaver interface {
	// Save publishes the confirm and blocks for the decision. It never writes
	// the preference itself — on approve, channelsd has already committed it
	// by the time Save returns; Save only reports the outcome. value is nil
	// to request CLEARING the stored preference (never a JSON null value); a
	// non-nil value is the JSON to save under key. display is the exact
	// approver-facing sentence the confirm card shows (e.g. `Save "language:
	// de" as your default for this agent?`).
	Save(ctx context.Context, key string, value *apiextv1.JSON, display string) (SaveOutcome, error)
}
