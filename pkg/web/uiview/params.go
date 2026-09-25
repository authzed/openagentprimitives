// The agent's half of the page's own controls.
//
// A binding parameter is normally the VIEWER's: they pick a date range, the
// bindings re-resolve, and nothing consults the agent. That stays the default.
// But a session can begin with the viewer already having said what they want —
// "companies from the last seven days", typed in a channel long before any
// browser is open — and a dashboard that meets them at its author's defaults
// has made them say it twice, in a different vocabulary, by hand.
//
// So the agent may state parameter values too. Two rules keep that from
// becoming the agent driving the page out from under the person reading it:
//
//   - The agent may only name keys the CURRENT declaration's own controls
//     drive. It cannot invent a parameter, and it cannot keep one alive across
//     a bundle redeploy that removed the control.
//   - The viewer's own explicit choice always wins. The browser applies these
//     to controls the viewer has not touched; see AgentUIView, which holds
//     that precedence, because only the browser knows what was touched.
package uiview

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewparams"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

// UnknownParamError reports that the agent named a parameter this declaration
// has no control for.
//
// Typed, and carrying the legal set, because the agent has to correct it while
// answering from a session-start description a bundle redeploy can have
// invalidated mid-session. "window.from is not a parameter here" sends it
// guessing; "…the parameters are span, minScore" it can act on in one turn.
type UnknownParamError struct {
	Key   string
	Known []string
}

func (e *UnknownParamError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("this view declares no binding parameters, so %q cannot be set", e.Key)
	}
	return fmt.Sprintf("%q is not a parameter of this view; it declares: %v", e.Key, e.Known)
}

// WriteParams replaces the agent's parameter choices for this UI.
//
// Whole-map replacement, never a merge: the agent states the set of controls it
// is driving, and a key it stops naming is one it has stopped driving. A merge
// would make an agent-set filter impossible to retract, leaving the page
// carrying a value nobody could explain the origin of.
//
// Validation is against the LIVE declaration, re-read here for the same reason
// Write re-reads it: a bundle redeploy changes the control set under a running
// session, and admitting a key for a control that no longer exists would store
// a value nothing can ever apply or clear.
func (r *Runtime) WriteParams(ctx context.Context, params map[string]string) error {
	if r.Mem == nil {
		return fmt.Errorf("uiview: WriteParams: no memory backend for %s/%s", r.Namespace, r.Session)
	}

	ui, err := r.Base(ctx)
	if err != nil {
		return err
	}
	base, err := DeclarationFromSpec(ui)
	if err != nil {
		return fmt.Errorf("uiview: WriteParams: convert %s: %w", ui.Name, err)
	}

	// ParamKeys, not ParamNames: an ap:daterange declaring "window" drives
	// "window.from" and "window.to" and never "window" itself, and the keys are
	// what a binding's {"$param": …} is actually satisfied by.
	known := uicomponents.ParamKeys(base)
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if !slices.Contains(known, k) {
			return &UnknownParamError{Key: k, Known: known}
		}
	}

	// Copied rather than stored by reference: the caller's map is the decoded
	// tool arguments, and a record that aliased it would change if anything
	// downstream reused the buffer.
	stored := make(map[string]string, len(params))
	maps.Copy(stored, params)

	if err := uiviewparams.Record(ctx, r.Mem, r.scope(), uiviewparams.Content{
		UI: r.UIName, Params: stored, WrittenAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	// Published AFTER the durable write: a push that preceded it would let a
	// browser show a state no reconnect could reproduce if the write failed.
	//
	// It reuses the ui_view_update envelope rather than minting its own kind.
	// That envelope is already a TRIGGER, not a document — webd re-resolves
	// and pushes what IT read — and a second kind would mean a second
	// subscription, a second grant, and two ways to learn one thing.
	r.publishParams(ctx)
	return nil
}

// ReadParams returns the agent's stored parameter choices, or an empty map.
func (r *Runtime) ReadParams(ctx context.Context) (map[string]string, error) {
	if r.Mem == nil {
		return nil, fmt.Errorf("uiview: ReadParams: no memory backend for %s/%s", r.Namespace, r.Session)
	}
	return uiviewparams.Get(ctx, r.Mem, r.scope(), r.UIName)
}

// publishParams sends the trigger, logging (never failing the write) on every
// non-happy path — see Publish's doc comment.
//
// Hook is empty: no hook changed. The browser's view handler treats Hook as
// diagnostics rather than a patch key, so an empty one is honest.
func (r *Runtime) publishParams(ctx context.Context) {
	if r.Publish == nil {
		slog.Default().Info("uiview: Publish not attached; ui_view_update push skipped after a parameter write (the write itself already succeeded)",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName)
		return
	}
	env, err := channelevents.BuildEnvelope(r.Namespace, r.Session, channelevents.KindUIViewUpdate,
		channelevents.UIViewUpdatePayload{UpdatedAt: time.Now().UTC()})
	if err != nil {
		slog.Default().Info("uiview: build ui_view_update envelope failed after a parameter write",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "err", err.Error())
		return
	}
	if err := r.Publish(ctx, env); err != nil {
		slog.Default().Info("uiview: ui_view_update push failed after a parameter write",
			"namespace", r.Namespace, "session", r.Session, "ui", r.UIName, "err", err.Error())
	}
}
